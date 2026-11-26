package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

// NewHostIdentityCertificate generates a self-signed X.509 certificate backed by id's Ed25519 keypair.
func NewHostIdentityCertificate(id *devices.HostIdentity) (tls.Certificate, error) {
	if id == nil || id.PrivateKey == "" || id.PublicKey == "" {
		return tls.Certificate{}, errors.New("server: host identity keypair missing")
	}
	privBytes, err := hex.DecodeString(id.PrivateKey)
	if err != nil || len(privBytes) != ed25519.PrivateKeySize {
		return tls.Certificate{}, errors.New("server: invalid host private key")
	}
	pubBytes, err := hex.DecodeString(id.PublicKey)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return tls.Certificate{}, errors.New("server: invalid host public key")
	}

	priv := ed25519.PrivateKey(privBytes)
	pub := ed25519.PublicKey(pubBytes)

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   id.HostID,
			Organization: []string{"Flitnix Local Peer"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{id.HostID + ".flitnix.local", "localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}

// tlsPeekListener multiplexes plain HTTP (for mobile QR browsers) and TLS 1.3
// (for desktop-to-desktop peers) on a single TCP port by inspecting the first byte.
type tlsPeekListener struct {
	inner     net.Listener
	tlsConfig *tls.Config

	ch        chan acceptResult
	closed    chan struct{}
	closeOnce sync.Once
}

type acceptResult struct {
	conn net.Conn
	err  error
}

type peekedConn struct {
	net.Conn
	buf  [1]byte
	read bool
}

func (c *peekedConn) Read(p []byte) (int, error) {
	if !c.read && len(p) > 0 {
		p[0] = c.buf[0]
		c.read = true
		if len(p) == 1 {
			return 1, nil
		}
		n, err := c.Conn.Read(p[1:])
		return 1 + n, err
	}
	return c.Conn.Read(p)
}

func newTLSPeekListener(inner net.Listener, cert tls.Certificate) net.Listener {
	l := &tlsPeekListener{
		inner: inner,
		tlsConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"http/1.1"},
		},
		ch:     make(chan acceptResult, 64),
		closed: make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *tlsPeekListener) acceptLoop() {
	for {
		c, err := l.inner.Accept()
		if err != nil {
			select {
			case l.ch <- acceptResult{err: err}:
			case <-l.closed:
			}
			return
		}
		go l.dispatchConn(c)
	}
}

func (l *tlsPeekListener) dispatchConn(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var first [1]byte
	_, err := io.ReadFull(c, first[:])
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return
	}

	wrapped := &peekedConn{Conn: c, buf: first}
	var finalConn net.Conn = wrapped
	if first[0] == 0x16 {
		// TLS ClientHello record header byte
		finalConn = tls.Server(wrapped, l.tlsConfig)
	}

	select {
	case l.ch <- acceptResult{conn: finalConn}:
	case <-l.closed:
		_ = finalConn.Close()
	}
}

func (l *tlsPeekListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case res := <-l.ch:
		return res.conn, res.err
	}
}

func (l *tlsPeekListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)
		err = l.inner.Close()
	})
	return err
}

func (l *tlsPeekListener) Addr() net.Addr {
	return l.inner.Addr()
}
