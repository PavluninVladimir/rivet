package runnertls

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/runnertls/tlstest"
)

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8090": true, "localhost:8090": true, "[::1]:8090": true, "127.0.0.2:8090": true,
		":8090": false, "0.0.0.0:8090": false, "[::]:8090": false, "10.0.0.5:8090": false, "rivet.example:8090": false,
	} {
		if got := LoopbackAddr(addr); got != want {
			t.Errorf("LoopbackAddr(%q) = %v, ждём %v", addr, got, want)
		}
	}
}

func TestIdentities(t *testing.T) {
	ca := tlstest.NewCA(t, "runners")
	certFile, keyFile := ca.Issue(t, "host", tlstest.Leaf{CN: "host-a", DNS: []string{"local-worker", "local-reviewer", "host-a"}})
	_, leaf, err := ClientConfig(ca.File, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := Identities(leaf); !slices.Equal(got, []string{"host-a", "local-worker", "local-reviewer"}) {
		t.Fatalf("идентичности: %v", got)
	}
	for id, want := range map[string]bool{"host-a": true, "local-worker": true, "local-tester": false, "": false, "LOCAL-WORKER": false} {
		if Matches(leaf, id) != want {
			t.Errorf("Matches(%q) != %v", id, want)
		}
	}
	if Matches(nil, "host-a") {
		t.Error("nil-сертификат совпал")
	}
}

func TestClientConfigChecks(t *testing.T) {
	ca := tlstest.NewCA(t, "runners")
	certFile, keyFile := ca.Issue(t, "r1", tlstest.Leaf{CN: "r1"})

	if _, _, err := ClientConfig(ca.File, certFile, ""); err == nil || !strings.Contains(err.Error(), "задаются вместе") {
		t.Fatalf("сертификат без ключа: %v", err)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClientConfig(ca.File, certFile, keyFile); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("ключ 0644: %v", err)
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("не сертификат\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ClientConfig(empty, certFile, keyFile); err == nil || !strings.Contains(err.Error(), "нет PEM") {
		t.Fatalf("пустой корень: %v", err)
	}
	srvCert, srvKey := ca.Server(t)
	if _, err := ServerConfig(srvCert, srvKey, empty); err == nil || !strings.Contains(err.Error(), "корни клиентских сертификатов") {
		t.Fatalf("сервер с пустым корнем клиентов: %v", err)
	}
	cfg, err := ServerConfig(srvCert, srvKey, ca.File)
	if err != nil || cfg.ClientAuth != tls.RequireAndVerifyClientCert || cfg.ClientCAs == nil {
		t.Fatalf("сервер с корнем клиентов: %v %+v", err, cfg)
	}
	if cfg, err := ServerConfig(srvCert, srvKey, ""); err != nil || cfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("сервер без mTLS: %v", err)
	}
	// Без сертификата клиента остаётся только корень сервера.
	cfg, leaf, err := ClientConfig("", "", "")
	if err != nil || leaf != nil || cfg.GetClientCertificate != nil || cfg.RootCAs != nil {
		t.Fatalf("клиент без сертификата: %v %+v", err, cfg)
	}
}

// Ротация: сертификат перечитывается при рукопожатии, а сбой чтения
// оставляет прежний.
func TestClientCertificateReload(t *testing.T) {
	ca := tlstest.NewCA(t, "runners")
	certFile, keyFile := ca.Issue(t, "r1", tlstest.Leaf{CN: "r1"})
	cfg, leaf, err := ClientConfig(ca.File, certFile, keyFile)
	if err != nil || leaf.Subject.CommonName != "r1" {
		t.Fatalf("загрузка: %v", err)
	}
	newCert, newKey := ca.Issue(t, "r1-next", tlstest.Leaf{CN: "r1", DNS: []string{"r1-rotated"}})
	for _, p := range [][2]string{{newCert, certFile}, {newKey, keyFile}} {
		if err := os.Rename(p[0], p[1]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil || !slices.Contains(got.Leaf.DNSNames, "r1-rotated") {
		t.Fatalf("сертификат не перечитан: %v", err)
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	again, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil || again != got {
		t.Fatalf("при сбое чтения ждём прежний сертификат: %v", err)
	}
}

func TestHandshakeLogLimiter(t *testing.T) {
	l := &addrLimiter{seen: map[string]time.Time{}}
	now := time.Now()
	if !l.allow("10.0.0.1", now) || l.allow("10.0.0.1", now.Add(time.Second)) {
		t.Fatal("повтор с того же адреса внутри окна должен молчать")
	}
	if !l.allow("10.0.0.2", now) {
		t.Fatal("другой адрес должен писаться")
	}
	if !l.allow("10.0.0.1", now.Add(handshakeLogWindow)) {
		t.Fatal("после окна адрес пишется снова")
	}
	for i := range handshakeLogMax {
		l.allow("h"+strings.Repeat("x", i), now)
	}
	if len(l.seen) > handshakeLogMax {
		t.Fatalf("карта выросла сверх потолка: %d", len(l.seen))
	}
}
