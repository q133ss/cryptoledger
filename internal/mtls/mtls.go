// Package mtls — генерация dev-сертификатов и настройка взаимного TLS.
//
// В проде это место занимают cert-manager и внутренний CA: сертификаты
// выдаются подом, ротируются автоматически, а NetworkPolicy не пускает к
// Ledger никого, кроме разрешённых сервисов. Здесь тот же протокол (mTLS),
// но сертификаты создаются на лету, чтобы демо запускалось одной командой.
package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Files — пути к материалам, которые нужны серверу и клиенту.
type Files struct {
	CAPEM      string
	ServerCert string
	ServerKey  string
	ClientCert string
	ClientKey  string
}

// EnsureDevCerts создаёт CA, серверный и клиентский сертификаты, если их ещё нет.
// Файлы пишутся с правами 0600.
func EnsureDevCerts(dir string) (Files, error) {
	f := Files{
		CAPEM:      filepath.Join(dir, "ca.pem"),
		ServerCert: filepath.Join(dir, "server.crt"),
		ServerKey:  filepath.Join(dir, "server.key"),
		ClientCert: filepath.Join(dir, "client.crt"),
		ClientKey:  filepath.Join(dir, "client.key"),
	}

	if allExist(f.CAPEM, f.ServerCert, f.ServerKey, f.ClientCert, f.ClientKey) {
		return f, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Files{}, fmt.Errorf("mtls: create cert dir: %w", err)
	}

	caCert, caKey, caDER, err := newCA()
	if err != nil {
		return Files{}, err
	}

	serverCert, serverKey, err := issue(caCert, caKey, "ledger-server", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return Files{}, err
	}
	clientCert, clientKey, err := issue(caCert, caKey, "ledger-demo-client", nil, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return Files{}, err
	}

	if err := writePEM(f.CAPEM, "CERTIFICATE", caDER); err != nil {
		return Files{}, err
	}
	if err := writeCertKey(f.ServerCert, f.ServerKey, serverCert, serverKey); err != nil {
		return Files{}, err
	}
	if err := writeCertKey(f.ClientCert, f.ClientKey, clientCert, clientKey); err != nil {
		return Files{}, err
	}
	return f, nil
}

// ServerTLS требует клиентский сертификат: без него TLS-хендшейк не состоится.
func ServerTLS(f Files) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.ServerCert, f.ServerKey)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server keypair: %w", err)
	}
	pool, err := loadPool(f.CAPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

// ClientTLS проверяет сервер по внутреннему CA и предъявляет клиентский сертификат.
func ClientTLS(f Files) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.ClientCert, f.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client keypair: %w", err)
	}
	pool, err := loadPool(f.CAPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   "localhost",
	}, nil
}

func newCA() (*x509.Certificate, ed25519.PrivateKey, []byte, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: ca key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ledger-demo-ca", Organization: []string{"ledger-demo"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: create ca: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mtls: parse ca: %w", err)
	}
	return cert, priv, der, nil
}

func issue(ca *x509.Certificate, caKey ed25519.PrivateKey, cn string, dns []string, ips []net.IP, usage []x509.ExtKeyUsage) ([]byte, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: issue key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"ledger-demo"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("mtls: issue cert %s: %w", cn, err)
	}
	return der, priv, nil
}

func writeCertKey(certPath, keyPath string, certDER []byte, key ed25519.PrivateKey) error {
	if err := writePEM(certPath, "CERTIFICATE", certDER); err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("mtls: marshal key: %w", err)
	}
	return writePEM(keyPath, "PRIVATE KEY", der)
}

func writePEM(path, blockType string, der []byte) error {
	block := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		return fmt.Errorf("mtls: write %s: %w", path, err)
	}
	return nil
}

func loadPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mtls: read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("mtls: no certificates in %s", path)
	}
	return pool, nil
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("mtls: serial: %w", err)
	}
	return n, nil
}

func allExist(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}
