package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const nodeURIPrefix = "spiffe://xirang/node/"

type Authority struct {
	directory   string
	certificate *x509.Certificate
	privateKey  *ecdsa.PrivateKey

	// serverMu guards the cached control-plane certificate and the hosts it
	// was issued for, so it can be reissued while the listeners are serving.
	serverMu    sync.Mutex
	serverHosts []string
	serverPair  *tls.Certificate
}

const (
	serverCertificateLifetime = 90 * 24 * time.Hour
	// A controller is expected to run for months at a time. Reissuing well
	// before expiry means the swap happens during normal operation rather than
	// at the moment agents and browsers would start rejecting the listener.
	serverCertificateRenewBefore = 30 * 24 * time.Hour
)

type IssuedCertificate struct {
	CertificatePEM   string
	CACertificatePEM string
	Serial           string
	NotAfter         time.Time
}

func OpenOrCreate(directory string, serverHosts []string) (*Authority, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	authority, err := loadAuthority(directory)
	if errors.Is(err, os.ErrNotExist) {
		authority, err = createAuthority(directory)
	}
	if err != nil {
		return nil, err
	}
	authority.serverHosts = append([]string(nil), serverHosts...)
	if _, err := authority.ServerCertificate(); err != nil {
		return nil, err
	}
	return authority, nil
}

// ServerCertificate returns the control-plane certificate, issuing a fresh one
// when none is loaded or the current one is near expiry. Certificates used to
// be issued only at process start, so a controller that stayed up beyond their
// lifetime began serving an expired certificate and every agent, which verifies
// the listener against this CA, lost its control stream until a restart.
func (authority *Authority) ServerCertificate() (*tls.Certificate, error) {
	authority.serverMu.Lock()
	defer authority.serverMu.Unlock()
	if authority.serverPair != nil && authority.serverPair.Leaf != nil &&
		time.Now().Before(authority.serverPair.Leaf.NotAfter.Add(-serverCertificateRenewBefore)) {
		return authority.serverPair, nil
	}
	if err := authority.issueServerCertificate(authority.serverHosts); err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(authority.ServerCertificatePath(), authority.ServerKeyPath())
	if err != nil {
		return nil, err
	}
	if pair.Leaf == nil {
		leaf, parseErr := x509.ParseCertificate(pair.Certificate[0])
		if parseErr != nil {
			return nil, parseErr
		}
		pair.Leaf = leaf
	}
	authority.serverPair = &pair
	return authority.serverPair, nil
}

func (authority *Authority) CAPath() string { return filepath.Join(authority.directory, "ca.crt") }
func (authority *Authority) ServerCertificatePath() string {
	return filepath.Join(authority.directory, "server.crt")
}
func (authority *Authority) ServerKeyPath() string {
	return filepath.Join(authority.directory, "server.key")
}

func (authority *Authority) TLSConfig() (*tls.Config, error) {
	caPEM, err := os.ReadFile(authority.CAPath())
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("load XIRANG CA certificate")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientCAs:  pool,
		ClientAuth: tls.VerifyClientCertIfGiven,
		// Resolved per handshake so a renewal reaches live listeners without a
		// restart.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return authority.ServerCertificate()
		},
	}, nil
}

func (authority *Authority) IssueNodeCertificate(nodeID string, csrPEM string) (IssuedCertificate, error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return IssuedCertificate{}, errors.New("invalid node certificate request")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || request.CheckSignature() != nil {
		return IssuedCertificate{}, errors.New("invalid node certificate request signature")
	}
	if _, ok := request.PublicKey.(*ecdsa.PublicKey); !ok {
		return IssuedCertificate{}, errors.New("node key must use ECDSA")
	}
	nodeURI, _ := url.Parse(nodeURIPrefix + url.PathEscape(nodeID))
	now := time.Now().UTC()
	serial, err := randomSerial()
	if err != nil {
		return IssuedCertificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"XIRANG Nodes"}},
		NotBefore:    now.Add(-5 * time.Minute), NotAfter: now.Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:                  []*url.URL{nodeURI},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, request.PublicKey, authority.privateKey)
	if err != nil {
		return IssuedCertificate{}, err
	}
	caPEM, err := os.ReadFile(authority.CAPath())
	if err != nil {
		return IssuedCertificate{}, err
	}
	return IssuedCertificate{
		CertificatePEM:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CACertificatePEM: string(caPEM), Serial: serial.Text(16), NotAfter: template.NotAfter,
	}, nil
}

func NodeIDFromCertificate(certificate *x509.Certificate) (string, bool) {
	for _, uri := range certificate.URIs {
		if strings.HasPrefix(uri.String(), nodeURIPrefix) {
			id, err := url.PathUnescape(strings.TrimPrefix(uri.String(), nodeURIPrefix))
			return id, err == nil && id != ""
		}
	}
	return "", false
}

func loadAuthority(directory string) (*Authority, error) {
	certificatePEM, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(directory, "ca.key"))
	if err != nil {
		return nil, err
	}
	certificateBlock, _ := pem.Decode(certificatePEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certificateBlock == nil || keyBlock == nil {
		return nil, errors.New("invalid XIRANG CA files")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, err
	}
	keyValue, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	privateKey, ok := keyValue.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("XIRANG CA key is not ECDSA")
	}
	return &Authority{directory: directory, certificate: certificate, privateKey: privateKey}, nil
}

func createAuthority(directory string) (*Authority, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "XIRANG Control Plane CA", Organization: []string{"XIRANG"}},
		NotBefore:    now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	if err := writePEM(filepath.Join(directory, "ca.crt"), 0o644, "CERTIFICATE", der); err != nil {
		return nil, err
	}
	if err := writePEM(filepath.Join(directory, "ca.key"), 0o600, "PRIVATE KEY", keyDER); err != nil {
		return nil, err
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Authority{directory: directory, certificate: certificate, privateKey: privateKey}, nil
}

func (authority *Authority) issueServerCertificate(hosts []string) error {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "XIRANG Control Plane", Organization: []string{"XIRANG"}},
		NotBefore:    time.Now().Add(-5 * time.Minute), NotAfter: time.Now().Add(serverCertificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, &privateKey.PublicKey, authority.privateKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	if err := writePEM(authority.ServerCertificatePath(), 0o644, "CERTIFICATE", der); err != nil {
		return err
	}
	return writePEM(authority.ServerKeyPath(), 0o600, "PRIVATE KEY", keyDER)
}

func writePEM(path string, mode os.FileMode, blockType string, der []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func randomSerial() (*big.Int, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return nil, err
	}
	serial := new(big.Int).SetBytes(buffer)
	if serial.Sign() == 0 {
		return nil, fmt.Errorf("invalid random serial %s", hex.EncodeToString(buffer))
	}
	return serial, nil
}
