package common

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const (
	caKeyBits   = 3072
	certKeyBits = 2048

	caValidity   = 10 * 365 * 24 * time.Hour
	certValidity = 2 * 365 * 24 * time.Hour

	pemTypeCert = "CERTIFICATE"
	pemTypeKey  = "RSA PRIVATE KEY"

	permKey  os.FileMode = 0600
	permCert os.FileMode = 0644
)

// GenerateCA creates a self-signed CA certificate using RSA 3072.
// The CA is valid for 10 years and belongs to the "HubFuse" organization.
func GenerateCA() (*x509.Certificate, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, caKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"HubFuse"},
			CommonName:   "HubFuse CA",
		},
		NotBefore:             now,
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	return cert, key, nil
}

// GenerateServerCert generates a TLS server certificate signed by the given CA.
// Each element of hosts is added as a SAN — DNS name if it does not parse as an
// IP, IP address otherwise.
func GenerateServerCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, certKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"HubFuse"},
			CommonName:   "HubFuse Server",
		},
		NotBefore: now,
		NotAfter:  now.Add(certValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create server certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// SignClientCert generates a TLS client certificate for deviceID signed by the
// given CA. The deviceID is recorded in the certificate's CN field.
func SignClientCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, deviceID string) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, certKeyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("generate client key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"HubFuse"},
			CommonName:   deviceID,
		},
		NotBefore: now,
		NotAfter:  now.Add(certValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("create client certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

// LoadTLSServerConfig returns a tls.Config suitable for use as a gRPC server.
// Clients that present a certificate must have it signed by the CA. Clients
// that do not present a certificate are still accepted (so that the unauthenticated
// Join RPC can be called before a device has its client cert).
func LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath string) (*tls.Config, error) {
	serverCert, err := tls.LoadX509KeyPair(serverCertPath, serverKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}

	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caCertPath, err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("parse CA cert from %q", caCertPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// LoadTLSClientConfig returns a tls.Config suitable for use as a gRPC client
// with mutual TLS.
func LoadTLSClientConfig(caCertPath, clientCertPath, clientKeyPath string) (*tls.Config, error) {
	clientCert, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}

	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", caCertPath, err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("parse CA cert from %q", caCertPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ExtractDeviceID retrieves the device_id (stored in the client certificate's
// CN field) from the gRPC peer information in ctx. It returns an error if no
// authenticated peer is present or the certificate chain is empty.
func ExtractDeviceID(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", ErrNotAuthenticated
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", ErrNotAuthenticated
	}

	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return "", ErrNotAuthenticated
	}

	return chains[0][0].Subject.CommonName, nil
}

// SavePEM encodes data as a PEM block of the given type and writes it to path.
// Key files (pemTypeKey) are written with 0600 permissions; all others with 0644.
func SavePEM(path string, pemType string, data []byte) error {
	block := &pem.Block{Type: pemType, Bytes: data}
	pemBytes := pem.EncodeToMemory(block)

	perm := permCert
	if pemType == pemTypeKey {
		perm = permKey
	}

	if err := os.WriteFile(path, pemBytes, perm); err != nil {
		return fmt.Errorf("write PEM to %q: %w", path, err)
	}
	return nil
}

// LoadPEM reads the file at path and decodes the first PEM block it contains,
// returning the raw DER bytes.
func LoadPEM(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read PEM file %q: %w", path, err)
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %q", path)
	}
	return block.Bytes, nil
}

// SaveCertAndKey is a convenience wrapper that writes certPEM to certPath and
// keyPEM to keyPath using the appropriate file permissions.
func SaveCertAndKey(certPath, keyPath string, certPEM, keyPEM []byte) error {
	if err := os.WriteFile(certPath, certPEM, permCert); err != nil {
		return fmt.Errorf("write cert to %q: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, keyPEM, permKey); err != nil {
		return fmt.Errorf("write key to %q: %w", keyPath, err)
	}
	return nil
}

// randomSerial generates a random 128-bit certificate serial number.
func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

// EncodeCACertPEM returns the PEM-encoded certificate suitable for
// writing to disk or embedding in a TLS config. The input cert must
// be either freshly generated (with Raw populated) or parsed from an
// existing PEM.
func EncodeCACertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: cert.Raw})
}

// EncodeCAKeyPEM returns the PEM-encoded PKCS#1 private key.
func EncodeCAKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeKey, Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
