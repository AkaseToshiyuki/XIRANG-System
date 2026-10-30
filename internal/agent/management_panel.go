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
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const managementPanelListenAddress = "0.0.0.0:8899"

// ManagementPanel relays the enrolled controller's Web/API surface from a
// node that has the management role. It deliberately has no configurable
// upstream: the Agent identity fixes the only controller it may contact.
type ManagementPanel struct {
	mu              sync.Mutex
	stateDirectory  string
	state           State
	server          *http.Server
	listener        net.Listener
	external        bool
	localController bool
	status          string
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
		if panel.external {
			return true, ""
		}
		if panel.server != nil {
			return true, ""
		}
		if panel.localController {
			// A relay to our own enrolled endpoint would proxy to itself once
			// it occupies the controller's port. Wait for xirangd to recover.
			return false, "local control-plane service is unavailable"
		}
		if err := panel.startLocked(); err != nil {
			panel.status = "error: " + err.Error()
			return false, err.Error()
		}
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
	return true, ""
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
			panel.status = "disabled"
		}
		return
	}
	panel.external = listening()
	if panel.external {
		panel.status = "controller"
	} else {
		panel.status = "error: local control-plane service is unavailable"
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

func (panel *ManagementPanel) startLocked() error {
	upstream, err := url.Parse(panel.state.ServerURL)
	if err != nil || upstream.Scheme != "https" || upstream.Hostname() == "" {
		return errors.New("enrolled controller URL is not a valid HTTPS endpoint")
	}
	tlsConfig, err := managementPanelUpstreamTLSConfig(panel.stateDirectory, panel.state, upstream)
	if err != nil {
		return fmt.Errorf("load enrolled controller trust: %w", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig,
		ForceAttemptHTTP2: true, MaxIdleConns: 64, IdleConnTimeout: 90 * time.Second,
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, cause error) {
		http.Error(writer, "XIRANG active controller is unavailable", http.StatusBadGateway)
	}
	certificate, err := managementPanelCertificate(panel.stateDirectory)
	if err != nil {
		return fmt.Errorf("prepare management-panel certificate: %w", err)
	}
	listener, err := net.Listen("tcp", managementPanelListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", managementPanelListenAddress, err)
	}
	server := &http.Server{
		Addr: managementPanelListenAddress, Handler: proxy,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}},
	}
	panel.server, panel.listener, panel.status = server, listener, "ready"
	go func() {
		err := server.Serve(tls.NewListener(listener, server.TLSConfig))
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			panel.mu.Lock()
			if panel.server == server {
				panel.server, panel.listener = nil, nil
				panel.status = "error: " + err.Error()
			}
			panel.mu.Unlock()
		}
	}()
	return nil
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
