package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

const (
	localCACertFileName = "flitnix-local-ca.crt"
	localCAKeyFileName  = "flitnix-local-ca.key"
	localCAValidity     = 10 * 365 * 24 * time.Hour
	localLeafValidity   = 90 * 24 * time.Hour
)

// LocalMDNSHost returns the deterministic .local mDNS hostname for hostID
// (e.g. "flitnix-0123456789abcdef.local"), or "" if hostID is empty.
func LocalMDNSHost(hostID string) string {
	clean := strings.ToLower(strings.TrimSpace(hostID))
	if clean == "" {
		return ""
	}
	if len(clean) > 16 {
		clean = clean[:16]
	}
	return "flitnix-" + clean + ".local"
}

// LocalHTTPSManager manages an opt-in local ECDSA P-256 Root CA and
// host/IP-bound leaf certificates for browser HTTPS sessions on the LAN.
//
// Plain local HTTP remains available on the same port via tlsPeekListener so
// zero-friction QR guest sharing continues without certificate warnings.
type LocalHTTPSManager struct {
	mu       sync.Mutex
	dir      string
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey
	caPEM    []byte
	caSHA256 string

	leafKey     string // "hostID|listenIP"
	leafCert    *tls.Certificate
	leafExpires time.Time
}

// NewLocalHTTPSManager creates a LocalHTTPSManager storing its CA material under dir.
// When dir is empty, the CA lives in memory only (useful for tests).
func NewLocalHTTPSManager(dir string) *LocalHTTPSManager {
	return &LocalHTTPSManager{dir: dir}
}

// HasCA reports whether a local Root CA certificate already exists on disk or in memory.
func (m *LocalHTTPSManager) HasCA() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caCert != nil && m.caKey != nil {
		return true
	}
	if m.dir == "" {
		return false
	}
	_, errC := os.Stat(filepath.Join(m.dir, localCACertFileName))
	_, errK := os.Stat(filepath.Join(m.dir, localCAKeyFileName))
	return errC == nil && errK == nil
}

