package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssueNodeCertificate(t *testing.T) {
	authority, err := OpenOrCreate(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "candidate"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueNodeCertificate("node_test", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(issued.CertificatePEM))
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if nodeID, ok := NodeIDFromCertificate(certificate); !ok || nodeID != "node_test" {
		t.Fatalf("unexpected node identity: id=%q ok=%v", nodeID, ok)
	}
	if issued.Serial == "" || issued.CACertificatePEM == "" {
		t.Fatal("issued certificate metadata is incomplete")
	}
}

func TestServerCertificateIsReusedUntilItApproachesExpiry(t *testing.T) {
	authority, err := OpenOrCreate(t.TempDir(), []string{"controller.example", "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := authority.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if first.Leaf == nil {
		t.Fatal("certificate was returned without a parsed leaf")
	}
	second, err := authority.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) != 0 {
		t.Fatal("a fresh certificate was reissued while the current one was still valid")
	}
	// The hosts requested at startup must survive into every reissue, or a
	// renewed certificate would stop matching the address agents dial.
	if len(first.Leaf.DNSNames) != 1 || first.Leaf.DNSNames[0] != "controller.example" ||
		len(first.Leaf.IPAddresses) != 1 || first.Leaf.IPAddresses[0].String() != "10.0.0.1" {
		t.Fatalf("server certificate lost its hosts: dns=%v ip=%v", first.Leaf.DNSNames, first.Leaf.IPAddresses)
	}
}

func TestServerCertificateRenewsBeforeALongRunningControllerServesAnExpiredOne(t *testing.T) {
	authority, err := OpenOrCreate(t.TempDir(), []string{"controller.example"})
	if err != nil {
		t.Fatal(err)
	}
	original, err := authority.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a controller that has been up long enough for the cached
	// certificate to fall inside the renewal window.
	authority.serverMu.Lock()
	aged := *original.Leaf
	aged.NotAfter = time.Now().Add(serverCertificateRenewBefore / 2)
	cached := *original
	cached.Leaf = &aged
	authority.serverPair = &cached
	authority.serverMu.Unlock()

	renewed, err := authority.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Leaf.SerialNumber.Cmp(original.Leaf.SerialNumber) == 0 {
		t.Fatal("an expiring certificate was served again instead of being reissued")
	}
	if remaining := time.Until(renewed.Leaf.NotAfter); remaining <= serverCertificateRenewBefore {
		t.Fatalf("the reissued certificate is already inside the renewal window: %s remaining", remaining)
	}
	if len(renewed.Leaf.DNSNames) != 1 || renewed.Leaf.DNSNames[0] != "controller.example" {
		t.Fatalf("the reissued certificate lost its hosts: %v", renewed.Leaf.DNSNames)
	}
}

func TestTLSConfigResolvesTheCertificatePerHandshake(t *testing.T) {
	authority, err := OpenOrCreate(t.TempDir(), []string{"controller.example"})
	if err != nil {
		t.Fatal(err)
	}
	config, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	// A static Certificates entry would take precedence for clients that send
	// no SNI, which is how Agents dial, so renewal has to arrive through
	// GetCertificate and Certificates must stay empty.
	if len(config.Certificates) != 0 {
		t.Fatal("TLSConfig pinned a static certificate, which would outlive its own renewal")
	}
	if config.GetCertificate == nil {
		t.Fatal("TLSConfig does not resolve its certificate per handshake")
	}
	resolved, err := config.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || resolved == nil || resolved.Leaf == nil {
		t.Fatalf("GetCertificate did not return a usable certificate: %v", err)
	}
}

func TestServerCertificateRecoversAfterClockMovesBackwards(t *testing.T) {
	authority, err := OpenOrCreate(t.TempDir(), []string{"controller.example", "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	original, err := authority.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	// Build a real CA-signed certificate for a clock seven hours ahead. The
	// standard X.509 verifier must reject its DER at the corrected time; merely
	// editing cached Leaf metadata would not reproduce the client failure.
	future := *original.Leaf
	future.NotBefore = time.Now().Add(7 * time.Hour)
	future.NotAfter = future.NotBefore.Add(serverCertificateLifetime)
	der, err := x509.CreateCertificate(rand.Reader, &future, authority.certificate,
		original.Leaf.PublicKey, authority.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.certificate)
	options := x509.VerifyOptions{Roots: roots, DNSName: "controller.example", CurrentTime: time.Now()}
	if _, err := leaf.Verify(options); err == nil {
		t.Fatal("test certificate must fail verification before its validity window")
	}
	cached := *original
	cached.Certificate = [][]byte{der}
	cached.Leaf = leaf
	authority.serverMu.Lock()
	authority.serverPair = &cached
	authority.serverMu.Unlock()
	config, err := authority.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := config.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	verifiedLeaf, err := x509.ParseCertificate(renewed.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedLeaf.Verify(options); err != nil {
		t.Fatalf("reissued certificate does not verify against the original CA: %v", err)
	}
	if err := verifiedLeaf.VerifyHostname("10.0.0.1"); err != nil {
		t.Fatalf("reissued certificate lost its IP identity: %v", err)
	}
	if verifiedLeaf.SerialNumber.Cmp(leaf.SerialNumber) == 0 {
		t.Fatal("future-dated certificate was reused after the clock correction")
	}
}
