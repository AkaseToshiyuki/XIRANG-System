package discovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
)

var (
	ErrDisabled       = errors.New("network discovery is disabled")
	ErrInvalidRequest = errors.New("invalid discovery request")
	ErrNotFound       = errors.New("discovery resource not found")
	ErrConflict       = errors.New("discovery state conflict")
)

type Repository interface {
	CreateDiscoveryScan(context.Context, domain.DiscoveryScan) error
	FinishDiscoveryScan(context.Context, domain.DiscoveryScan, []domain.DiscoveryCandidate) error
	ListDiscoveryScans(context.Context, int) ([]domain.DiscoveryScan, error)
	ListDiscoveryCandidates(context.Context) ([]domain.DiscoveryCandidate, error)
	UpdateDiscoveryCandidate(context.Context, string, func(*domain.DiscoveryCandidate) error, *domain.AuditEvent) (domain.DiscoveryCandidate, error)
}

type TokenIssuer interface {
	CreateToken(context.Context, enrollment.TokenOptions) (enrollment.CreatedToken, error)
	RevokeToken(context.Context, string) error
}

type Config struct {
	AllowedCIDRs    []string
	Ports           []int
	AgentPort       int
	MaxHosts        int
	Concurrency     int
	ConnectTimeout  time.Duration
	ControllerURL   string
	ControllerCAPEM string
	Dial            func(context.Context, string, string) (net.Conn, error)
	ProbeAgent      func(context.Context, string, int) (domain.DiscoveryCandidate, error)
	EnrollAgent     func(context.Context, domain.DiscoveryCandidate, string, string, string, string) (string, error)
}

type Service struct {
	repository      Repository
	issuer          TokenIssuer
	approvalMu      sync.Mutex
	allowed         []netip.Prefix
	ports           []int
	maxHosts        int
	concurrency     int
	timeout         time.Duration
	controllerURL   string
	controllerCAPEM string
	now             func() time.Time
	dial            func(context.Context, string, string) (net.Conn, error)
	probeAgent      func(context.Context, string, int) (domain.DiscoveryCandidate, error)
	enrollAgent     func(context.Context, domain.DiscoveryCandidate, string, string, string, string) (string, error)
}

func New(repository Repository, issuer TokenIssuer, config Config) (*Service, error) {
	service := &Service{
		repository: repository, issuer: issuer, maxHosts: config.MaxHosts,
		concurrency: config.Concurrency, timeout: config.ConnectTimeout,
		controllerURL:   strings.TrimRight(strings.TrimSpace(config.ControllerURL), "/"),
		controllerCAPEM: config.ControllerCAPEM, now: time.Now,
	}
	if service.maxHosts == 0 {
		service.maxHosts = 256
	}
	if service.concurrency == 0 {
		service.concurrency = 32
	}
	if service.timeout == 0 {
		service.timeout = 300 * time.Millisecond
	}
	if service.maxHosts < 1 || service.maxHosts > 4096 || service.concurrency < 1 || service.concurrency > 128 ||
		service.timeout < 10*time.Millisecond || service.timeout > 5*time.Second {
		return nil, fmt.Errorf("%w: unsafe scan bounds", ErrInvalidRequest)
	}
	for _, raw := range config.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: allowed CIDR %q", ErrInvalidRequest, raw)
		}
		service.allowed = append(service.allowed, prefix.Masked())
	}
	agentPort := config.AgentPort
	if agentPort == 0 && len(config.Ports) == 1 {
		agentPort = config.Ports[0]
	}
	if agentPort == 0 {
		agentPort = 8844
	}
	if agentPort < 1 || agentPort > 65535 {
		return nil, fmt.Errorf("%w: invalid Agent discovery port", ErrInvalidRequest)
	}
	service.ports = []int{agentPort}
	dialer := &net.Dialer{Timeout: service.timeout}
	service.dial = dialer.DialContext
	if config.Dial != nil {
		service.dial = config.Dial
	}
	service.probeAgent = service.probeAgentStatus
	if config.ProbeAgent != nil {
		service.probeAgent = config.ProbeAgent
	}
	service.enrollAgent = service.enrollCandidate
	if config.EnrollAgent != nil {
		service.enrollAgent = config.EnrollAgent
	}
	return service, nil
}