// EnsureCA loads the existing Local Root CA or generates a new ECDSA P-256 CA.
func (m *LocalHTTPSManager) EnsureCA(hostID string) error {
	if m == nil {
		return errors.New("server: local HTTPS manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureCALocked(hostID)
}

func (m *LocalHTTPSManager) ensureCALocked(hostID string) error {
	now := time.Now().UTC()
	if m.caCert != nil && m.caKey != nil && now.Before(m.caCert.NotAfter.Add(-24*time.Hour)) {
		return nil
	}

	if m.dir != "" {
		certPath := filepath.Join(m.dir, localCACertFileName)
		keyPath := filepath.Join(m.dir, localCAKeyFileName)
		if certPEM, errC := os.ReadFile(certPath); errC == nil {
			if keyPEM, errK := os.ReadFile(keyPath); errK == nil {
				if cert, key, parseErr := parseLocalCA(certPEM, keyPEM); parseErr == nil && now.Before(cert.NotAfter.Add(-24*time.Hour)) {
					m.caCert = cert
					m.caKey = key
					m.caPEM = certPEM
					sum := sha256.Sum256(cert.Raw)
					m.caSHA256 = hex.EncodeToString(sum[:])
					return nil
				}
			}
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("local_https: generate CA key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("local_https: generate CA serial: %w", err)
	}
	cnSuffix := "Local"
	if len(hostID) >= 8 {
		cnSuffix = strings.ToUpper(hostID[:8])
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Flitnix Local Root CA (" + cnSuffix + ")",
			Organization: []string{"Flitnix Local Network"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(localCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("local_https: create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("local_https: parse CA cert: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("local_https: marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if m.dir != "" {
		if err := os.MkdirAll(m.dir, 0o700); err != nil {
			return fmt.Errorf("local_https: mkdir CA dir: %w", err)
		}
		if err := writeAtomicFile(filepath.Join(m.dir, localCAKeyFileName), keyPEM, 0o600); err != nil {
			return fmt.Errorf("local_https: write CA key: %w", err)
		}
		if err := writeAtomicFile(filepath.Join(m.dir, localCACertFileName), certPEM, 0o644); err != nil {
			return fmt.Errorf("local_https: write CA cert: %w", err)
		}
	}

	m.caCert = cert
	m.caKey = key
	m.caPEM = certPEM
	sum := sha256.Sum256(cert.Raw)
	m.caSHA256 = hex.EncodeToString(sum[:])
	m.leafCert = nil
	m.leafKey = ""
	return nil
}

func parseLocalCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cBlock, _ := pem.Decode(certPEM)
	if cBlock == nil {
		return nil, nil, errors.New("invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(cBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA {
		return nil, nil, errors.New("certificate is not a CA")
	}
	kBlock, _ := pem.Decode(keyPEM)
	if kBlock == nil {
		return nil, nil, errors.New("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(kBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func writeAtomicFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LeafCertificate returns a Local-CA-signed ECDSA P-256 TLS certificate bound
// to listenIP, localhost, 127.0.0.1, ::1, and the host's .local mDNS hostname.
func (m *LocalHTTPSManager) LeafCertificate(hostID, listenIP string) (tls.Certificate, []string, error) {
	if m == nil {
		return tls.Certificate{}, nil, errors.New("server: local HTTPS manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureCALocked(hostID); err != nil {
		return tls.Certificate{}, nil, err
	}

	cleanIP := strings.Trim(strings.TrimSpace(listenIP), "[]")
	cacheKey := strings.ToLower(hostID) + "|" + cleanIP
	now := time.Now().UTC()
	if m.leafCert != nil && m.leafKey == cacheKey && now.Before(m.leafExpires.Add(-24*time.Hour)) {
		return *m.leafCert, sansFromCert(m.leafCert.Leaf), nil
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("local_https: generate leaf key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("local_https: generate leaf serial: %w", err)
	}

	dnsNames := []string{"localhost"}
	if mdns := LocalMDNSHost(hostID); mdns != "" {
		dnsNames = append(dnsNames, mdns)
	}
	ipAddrs := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if parsed := net.ParseIP(cleanIP); parsed != nil && !parsed.IsUnspecified() && !parsed.IsLoopback() {
		ipAddrs = append(ipAddrs, parsed)
	}

	cn := "localhost"
	if cleanIP != "" {
		cn = cleanIP
	} else if mdns := LocalMDNSHost(hostID); mdns != "" {
		cn = mdns
	}

	expires := now.Add(localLeafValidity)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"Flitnix Local HTTPS"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              expires,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddrs,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.caCert, &leafKey.PublicKey, m.caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("local_https: sign leaf cert: %w", err)
	}
	leafParsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	tlsCert := tls.Certificate{
		Certificate: [][]byte{der, m.caCert.Raw},
		PrivateKey:  leafKey,
		Leaf:        leafParsed,
	}
	m.leafCert = &tlsCert
	m.leafKey = cacheKey
	m.leafExpires = expires
	return tlsCert, sansFromCert(leafParsed), nil
}

func sansFromCert(c *x509.Certificate) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.DNSNames)+len(c.IPAddresses))
	out = append(out, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

// CACertPEM returns the public Local Root CA PEM (never the private key).
func (m *LocalHTTPSManager) CACertPEM(hostID string) ([]byte, string, error) {
	if m == nil {
		return nil, "", errors.New("server: local HTTPS manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureCALocked(hostID); err != nil {
		return nil, "", err
	}
	cp := make([]byte, len(m.caPEM))
	copy(cp, m.caPEM)
	return cp, m.caSHA256, nil
}

// CASHA256 returns the hex SHA-256 fingerprint of the active Local Root CA if present.
func (m *LocalHTTPSManager) CASHA256() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caSHA256
}

// CACertPath returns the on-disk path of the CA certificate file if persisted.
func (m *LocalHTTPSManager) CACertPath() string {
	if m == nil || m.dir == "" {
		return ""
	}
	return filepath.Join(m.dir, localCACertFileName)
}

// RemoveCA removes the Local Root CA key and certificate from memory and disk.
func (m *LocalHTTPSManager) RemoveCA() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caCert = nil
	m.caKey = nil
	m.caPEM = nil
	m.caSHA256 = ""
	m.leafCert = nil
	m.leafKey = ""
	m.leafExpires = time.Time{}

	if m.dir != "" {
		_ = os.Remove(filepath.Join(m.dir, localCACertFileName))
		_ = os.Remove(filepath.Join(m.dir, localCAKeyFileName))
	}
	return nil
}

// RegenerateLeaf invalidates cached leaf certificates so the next bind/request re-issues.
func (m *LocalHTTPSManager) RegenerateLeaf() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.leafCert = nil
	m.leafKey = ""
	m.leafExpires = time.Time{}
	m.mu.Unlock()
}

// HostIDForCA returns the host ID prefix if needed by callers.
func HostCertForIdentity(id *devices.HostIdentity) (tls.Certificate, error) {
	return NewHostIdentityCertificate(id)
}
