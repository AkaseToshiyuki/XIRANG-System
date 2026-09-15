package discovery_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/discovery"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/pki"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestRestrictedDiscoveryPersistsCandidatesAndRequiresApproval(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentService := enrollment.NewService(store, authority)
	probe := func(_ context.Context, address string, port int) (domain.DiscoveryCandidate, error) {
		if address != "127.0.0.2" || port != 8844 {
			return domain.DiscoveryCandidate{}, errors.New("closed")
		}
		return domain.DiscoveryCandidate{
			Address: address, AgentPort: port, OpenPorts: []int{port},
			Protocol: "xirang-agent-bootstrap-v1", AgentVersion: "test",
			Hostname: "worker-a", Architecture: "arm64", Fingerprint: "fingerprint",
		}, nil
	}
	service, err := discovery.New(store, enrollmentService, discovery.Config{
		AllowedCIDRs: []string{"127.0.0.0/24"}, AgentPort: 8844, MaxHosts: 8,
		Concurrency: 2, ConnectTimeout: 50 * time.Millisecond,
		ControllerURL: "https://controller.example:8899", ControllerCAPEM: "test-ca",
		ProbeAgent: probe,
		EnrollAgent: func(_ context.Context, candidate domain.DiscoveryCandidate, controllerURL, caPEM, token, pairingCode string) (string, error) {
			if candidate.Address != "127.0.0.2" || controllerURL == "" || caPEM == "" || token == "" {
				t.Fatal("approval omitted automatic enrollment inputs")
			}
			return "node-enrolled", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := service.Scan(context.Background(), []string{"127.0.0.0/30"})
	if err != nil {
		t.Fatal(err)
	}
	if scan.ScannedHosts != 4 || scan.CandidatesSeen != 1 {
		t.Fatalf("unexpected bounded scan: %#v", scan)
	}
	candidates, err := service.ListCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Address != "127.0.0.2" ||
		candidates[0].Hostname != "worker-a" || candidates[0].AgentPort != 8844 {
		t.Fatalf("unexpected candidate: %#v", candidates)
	}
	bootstrap, err := service.Approve(context.Background(), candidates[0].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap.EnrollmentToken != "" || bootstrap.Candidate.ApprovedAt == nil ||
		bootstrap.NodeID != "node-enrolled" || time.Until(bootstrap.ExpiresAt) > 16*time.Minute {
		t.Fatalf("approval did not return bounded one-use bootstrap: %#v", bootstrap)
	}
	if _, err := service.Approve(context.Background(), candidates[0].ID, ""); !errors.Is(err, discovery.ErrConflict) {
		t.Fatalf("duplicate approval should conflict, got %v", err)
	}
	if _, err := service.Scan(context.Background(), []string{"10.0.0.0/30"}); !errors.Is(err, discovery.ErrInvalidRequest) {
		t.Fatalf("out-of-allowlist scan was accepted: %v", err)
	}
	if _, err := service.Scan(context.Background(), []string{"10.0.0.7"}); !errors.Is(err, discovery.ErrInvalidRequest) {
		t.Fatalf("out-of-allowlist bare address was accepted: %v", err)
	}
	if _, err := service.Scan(context.Background(), []string{"127.0.0.0/28"}); !errors.Is(err, discovery.ErrInvalidRequest) {
		t.Fatalf("host-limit overflow was accepted: %v", err)
	}
}

func TestSingleIPAddressScanDoesNotRequireCIDRAllowlist(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := discovery.New(store, nil, discovery.Config{
		AgentPort: 8844, MaxHosts: 8, Concurrency: 1,
		ProbeAgent: func(_ context.Context, address string, port int) (domain.DiscoveryCandidate, error) {
			if address != "192.0.2.7" || port != 8844 {
				return domain.DiscoveryCandidate{}, errors.New("unexpected target")
			}
			return domain.DiscoveryCandidate{
				Address: address, AgentPort: port, Protocol: "xirang-agent-bootstrap-v1",
				Hostname: "waiting-node", Architecture: "amd64", Fingerprint: "test",
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := service.Scan(context.Background(), []string{"192.0.2.7"})
	if err != nil {
		t.Fatal(err)
	}
	if scan.ScannedHosts != 1 || scan.CandidatesSeen != 1 || len(scan.CIDRs) != 1 || scan.CIDRs[0] != "192.0.2.7/32" {
		t.Fatalf("single-address scan was not bounded to one Agent: %#v", scan)
	}
	if _, err := service.Scan(context.Background(), []string{"192.0.2.0/30"}); !errors.Is(err, discovery.ErrDisabled) {
		t.Fatalf("CIDR scan without an allowlist should remain disabled, got %v", err)
	}
}