func (service *Service) Scan(ctx context.Context, cidrs []string) (domain.DiscoveryScan, error) {
	prefixes, addresses, err := service.validateTargets(cidrs)
	if err != nil {
		return domain.DiscoveryScan{}, err
	}
	id, err := randomID("scan_")
	if err != nil {
		return domain.DiscoveryScan{}, err
	}
	now := service.now().UTC()
	scan := domain.DiscoveryScan{
		ID: id, CIDRs: prefixes, Ports: append([]int(nil), service.ports...),
		Status: domain.DiscoveryScanRunning, ScannedHosts: len(addresses), StartedAt: now,
	}
	if err := service.repository.CreateDiscoveryScan(ctx, scan); err != nil {
		return domain.DiscoveryScan{}, err
	}
	work := make(chan string)
	results := make(chan domain.DiscoveryCandidate, len(addresses))
	var workers sync.WaitGroup
	for index := 0; index < service.concurrency; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for address := range work {
				candidate, err := service.probeAgent(ctx, address, service.ports[0])
				if err == nil {
					results <- candidate
				}
			}
		}()
	}
	go func() {
		defer close(work)
		for _, address := range addresses {
			select {
			case work <- address:
			case <-ctx.Done():
				return
			}
		}
	}()
	workers.Wait()
	close(results)
	finished := service.now().UTC()
	scan.FinishedAt = &finished
	candidates := make([]domain.DiscoveryCandidate, 0, len(results))
	for candidate := range results {
		candidate.ID = candidateID(candidate.Address)
		candidate.Status = domain.DiscoveryCandidateDiscovered
		candidate.FirstSeenAt, candidate.LastSeenAt, candidate.LastScanID = finished, finished, scan.ID
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Address < candidates[j].Address })
	scan.CandidatesSeen = len(candidates)
	if err := ctx.Err(); err != nil {
		scan.Status, scan.Failure = domain.DiscoveryScanFailed, "scan canceled"
	} else {
		scan.Status = domain.DiscoveryScanCompleted
	}
	if err := service.repository.FinishDiscoveryScan(context.WithoutCancel(ctx), scan, candidates); err != nil {
		return domain.DiscoveryScan{}, err
	}
	if scan.Status == domain.DiscoveryScanFailed {
		return scan, ctx.Err()
	}
	return scan, nil
}

func (service *Service) ListScans(ctx context.Context, limit int) ([]domain.DiscoveryScan, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalidRequest
	}
	return service.repository.ListDiscoveryScans(ctx, limit)
}

func (service *Service) ListCandidates(ctx context.Context) ([]domain.DiscoveryCandidate, error) {
	return service.repository.ListDiscoveryCandidates(ctx)
}

func (service *Service) Approve(ctx context.Context, id, pairingCode string) (domain.DiscoveryBootstrap, error) {
	service.approvalMu.Lock()
	defer service.approvalMu.Unlock()
	if service.issuer == nil || service.controllerURL == "" || service.controllerCAPEM == "" || service.enrollAgent == nil {
		return domain.DiscoveryBootstrap{}, ErrDisabled
	}
	candidates, err := service.repository.ListDiscoveryCandidates(ctx)
	if err != nil {
		return domain.DiscoveryBootstrap{}, err
	}
	var selected domain.DiscoveryCandidate
	for _, candidate := range candidates {
		if candidate.ID == id {
			selected = candidate
			break
		}
	}
	if selected.ID == "" {
		return domain.DiscoveryBootstrap{}, ErrNotFound
	}
	if selected.Status != domain.DiscoveryCandidateDiscovered {
		return domain.DiscoveryBootstrap{}, ErrConflict
	}
	if selected.PairingRequired && !validPairingCode(pairingCode) {
		return domain.DiscoveryBootstrap{}, fmt.Errorf("%w: the Agent pairing code is required", ErrInvalidRequest)
	}
	created, err := service.issuer.CreateToken(ctx, enrollment.TokenOptions{
		Name: "approved discovery candidate " + id, TTL: 15 * time.Minute, MaxUses: 1, RequireApproval: false,
	})
	if err != nil {
		return domain.DiscoveryBootstrap{}, err
	}
	nodeID, err := service.enrollAgent(ctx, selected, service.controllerURL, service.controllerCAPEM, created.Secret, normalizePairingCode(pairingCode))
	if err != nil {
		_ = service.issuer.RevokeToken(context.WithoutCancel(ctx), created.Token.ID)
		return domain.DiscoveryBootstrap{}, fmt.Errorf("enroll discovered Agent: %w", err)
	}
	now := service.now().UTC()
	candidate, err := service.repository.UpdateDiscoveryCandidate(ctx, id, func(candidate *domain.DiscoveryCandidate) error {
		if candidate.Status != domain.DiscoveryCandidateDiscovered {
			return ErrConflict
		}
		candidate.Status, candidate.ApprovedAt, candidate.BootstrapTTL = domain.DiscoveryCandidateApproved, &now, 15*time.Minute
		candidate.EnrolledNodeID = nodeID
		return nil
	}, &domain.AuditEvent{
		ID: "candidate_approved_" + id, ActorType: "admin", ActorID: "bootstrap-admin",
		Action: "discovery.candidate.approved", TargetKind: "discovery_candidate", TargetID: id,
		Outcome: "succeeded", OccurredAt: now, Details: map[string]string{
			"address": selected.Address, "fingerprint": selected.Fingerprint, "nodeId": nodeID,
		},
	})
	if err != nil {
		return domain.DiscoveryBootstrap{}, err
	}
	return domain.DiscoveryBootstrap{
		Candidate: candidate, ExpiresAt: created.Token.ExpiresAt, ControllerURL: service.controllerURL, NodeID: nodeID,
	}, nil
}

