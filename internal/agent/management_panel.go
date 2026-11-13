package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ManagementPanel reports the local controller or the installed passive role.
// Only an elected xirangd opens the panel listener; Agents never open relays.
type ManagementPanel struct {
	mu              sync.Mutex
	stateDirectory  string
	state           State
	server          *http.Server
	listener        net.Listener
	external        bool
	localController bool
	status          string
	installed       bool
}

func NewManagementPanel(stateDirectory string, state State) *ManagementPanel {
	panel := &ManagementPanel{stateDirectory: stateDirectory, state: state, status: "disabled"}
	panel.refreshControllerLocked(enrolledControllerIsLocal, localPanelListening)
	return panel
}

func (panel *ManagementPanel) SetEnabled(enabled bool) (bool, string) {
	panel.mu.Lock()
	defer panel.mu.Unlock()
	panel.refreshControllerLocked(enrolledControllerIsLocal, localPanelListening)
	if enabled {
		panel.installed = true
		if panel.external {
			return true, ""
		}
		if panel.server != nil {
			return true, ""
		}
		// Management assignment installs/enables the role only. The elected
		// xirangd exclusively owns the panel listener; a passive Agent must
		// never open a relay that looks like another active controller.
		if err := controllerInstallationReady(); err != nil {
			panel.installed = false
			panel.status = "error: " + err.Error()
			return false, err.Error()
		}
		panel.status = "installed"
		return true, ""
	}
	if panel.external {
		return false, "port 8899 is served by the local control-plane service"
	}
	if panel.server == nil {
		// A listener not owned by this Agent is normally the primary xirangd.
		// Refuse to claim it was disabled and let the controller restore the
		// management role instead of silently leaving an unlabelled panel.
		connection, err := net.DialTimeout("tcp", "127.0.0.1:8899", 250*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			panel.status = "externally managed"
			return false, "port 8899 is served by an external control-plane service"
		}
		panel.status = "disabled"
		panel.installed = false
		return true, ""
	}
	server, listener := panel.server, panel.listener
	panel.server, panel.listener = nil, nil
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panel.status = "error: " + err.Error()
		return false, err.Error()
	}
	if listener != nil {
		_ = listener.Close()
	}
	panel.status = "disabled"
	panel.installed = false
	return true, ""
}

// A passive role is reported installed only when the standard node bundle
// actually contains both the controller executable and compiled panel assets.
func controllerInstallationReady() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	binary := filepath.Join(filepath.Dir(executable), "xirangd")
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("controller executable is not installed beside the Agent")
	}
	info, err = os.Stat("/usr/local/share/xirang/web/index.html")
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("control panel assets are not installed")
	}
	return nil
}

func (panel *ManagementPanel) State() (bool, string) {
	panel.mu.Lock()
	defer panel.mu.Unlock()
	panel.refreshControllerLocked(enrolledControllerIsLocal, localPanelListening)
	return panel.server != nil || panel.external, panel.status
}

// Network-online does not guarantee that an overlay address already exists.
// Re-evaluate locality on each heartbeat, including after a false startup result,
// and stop advertising a disappeared listener as an enabled controller.
func (panel *ManagementPanel) refreshControllerLocked(isLocal func(State) bool, listening func() bool) {
	if panel.server != nil {
		return
	}
	wasLocal := panel.localController
	panel.localController = isLocal(panel.state)
	if !panel.localController {
		panel.external = false
		if wasLocal {
			if panel.installed {
				panel.status = "installed"
			} else {
				panel.status = "disabled"
			}
		}
		return
	}
	panel.external = listening()
	if panel.external {
		panel.status = "controller"
	} else {
		if panel.installed {
			panel.status = "installed"
		} else {
			panel.status = "error: local control-plane service is unavailable"
		}
	}
}

func localPanelListening() bool {
	connection, err := net.DialTimeout("tcp", "127.0.0.1:8899", 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func enrolledControllerIsLocal(state State) bool {
	upstream, err := url.Parse(state.ServerURL)
	if err != nil || upstream.Hostname() == "" {
		return false
	}
	target := strings.TrimSuffix(strings.ToLower(upstream.Hostname()), ".")
	hostname, _ := os.Hostname()
	if strings.TrimSuffix(strings.ToLower(hostname), ".") == target {
		return true
	}
	targetIP := net.ParseIP(target)
	if targetIP == nil {
		return target == "localhost"
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if ip, _, parseErr := net.ParseCIDR(address.String()); parseErr == nil && ip.Equal(targetIP) {
			return true
		}
	}
	return targetIP.IsLoopback()
}

func (panel *ManagementPanel) Close() {
	panel.mu.Lock()
	server, listener := panel.server, panel.listener
	panel.server, panel.listener = nil, nil
	panel.status = "disabled"
	panel.mu.Unlock()
	if server != nil {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}
	if listener != nil {
		_ = listener.Close()
	}
}

// managementPanelUpstreamTLSConfig trusts the enrolled controller CA without
// presenting the Agent certificate. The panel is a user-facing relay: human
// requests must retain their own Bearer/OIDC authorization boundary and must
// not inherit node-only privileges such as authenticated artifact download.
func managementPanelUpstreamTLSConfig(stateDirectory string, state State, upstream *url.URL) (*tls.Config, error) {
	if upstream == nil || upstream.Scheme != "https" || upstream.Hostname() == "" || state.Identity == "" {
		return nil, errors.New("management-panel upstream identity is incomplete")
	}
	identityDirectory := filepath.Join(stateDirectory, "identities", state.Identity)
	pool, err := loadCAPool(filepath.Join(identityDirectory, "ca.crt"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: upstream.Hostname(),
	}, nil
}

func managementPanelCertificate(directory string) (tls.Certificate, error) {
	inventory, err := CollectInventory()
	if err != nil {
		return tls.Certificate{}, err
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "XIRANG Management Panel", Organization: []string{"XIRANG"}},
		NotBefore:    now.Add(-5 * time.Minute), NotAfter: now.Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if hostname := strings.TrimSpace(inventory.Hostname); hostname != "" {
		template.DNSNames = append(template.DNSNames, hostname)
	}
	for _, networkInterface := range inventory.NetworkInterfaces {
		for _, rawAddress := range networkInterface.Addresses {
			address := rawAddress
			if prefix, _, parseErr := net.ParseCIDR(rawAddress); parseErr == nil {
				address = prefix.String()
			}
			if ip := net.ParseIP(address); ip != nil && !ip.IsUnspecified() {
				template.IPAddresses = append(template.IPAddresses, ip)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := atomicWrite(filepath.Join(directory, "management-panel.crt"), certificatePEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := atomicWrite(filepath.Join(directory, "management-panel.key"), keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certificatePEM, keyPEM)
}
