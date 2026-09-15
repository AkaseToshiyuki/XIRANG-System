package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/pki"
)

type State struct {
	ServerURL string `json:"serverUrl"`
	NodeID    string `json:"nodeId"`
	Identity  string `json:"identity"`
}

type registrationRequest struct {
	Token     string                  `json:"token"`
	CSRPEM    string                  `json:"csrPem"`
	Inventory domain.InventorySummary `json:"inventory"`
}

type registrationResponse struct {
	Node             domain.Node `json:"node"`
	CertificatePEM   string      `json:"certificatePem"`
	CACertificatePEM string      `json:"caCertificatePem"`
}

func Enroll(ctx context.Context, serverURL, token, caFile, stateDirectory string) (State, error) {
	baseURL, err := validateServerURL(serverURL)
	if err != nil {
		return State{}, err
	}
	inventory, err := CollectInventory()
	if err != nil {
		return State{}, err
	}
	privateKey, keyPEM, csrPEM, err := createNodeCSR(inventory.Hostname)
	if err != nil {
		return State{}, err
	}
	requestBody := registrationRequest{
		Token: token, CSRPEM: csrPEM,
		Inventory: domain.InventorySummary{
			Hostname: inventory.Hostname, OS: inventory.OS, Architecture: inventory.Architecture,
			Distribution: inventory.Distribution, KernelVersion: inventory.KernelVersion,
			RuntimeFacts: cloneDeviceAttributes(inventory.RuntimeFacts),
			AgentVersion: inventory.AgentVersion, CPUCount: inventory.CPUCount, MemoryBytes: inventory.MemoryBytes,
			CPUModel: inventory.CPUModel, CPUFeatures: inventory.CPUFeatures,
			CPUBenchmarkScore: inventory.CPUBenchmarkScore, ComputeWeight: inventory.ComputeWeight,
			EphemeralStorageBytes: inventory.EphemeralStorageBytes,
			Devices:               deviceQuantities(inventory.Devices),
			NUMATopology:          numaSummaries(inventory.NUMATopology), DeviceTopology: deviceTopologyQuantities(inventory.Devices),
		},
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return State{}, err
	}
	httpClient, err := bootstrapHTTPClient(caFile)
	if err != nil {
		return State{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/enrollment/register", bytes.NewReader(encoded))
	if err != nil {
		return State{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return State{}, fmt.Errorf("register node: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return State{}, decodeAPIError(response)
	}
	var result registrationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return State{}, err
	}
	if result.CertificatePEM == "" || result.CACertificatePEM == "" {
		return State{}, errors.New("control plane returned incomplete node identity")
	}
	if err := verifyIssuedCertificate(privateKey, result.CertificatePEM, result.Node.ID, result.Node.CertificateSerial); err != nil {
		return State{}, err
	}
	state := State{ServerURL: baseURL, NodeID: result.Node.ID, Identity: result.Node.CertificateSerial}
	if err := saveIdentity(stateDirectory, state, keyPEM, result.CertificatePEM, result.CACertificatePEM); err != nil {
		return State{}, err
	}
	return state, nil
}

func FetchControllerCA(ctx context.Context, serverURL string) (string, string, error) {
	baseURL, err := validateServerURL(serverURL)
	if err != nil {
		return "", "", err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		// #nosec G402 -- the returned peer chain is verified against the returned
		// CA before it is accepted; callers must confirm the displayed CA pin.
		InsecureSkipVerify: true,
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/enrollment/ca", nil)
	if err != nil {
		return "", "", err
	}
	response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return "", "", fmt.Errorf("fetch controller CA: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		return "", "", errors.New("controller CA bootstrap response is invalid")
	}
	var payload struct {
		CACertificatePEM string `json:"caCertificatePem"`
		Fingerprint      string `json:"fingerprint"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", "", err
	}
	block, _ := pem.Decode([]byte(payload.CACertificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", "", errors.New("controller returned an invalid CA certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(ca.Raw)
	fingerprint := hex.EncodeToString(digest[:])
	if payload.Fingerprint != fingerprint {
		return "", "", errors.New("controller CA fingerprint does not match its certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	parsed, _ := url.Parse(baseURL)
	intermediates := x509.NewCertPool()
	for _, certificate := range response.TLS.PeerCertificates[1:] {
		intermediates.AddCert(certificate)
	}
	if _, err := response.TLS.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots: pool, DNSName: parsed.Hostname(), Intermediates: intermediates,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return "", "", fmt.Errorf("controller TLS certificate is not signed by the advertised CA: %w", err)
	}
	return payload.CACertificatePEM, fingerprint, nil
}

func numaSummaries(nodes []NUMANodeInventory) []domain.NUMANode {
	result := make([]domain.NUMANode, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, domain.NUMANode{ID: node.ID, CPUs: node.CPUs, MemoryBytes: node.MemoryBytes})
	}
	return result
}

func deviceTopologyQuantities(devices []DeviceInventory) map[string]map[string]int64 {
	topology := make(map[string]map[string]int64)
	for _, device := range devices {
		numa := device.Attributes["numaNode"]
		if device.ResourceName == "" || numa == "" {
			continue
		}
		if topology[device.ResourceName] == nil {
			topology[device.ResourceName] = make(map[string]int64)
		}
		topology[device.ResourceName][numa]++
	}
	if len(topology) == 0 {
		return nil
	}
	return topology
}

func deviceQuantities(devices []DeviceInventory) map[string]int64 {
	quantities := make(map[string]int64)
	for _, device := range devices {
		if device.ResourceName != "" {
			quantities[device.ResourceName]++
		}
	}
	if len(quantities) == 0 {
		return nil
	}
	return quantities
}

func SendHeartbeat(ctx context.Context, state State, stateDirectory string) (domain.Node, error) {
	httpClient, err := authenticatedHTTPClient(stateDirectory, state)
	if err != nil {
		return domain.Node{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		state.ServerURL+"/api/v1/nodes/"+url.PathEscape(state.NodeID)+"/heartbeat", nil)
	if err != nil {
		return domain.Node{}, err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return domain.Node{}, fmt.Errorf("send heartbeat: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return domain.Node{}, decodeAPIError(response)
	}
	var node domain.Node
	if err := json.NewDecoder(response.Body).Decode(&node); err != nil {
		return domain.Node{}, err
	}
	return node, nil
}

func RotateCertificate(ctx context.Context, state State, stateDirectory string) (State, domain.Node, error) {
	if _, err := SendHeartbeat(ctx, state, stateDirectory); err != nil {
		return State{}, domain.Node{}, fmt.Errorf("verify current certificate: %w", err)
	}
	inventory, err := CollectInventory()
	if err != nil {
		return State{}, domain.Node{}, err
	}
	privateKey, keyPEM, csrPEM, err := createNodeCSR(inventory.Hostname)
	if err != nil {
		return State{}, domain.Node{}, err
	}
	payload, err := json.Marshal(map[string]string{"csrPem": csrPEM})
	if err != nil {
		return State{}, domain.Node{}, err
	}
	httpClient, err := authenticatedHTTPClient(stateDirectory, state)
	if err != nil {
		return State{}, domain.Node{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		state.ServerURL+"/api/v1/nodes/"+url.PathEscape(state.NodeID)+"/certificate/rotate", bytes.NewReader(payload))
	if err != nil {
		return State{}, domain.Node{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return State{}, domain.Node{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return State{}, domain.Node{}, decodeAPIError(response)
	}
	var result registrationResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return State{}, domain.Node{}, err
	}
	if result.Node.PendingCertificateSerial == "" {
		return State{}, domain.Node{}, errors.New("control plane did not return a pending certificate identity")
	}
	if err := verifyIssuedCertificate(privateKey, result.CertificatePEM, state.NodeID, result.Node.PendingCertificateSerial); err != nil {
		return State{}, domain.Node{}, err
	}
	rotatedState := state
	rotatedState.Identity = result.Node.PendingCertificateSerial
	if err := saveIdentity(stateDirectory, rotatedState, keyPEM, result.CertificatePEM, result.CACertificatePEM); err != nil {
		return State{}, domain.Node{}, err
	}
	activated, err := SendHeartbeat(ctx, rotatedState, stateDirectory)
	if err != nil {
		return rotatedState, domain.Node{}, fmt.Errorf("certificate saved but activation heartbeat failed: %w", err)
	}
	return rotatedState, activated, nil
}

func SaveState(directory string, state State) error {
	if state.NodeID == "" || state.ServerURL == "" || state.Identity == "" {
		return errors.New("agent state is incomplete")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(directory, "state.json"), append(encoded, '\n'), 0o600)
}

func LoadState(directory string) (State, error) {
	data, err := os.ReadFile(filepath.Join(directory, "state.json"))
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, err
	}
	if state.NodeID == "" || state.ServerURL == "" {
		return State{}, errors.New("agent state is incomplete")
	}
	if state.Identity == "" {
		return migrateLegacyIdentity(directory, state)
	}
	return state, nil
}

func migrateLegacyIdentity(directory string, state State) (State, error) {
	keyPEM, err := os.ReadFile(filepath.Join(directory, "node.key"))
	if err != nil {
		return State{}, fmt.Errorf("read legacy node key: %w", err)
	}
	certificatePEM, err := os.ReadFile(filepath.Join(directory, "node.crt"))
	if err != nil {
		return State{}, fmt.Errorf("read legacy node certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil {
		return State{}, fmt.Errorf("read legacy CA certificate: %w", err)
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return State{}, errors.New("invalid legacy node certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return State{}, fmt.Errorf("parse legacy node certificate: %w", err)
	}
	state.Identity = certificate.SerialNumber.Text(16)
	if err := saveIdentity(directory, state, string(keyPEM), string(certificatePEM), string(caPEM)); err != nil {
		return State{}, fmt.Errorf("migrate legacy Agent identity: %w", err)
	}
	return state, nil
}

func validateServerURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return "", errors.New("control-plane URL must be a valid https URL")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func createNodeCSR(hostname string) (*ecdsa.PrivateKey, string, string, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, "", "", err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: hostname, Organization: []string{"XIRANG Candidate Nodes"}},
	}, privateKey)
	if err != nil {
		return nil, "", "", err
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))
	return privateKey, keyPEM, csrPEM, nil
}

func verifyIssuedCertificate(privateKey *ecdsa.PrivateKey, certificatePEM, nodeID, serial string) error {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		return errors.New("invalid issued node certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || !publicKey.Equal(&privateKey.PublicKey) {
		return errors.New("issued certificate does not match generated node key")
	}
	certificateNodeID, ok := pki.NodeIDFromCertificate(certificate)
	if !ok || certificateNodeID != nodeID {
		return errors.New("issued certificate has an unexpected node identity")
	}
	if serial == "" || certificate.SerialNumber.Text(16) != serial {
		return errors.New("issued certificate serial does not match control-plane state")
	}
	return nil
}

func bootstrapHTTPClient(caFile string) (*http.Client, error) {
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool,
	}}}, nil
}

func authenticatedHTTPClient(directory string, state State) (*http.Client, error) {
	tlsConfig, err := authenticatedTLSConfig(directory, state)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}

func authenticatedTLSConfig(directory string, state State) (*tls.Config, error) {
	identityDirectory := filepath.Join(directory, "identities", state.Identity)
	certificate, err := tls.LoadX509KeyPair(filepath.Join(identityDirectory, "node.crt"), filepath.Join(identityDirectory, "node.key"))
	if err != nil {
		return nil, err
	}
	pool, err := loadCAPool(filepath.Join(identityDirectory, "ca.crt"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate},
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read control-plane CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid control-plane CA certificate")
	}
	return pool, nil
}

func saveIdentity(directory string, state State, keyPEM, certificatePEM, caPEM string) error {
	if state.Identity == "" {
		return errors.New("certificate identity serial is required")
	}
	identityDirectory := filepath.Join(directory, "identities", state.Identity)
	if err := os.MkdirAll(identityDirectory, 0o700); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(identityDirectory, "node.key"), []byte(keyPEM), 0o600); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(identityDirectory, "node.crt"), []byte(certificatePEM), 0o644); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(identityDirectory, "ca.crt"), []byte(caPEM), 0o644); err != nil {
		return err
	}
	return SaveState(directory, state)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func decodeAPIError(response *http.Response) error {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err == nil && payload.Error != "" {
		return fmt.Errorf("control plane returned %s: %s", response.Status, payload.Error)
	}
	return fmt.Errorf("control plane returned %s", response.Status)
}
