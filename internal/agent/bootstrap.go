package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/pkg/version"
)

const BootstrapProtocol = "xirang-agent-bootstrap-v1"

type BootstrapStatus struct {
	Protocol        string                      `json:"protocol"`
	AgentVersion    string                      `json:"agentVersion"`
	Hostname        string                      `json:"hostname"`
	Architecture    string                      `json:"architecture"`
	Distribution    string                      `json:"distribution,omitempty"`
	KernelVersion   string                      `json:"kernelVersion,omitempty"`
	CPUCount        int                         `json:"cpuCount"`
	MemoryBytes     int64                       `json:"memoryBytes"`
	Interfaces      []NetworkInterfaceInventory `json:"interfaces"`
	Fingerprint     string                      `json:"fingerprint"`
	PairingRequired bool                        `json:"pairingRequired"`
}

type bootstrapEnrollmentRequest struct {
	ControllerURL    string `json:"controllerUrl"`
	Token            string `json:"token"`
	CACertificatePEM string `json:"caCertificatePem"`
	PairingCode      string `json:"pairingCode"`
}

type bootstrapEnrollmentResponse struct {
	NodeID string `json:"nodeId"`
}

func ServeBootstrapEnrollment(ctx context.Context, listenAddress, stateDirectory string) error {
	if listenAddress == "" {
		return errors.New("bootstrap listen address is required")
	}
	if _, err := LoadState(stateDirectory); err == nil {
		return errors.New("this Agent is already enrolled")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Agent state: %w", err)
	}
	inventory, err := CollectInventory()
	if err != nil {
		return fmt.Errorf("collect bootstrap inventory: %w", err)
	}
	certificate, fingerprint, err := bootstrapCertificate(stateDirectory, inventory.Hostname)
	if err != nil {
		return err
	}
	status := BootstrapStatus{
		Protocol: BootstrapProtocol, AgentVersion: version.Version,
		Hostname: inventory.Hostname, Architecture: inventory.Architecture,
		Distribution: inventory.Distribution, KernelVersion: inventory.KernelVersion,
		CPUCount: inventory.CPUCount, MemoryBytes: inventory.MemoryBytes,
		Interfaces:  append([]NetworkInterfaceInventory(nil), inventory.NetworkInterfaces...),
		Fingerprint: fingerprint, PairingRequired: true,
	}
	pairingCode, err := BootstrapPairingCode(stateDirectory)
	if err != nil {
		return err
	}
	var enrollMu sync.Mutex
	enrolled := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /xirang/v1/status", func(writer http.ResponseWriter, _ *http.Request) {
		writeBootstrapJSON(writer, http.StatusOK, status)
	})
	mux.HandleFunc("POST /xirang/v1/enroll", func(writer http.ResponseWriter, request *http.Request) {
		enrollMu.Lock()
		defer enrollMu.Unlock()
		if _, err := LoadState(stateDirectory); err == nil {
			http.Error(writer, "Agent is already enrolled", http.StatusConflict)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
		var input bootstrapEnrollmentRequest
		decoder := json.NewDecoder(request.Body)
		if err := decoder.Decode(&input); err != nil || input.ControllerURL == "" || input.Token == "" || input.CACertificatePEM == "" ||
			subtle.ConstantTimeCompare([]byte(normalizePairingCode(input.PairingCode)), []byte(pairingCode)) != 1 {
			http.Error(writer, "invalid enrollment request", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
			http.Error(writer, "cannot prepare Agent state", http.StatusInternalServerError)
			return
		}
		caPath := filepath.Join(stateDirectory, ".bootstrap-ca.crt")
		if err := atomicWrite(caPath, []byte(input.CACertificatePEM), 0o600); err != nil {
			http.Error(writer, "cannot store bootstrap CA", http.StatusInternalServerError)
			return
		}
		defer os.Remove(caPath)
		enrollContext, cancel := context.WithTimeout(request.Context(), 45*time.Second)
		defer cancel()
		state, err := Enroll(enrollContext, input.ControllerURL, input.Token, caPath, stateDirectory)
		if err != nil {
			http.Error(writer, "control-plane enrollment failed", http.StatusBadGateway)
			return
		}
		_ = os.Remove(filepath.Join(stateDirectory, bootstrapPairingCodeFile))
		writeBootstrapJSON(writer, http.StatusCreated, bootstrapEnrollmentResponse{NodeID: state.NodeID})
		select {
		case <-enrolled:
		default:
			close(enrolled)
		}
	})
	server := &http.Server{
		Addr: listenAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 50 * time.Second, WriteTimeout: 50 * time.Second, IdleTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}},
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen for bootstrap enrollment: %w", err)
	}
	tlsListener := tls.NewListener(listener, server.TLSConfig)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(tlsListener) }()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		return ctx.Err()
	case <-enrolled:
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		return nil
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

const bootstrapPairingCodeFile = ".bootstrap-pairing-code"

// BootstrapPairingCode returns the local one-time code that an administrator
// must enter when approving this fresh Agent from the controller console.
func BootstrapPairingCode(stateDirectory string) (string, error) {
	path := filepath.Join(stateDirectory, bootstrapPairingCodeFile)
	if encoded, err := os.ReadFile(path); err == nil {
		if code := normalizePairingCode(string(encoded)); validPairingCode(code) {
			return code, nil
		}
		return "", errors.New("stored Agent pairing code is invalid")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buffer := make([]byte, 10)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	compact := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buffer)
	code := compact[:4] + "-" + compact[4:8] + "-" + compact[8:12] + "-" + compact[12:16]
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return "", err
	}
	if err := atomicWrite(path, []byte(code+"\n"), 0o600); err != nil {
		return "", err
	}
	return code, nil
}

func normalizePairingCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func validPairingCode(value string) bool {
	if len(value) != 19 {
		return false
	}
	for index, character := range value {
		if index == 4 || index == 9 || index == 14 {
			if character != '-' {
				return false
			}
			continue
		}
		if !(character >= 'A' && character <= 'Z') && !(character >= '2' && character <= '7') {
			return false
		}
	}
	return true
}

func bootstrapCertificate(directory, hostname string) (tls.Certificate, string, error) {
	certificatePath := filepath.Join(directory, "bootstrap.crt")
	keyPath := filepath.Join(directory, "bootstrap.key")
	if certificate, err := tls.LoadX509KeyPair(certificatePath, keyPath); err == nil {
		parsed, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return tls.Certificate{}, "", err
		}
		return certificate, certificateFingerprint(parsed.Raw), nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: hostname, Organization: []string{"XIRANG Bootstrap"}},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(365 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := atomicWrite(certificatePath, certificatePEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := atomicWrite(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return certificate, certificateFingerprint(der), nil
}

func certificateFingerprint(der []byte) string {
	digest := sha256.Sum256(der)
	return hex.EncodeToString(digest[:])
}

func writeBootstrapJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
