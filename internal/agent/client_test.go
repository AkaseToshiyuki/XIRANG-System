package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateServerURL(t *testing.T) {
	if _, err := validateServerURL("http://127.0.0.1:8080"); err == nil {
		t.Fatal("expected plaintext URL to be rejected")
	}
	if _, err := validateServerURL("https://control.example"); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultControlAddress(t *testing.T) {
	address, err := DefaultControlAddress(State{ServerURL: "https://control.example:9443"})
	if err != nil {
		t.Fatal(err)
	}
	if address != "control.example:8444" {
		t.Fatalf("unexpected control address: %q", address)
	}
}

func TestAgentStateRoundTrip(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agent")
	expected := State{ServerURL: "https://control.example", NodeID: "node_test", Identity: "serial_test"}
	if err := SaveState(directory, expected); err != nil {
		t.Fatal(err)
	}
	actual, err := LoadState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("state mismatch: got %#v want %#v", actual, expected)
	}
}

func TestLoadStateMigratesLegacyIdentityLayout(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := State{ServerURL: "https://control.example", NodeID: "node_legacy"}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "state.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "legacy"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	for name, data := range map[string][]byte{
		"node.key": privatePEM, "node.crt": certificatePEM, "ca.crt": certificatePEM,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	migrated, err := LoadState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Identity != "2a" {
		t.Fatalf("unexpected migrated identity: %q", migrated.Identity)
	}
	for _, name := range []string{"node.key", "node.crt", "ca.crt"} {
		if _, err := os.Stat(filepath.Join(directory, "identities", "2a", name)); err != nil {
			t.Fatalf("migrated %s: %v", name, err)
		}
	}
	reloaded, err := LoadState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != migrated {
		t.Fatalf("persisted migration mismatch: got %#v want %#v", reloaded, migrated)
	}
}
