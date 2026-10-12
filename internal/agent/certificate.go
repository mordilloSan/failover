package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// The UI's certificate, made and renewed by the agent as LinuxIO does: self
// signed, the same across restarts and updates, so the browser's warning is
// accepted once.
const (
	certName     = "0-self-signed.cert"
	keyName      = "0-self-signed.key"
	certLifetime = 395 * 24 * time.Hour
	certRenewal  = 30 * 24 * time.Hour
)

// CertFile is where the certificate for the agent with statePath lives.
func CertFile(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), "certificates", certName)
}

// needsNew: near its end, or without one of the addresses it must name.
func needsNew(leaf *x509.Certificate, now time.Time, ips []net.IP) bool {
	if !leaf.NotAfter.After(now.Add(certRenewal)) {
		return true
	}
	return slices.ContainsFunc(ips, func(ip net.IP) bool { return !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) })
}

// loadOrCreate returns the certificate in dir, making a new one when it is
// missing, unreadable, near its end or without one of ips.
func loadOrCreate(dir string, now time.Time, ips []net.IP) (tls.Certificate, *x509.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, certName), filepath.Join(dir, keyName)
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, perr := x509.ParseCertificate(c.Certificate[0]); perr == nil && !needsNew(leaf, now, ips) {
			return c, leaf, nil
		}
	} else if fileExists(certPath) || fileExists(keyPath) {
		slog.Warn("certificado da interface ilegível: vou fazer outro", "dir", dir, "error", err)
	}
	certPEM, keyPEM, err := makeCert(now, ips)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	// Not saved (a read-only or full disk): serve it from memory all the same,
	// or the UI, and the agent with it, would not start.
	err = os.MkdirAll(dir, 0o700)
	if err == nil {
		err = writeAtomic(keyPath, keyPEM)
	}
	if err == nil {
		err = writeAtomic(certPath, certPEM)
	}
	if err != nil {
		slog.Error("guardar o certificado da interface: fica só em memória até ao próximo arranque", "dir", dir, "error", err)
	} else {
		_ = os.Chmod(certPath, 0o644) // the healthcheck and the user may read it; the key stays 0600
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	slog.Info("certificado da interface criado", "até", leaf.NotAfter.Format(time.DateOnly))
	return c, leaf, err
}

func makeCert(now time.Time, ips []net.IP) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	names := []string{"localhost"}
	if h, herr := os.Hostname(); herr == nil && h != "" && !strings.EqualFold(h, "localhost") {
		names = append(names, h, h+".local")
	}
	all := append([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, ips...)
	if addrs, aerr := net.InterfaceAddrs(); aerr == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.IsGlobalUnicast() {
				all = append(all, n.IP)
			}
		}
	}
	var uniq []net.IP
	for _, ip := range all {
		if ip != nil && !slices.ContainsFunc(uniq, ip.Equal) {
			uniq = append(uniq, ip)
		}
	}
	tmpl := x509.Certificate{
		SerialNumber: serial, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(certLifetime),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: names, IPAddresses: uniq,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// certStore hands out the certificate on each handshake and makes a new one
// when needsNew says so, so a renewal or a new TNAS address needs no restart.
type certStore struct {
	dir  string
	ips  func() []net.IP
	mu   sync.Mutex
	cur  tls.Certificate
	leaf *x509.Certificate
}

func (s *certStore) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leaf == nil || needsNew(s.leaf, time.Now(), s.ips()) {
		c, leaf, err := loadOrCreate(s.dir, time.Now(), s.ips())
		if err != nil {
			if s.leaf == nil {
				return nil, err
			}
			slog.Error("renovar o certificado da interface", "error", err) // keep serving the old one
		} else {
			s.cur, s.leaf = c, leaf
		}
	}
	return &s.cur, nil
}

// TLSConfig is the UI's TLS configuration; the certificate is ready when it returns.
func (a *Agent) TLSConfig() (*tls.Config, error) {
	s := &certStore{dir: filepath.Dir(CertFile(a.statePath)), ips: func() []net.IP {
		if ip := net.ParseIP(*a.tnasIP.Load()); ip != nil {
			return []net.IP{ip}
		}
		return nil
	}}
	if _, err := s.get(nil); err != nil {
		return nil, fmt.Errorf("certificado da interface: %w", err)
	}
	a.certs.Store(s)
	return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}, GetCertificate: s.get}, nil
}

// CertInfo is what the certificate names and when it ends, for the settings page.
func (a *Agent) CertInfo() (names []string, notAfter time.Time) {
	s := a.certs.Load()
	if s == nil {
		return nil, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leaf == nil {
		return nil, time.Time{}
	}
	names = slices.Clone(s.leaf.DNSNames)
	for _, ip := range s.leaf.IPAddresses {
		names = append(names, ip.String())
	}
	return names, s.leaf.NotAfter
}
