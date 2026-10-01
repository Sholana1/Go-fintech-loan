package grpcx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// ServerTLS loads a server certificate and the CA that signs client
// certificates, and requires verified client certificates.
func ServerTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, pool, err := loadKeyPairAndCA(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLS loads a client certificate and the CA that signs server
// certificates. serverName must match a DNS SAN of the server certificate.
func ClientTLS(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	cert, pool, err := loadKeyPairAndCA(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadKeyPairAndCA(certFile, keyFile, caFile string) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("load key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("CA file contains no certificates")
	}
	return cert, pool, nil
}