func (service *Service) Reject(ctx context.Context, id string) (domain.DiscoveryCandidate, error) {
	now := service.now().UTC()
	return service.repository.UpdateDiscoveryCandidate(ctx, id, func(candidate *domain.DiscoveryCandidate) error {
		if candidate.Status != domain.DiscoveryCandidateDiscovered {
			return ErrConflict
		}
		candidate.Status, candidate.RejectedAt = domain.DiscoveryCandidateRejected, &now
		return nil
	}, &domain.AuditEvent{
		ID: "candidate_rejected_" + id, ActorType: "admin", ActorID: "bootstrap-admin",
		Action: "discovery.candidate.rejected", TargetKind: "discovery_candidate", TargetID: id,
		Outcome: "succeeded", OccurredAt: now,
	})
}

func (service *Service) validateTargets(rawCIDRs []string) ([]string, []string, error) {
	if len(rawCIDRs) == 0 || len(rawCIDRs) > 16 {
		return nil, nil, ErrInvalidRequest
	}
	prefixes := make([]string, 0, len(rawCIDRs))
	addresses := make([]string, 0)
	seen := map[netip.Addr]bool{}
	for _, raw := range rawCIDRs {
		value := strings.TrimSpace(raw)
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, addressErr := netip.ParseAddr(value)
			if addressErr != nil {
				return nil, nil, ErrInvalidRequest
			}
			bits := 128
			if address.Is4() {
				bits = 32
			}
			prefix = netip.PrefixFrom(address, bits)
		}
		prefix = prefix.Masked()
		// An explicitly configured boundary applies equally to CIDRs and bare
		// addresses. A bare address remains a bounded one-host scan when the
		// deployment intentionally has no CIDR boundary.
		allowed := len(service.allowed) == 0 && !strings.Contains(value, "/")
		for _, boundary := range service.allowed {
			if boundary.Addr().BitLen() == prefix.Addr().BitLen() && boundary.Contains(prefix.Addr()) && prefix.Bits() >= boundary.Bits() {
				allowed = true
				break
			}
		}
		if !allowed {
			if len(service.allowed) == 0 {
				return nil, nil, ErrDisabled
			}
			return nil, nil, fmt.Errorf("%w: CIDR is outside the configured allowlist", ErrInvalidRequest)
		}
		prefixes = append(prefixes, prefix.String())
		for address := prefix.Addr(); prefix.Contains(address); address = address.Next() {
			if !seen[address] {
				seen[address] = true
				addresses = append(addresses, address.String())
				if len(addresses) > service.maxHosts {
					return nil, nil, fmt.Errorf("%w: scan exceeds %d unique hosts", ErrInvalidRequest, service.maxHosts)
				}
			}
		}
	}
	sort.Strings(prefixes)
	sort.Strings(addresses)
	return prefixes, addresses, nil
}

