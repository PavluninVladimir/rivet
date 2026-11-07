// Package tlstest: сертификаты для тестов протокола runner'ов
// (add-runner-identity): корень и выпущенные им сертификаты сервера и
// runner'ов, сгенерированные crypto/x509 и записанные в каталог теста.
// Только тестовая инфраструктура, в сборку продукта не входит.
package tlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA: корень, которым тест выпускает сертификаты.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
	// File: PEM корня в каталоге теста.
	File string
}

// NewCA создаёт корень с именем name и пишет его в каталог теста.
func NewCA(t testing.TB, name string) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ca := &CA{cert: cert, key: key, dir: dir, File: filepath.Join(dir, name+"-ca.pem")}
	writePEM(t, ca.File, "CERTIFICATE", der, 0o644)
	return ca
}

// Leaf: параметры выпускаемого сертификата.
type Leaf struct {
	CN       string
	DNS      []string
	IPs      []net.IP
	Server   bool
	NotAfter time.Time // ноль означает сутки от текущего момента
}

// Issue выпускает сертификат и пишет его и ключ (0600) в каталог теста.
func (ca *CA) Issue(t testing.TB, name string, l Leaf) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := l.NotAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(24 * time.Hour)
	}
	usage := x509.ExtKeyUsageClientAuth
	if l.Server {
		usage = x509.ExtKeyUsageServerAuth
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: l.CN},
		DNSNames:     l.DNS,
		IPAddresses:  l.IPs,
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(ca.dir, name+".pem")
	keyFile = filepath.Join(ca.dir, name+"-key.pem")
	writePEM(t, certFile, "CERTIFICATE", der, 0o644)
	writePEM(t, keyFile, "EC PRIVATE KEY", kder, 0o600)
	return certFile, keyFile
}

// Server выпускает сертификат сервера для localhost и 127.0.0.1.
func (ca *CA) Server(t testing.TB) (certFile, keyFile string) {
	t.Helper()
	return ca.Issue(t, "server", Leaf{CN: "rivetd", DNS: []string{"localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Server: true})
}

func writePEM(t testing.TB, path, typ string, der []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
