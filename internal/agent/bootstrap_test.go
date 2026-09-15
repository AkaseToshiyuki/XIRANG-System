package agent

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestBootstrapPairingCodeIsStableAndPrivate(t *testing.T) {
	directory := t.TempDir()
	first, err := BootstrapPairingCode(directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BootstrapPairingCode(directory)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !regexp.MustCompile(`^[A-Z2-7]{4}(?:-[A-Z2-7]{4}){3}$`).MatchString(first) {
		t.Fatalf("unexpected pairing code lifecycle: first=%q second=%q", first, second)
	}
	info, err := os.Stat(filepath.Join(directory, bootstrapPairingCodeFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pairing code mode = %o, want 600", info.Mode().Perm())
	}
}

func TestBootstrapPairingCodeRejectsInvalidStoredValue(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, bootstrapPairingCodeFile), []byte("predictable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapPairingCode(directory); err == nil {
		t.Fatal("invalid stored pairing code was accepted")
	}
}

func TestBootstrapCertificatePersistsStableFingerprint(t *testing.T) {
	directory := t.TempDir()
	first, firstFingerprint, err := bootstrapCertificate(directory, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	second, secondFingerprint, err := bootstrapCertificate(directory, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if firstFingerprint == "" || firstFingerprint != secondFingerprint {
		t.Fatalf("bootstrap identity was not stable: %q %q", firstFingerprint, secondFingerprint)
	}
	parsed, err := x509.ParseCertificate(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject.CommonName != "node-a" || certificateFingerprint(parsed.Raw) != firstFingerprint ||
		len(second.Certificate) == 0 {
		t.Fatalf("unexpected bootstrap certificate: %#v", parsed.Subject)
	}
}