func (service *Service) probeAgentStatus(ctx context.Context, address string, port int) (domain.DiscoveryCandidate, error) {
	target := net.JoinHostPort(address, strconv.Itoa(port))
	// Bootstrap discovery uses TOFU: the self-signed certificate fingerprint is
	// captured here and pinned during the later administrator-approved request.
	transport := &http.Transport{
		DialContext: service.dial,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// #nosec G402 -- identity is pinned from the observed certificate below.
			InsecureSkipVerify: true,
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: max(2*time.Second, service.timeout)}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+target+"/xirang/v1/status", nil)
	if err != nil {
		return domain.DiscoveryCandidate{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return domain.DiscoveryCandidate{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		return domain.DiscoveryCandidate{}, errors.New("endpoint is not an available XIRANG Agent")
	}
	var status struct {
		Protocol        string                            `json:"protocol"`
		AgentVersion    string                            `json:"agentVersion"`
		Hostname        string                            `json:"hostname"`
		Architecture    string                            `json:"architecture"`
		Distribution    string                            `json:"distribution"`
		KernelVersion   string                            `json:"kernelVersion"`
		CPUCount        int                               `json:"cpuCount"`
		MemoryBytes     int64                             `json:"memoryBytes"`
		Interfaces      []domain.NetworkInterfaceSnapshot `json:"interfaces"`
		Fingerprint     string                            `json:"fingerprint"`
		PairingRequired bool                              `json:"pairingRequired"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&status); err != nil {
		return domain.DiscoveryCandidate{}, err
	}
	fingerprint := certificateFingerprint(response.TLS.PeerCertificates[0].Raw)
	if status.Protocol != "xirang-agent-bootstrap-v1" || status.Hostname == "" || status.Architecture == "" ||
		len(status.Hostname) > 255 || len(status.Architecture) > 64 || len(status.AgentVersion) > 64 ||
		status.CPUCount < 1 || status.CPUCount > 1_000_000 || status.MemoryBytes < 1 ||
		status.Fingerprint != fingerprint || !status.PairingRequired || len(status.Interfaces) > 256 {
		return domain.DiscoveryCandidate{}, errors.New("Agent bootstrap status is invalid")
	}
	for _, networkInterface := range status.Interfaces {
		if networkInterface.Name == "" || len(networkInterface.Name) > 64 || len(networkInterface.Addresses) > 64 ||
			networkInterface.SpeedBitsPerSecond > 1_000_000_000_000_000 {
			return domain.DiscoveryCandidate{}, errors.New("Agent bootstrap interface status is invalid")
		}
		for _, rawAddress := range networkInterface.Addresses {
			if _, err := netip.ParsePrefix(rawAddress); err != nil {
				return domain.DiscoveryCandidate{}, errors.New("Agent bootstrap interface address is invalid")
			}
		}
	}
	return domain.DiscoveryCandidate{
		Address: address, AgentPort: port, OpenPorts: []int{port},
		Protocol: status.Protocol, AgentVersion: status.AgentVersion,
		Hostname: status.Hostname, Architecture: status.Architecture,
		Distribution: status.Distribution, KernelVersion: status.KernelVersion,
		CPUCount: status.CPUCount, MemoryBytes: status.MemoryBytes,
		Interfaces: status.Interfaces, Fingerprint: fingerprint, PairingRequired: status.PairingRequired,
	}, nil
}

func (service *Service) enrollCandidate(ctx context.Context, candidate domain.DiscoveryCandidate, controllerURL, caPEM, token, pairingCode string) (string, error) {
	expected, err := hex.DecodeString(candidate.Fingerprint)
	if err != nil || len(expected) != sha256.Size {
		return "", errors.New("candidate certificate fingerprint is invalid")
	}
	target := net.JoinHostPort(candidate.Address, strconv.Itoa(candidate.AgentPort))
	transport := &http.Transport{
		DialContext: service.dial,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// #nosec G402 -- VerifyPeerCertificate enforces the scan-time pin.
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCertificates [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCertificates) == 0 {
					return errors.New("candidate omitted its bootstrap certificate")
				}
				digest := sha256.Sum256(rawCertificates[0])
				if !bytes.Equal(digest[:], expected) {
					return errors.New("candidate bootstrap certificate changed after discovery")
				}
				return nil
			},
		},
	}
	defer transport.CloseIdleConnections()
	payload, err := json.Marshal(map[string]string{
		"controllerUrl": controllerURL, "caCertificatePem": caPEM, "token": token, "pairingCode": pairingCode,
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+target+"/xirang/v1/enroll", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: transport, Timeout: 50 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("Agent bootstrap returned HTTP %d", response.StatusCode)
	}
	var result struct {
		NodeID string `json:"nodeId"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", err
	}
	if result.NodeID == "" {
		return "", errors.New("Agent bootstrap omitted the enrolled node ID")
	}
	return result.NodeID, nil
}

func normalizePairingCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func validPairingCode(value string) bool {
	value = normalizePairingCode(value)
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

func certificateFingerprint(der []byte) string {
	digest := sha256.Sum256(der)
	return hex.EncodeToString(digest[:])
}

func candidateID(address string) string {
	replacer := strings.NewReplacer(".", "_", ":", "_", "%", "_")
	return "candidate_" + replacer.Replace(address)
}

func randomID(prefix string) (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}
