// Package devcert issues short-lived certificates for local development and
// tests, so that mutual TLS and workload identity are exercised everywhere
// instead of being switched off outside production.
//
// It must never be used to issue production certificates: keys are generated
// in-process and the CA key is kept in memory or written to the developer's
// machine. Production identities come from the platform's certificate
// authority (an unresolved infrastructure decision recorded in the ADRs).
package devcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Authority is an in-memory development CA.
type Authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

// NewAuthority creates a development CA valid for 30 days.
func NewAuthority() (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "bankplatform development CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// Pool returns a certificate pool containing only this CA.
func (a *Authority) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.cert)
	return p
}

// Leaf is an issued certificate with its PEM encodings.
type Leaf struct {
	TLS     tls.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

// Issue creates a certificate usable as both server and client certificate
// for the named workload. identity is the URI SAN (see grpcx.WorkloadURI).
func (a *Authority) Issue(identity *url.URL, dnsNames ...string) (*Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: identity.Path},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{identity},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &Leaf{TLS: pair, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// ServerConfig returns a TLS config for a gRPC server that requires client
// certificates signed by this CA.
func (a *Authority) ServerConfig(leaf *Leaf) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{leaf.TLS},
		ClientCAs:    a.Pool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// ClientConfig returns a TLS config for a client presenting leaf.
func (a *Authority) ClientConfig(leaf *Leaf, serverName string) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{leaf.TLS},
		RootCAs:      a.Pool(),
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}
}

// WriteFiles writes ca.pem and, per workload, <name>.pem and <name>-key.pem
// into dir with owner-only permissions on keys.
func (a *Authority) WriteFiles(dir string, leaves map[string]*Leaf) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), a.pem, 0o644); err != nil {
		return err
	}
	for name, leaf := range leaves {
		if err := os.WriteFile(filepath.Join(dir, name+".pem"), leaf.CertPEM, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-key.pem"), leaf.KeyPEM, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		panic(fmt.Sprintf("devcert: read random serial: %v", err))
	}
	return n
}
