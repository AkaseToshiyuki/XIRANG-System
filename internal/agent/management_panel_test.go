package agent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestManagementPanelCertificateContainsLocalHostname(t *testing.T) {
	directory := t.TempDir()
	if _, err := managementPanelCertificate(directory); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(directory, "management-panel.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		t.Fatal("management panel certificate is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := CollectInventory()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Hostname != "" && certificate.VerifyHostname(inventory.Hostname) != nil {
		t.Fatalf("certificate does not cover local hostname %q", inventory.Hostname)
	}
	if info, err := os.Stat(filepath.Join(directory, "management-panel.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("management panel private key permissions are not 0600: info=%#v err=%v", info, err)
	}
}

func TestEnrolledControllerLocalityUsesInventory(t *testing.T) {
	inventory, err := CollectInventory()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Hostname == "" {
		t.Skip("host has no name")
	}
	if !enrolledControllerIsLocal(State{ServerURL: "https://" + inventory.Hostname + ":8899"}) {
		t.Fatal("local controller hostname was not recognized")
	}
	if enrolledControllerIsLocal(State{ServerURL: "https://192.0.2.250:8899"}) {
		t.Fatal("documentation-only remote address was treated as local")
	}
}

func TestManagementPanelDoesNotPresentAgentIdentityUpstream(t *testing.T) {
	certificateDirectory := t.TempDir()
	if _, err := managementPanelCertificate(certificateDirectory); err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(certificateDirectory, "management-panel.crt"))
	if err != nil {
		t.Fatal(err)
	}
	stateDirectory := t.TempDir()
	identityDirectory := filepath.Join(stateDirectory, "identities", "serial-1")
	if err := os.MkdirAll(identityDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(identityDirectory, "ca.crt"), caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("https://controller.example:8899")
	tlsConfig, err := managementPanelUpstreamTLSConfig(stateDirectory, State{Identity: "serial-1"}, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.RootCAs == nil || tlsConfig.ServerName != "controller.example" || tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("unexpected upstream trust configuration: %#v", tlsConfig)
	}
	if len(tlsConfig.Certificates) != 0 || tlsConfig.GetClientCertificate != nil {
		t.Fatal("management panel inherited the Agent client certificate")
	}
}
