// Package runnertls: TLS протокола runner'ов (change add-runner-identity):
// конфигурация сервера с клиентскими сертификатами (mTLS), конфигурация
// клиента runner'а и идентичность runner'а из сертификата. Общий для
// rivetd, rivet-runner и тестов, чтобы обе стороны собирались одинаково.
package runnertls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// ServerConfig: TLS сервера протокола runner'ов. clientCAFile непустой
// включает mTLS: соединение без сертификата, подписанного одним из этих
// корней, не проходит рукопожатие.
func ServerConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("сертификат сервера: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if clientCAFile != "" {
		pool, err := loadPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("корни клиентских сертификатов: %w", err)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ClientConfig: TLS runner'а. caFile: корень сервера (пусто, тогда системные
// корни). certFile и keyFile задаются вместе: клиентский сертификат
// runner'а. Ключ принимается только с правами 0600. Сертификат
// перечитывается при каждом рукопожатии, чтобы ротация не требовала
// перезапуска; если чтение не удалось, используется последний
// загруженный. Возвращает и сам сертификат для проверки идентичности.
func ClientConfig(caFile, certFile, keyFile string) (*tls.Config, *x509.Certificate, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pool, err := loadPool(caFile)
		if err != nil {
			return nil, nil, fmt.Errorf("tls-ca: %w", err)
		}
		cfg.RootCAs = pool
	}
	if (certFile == "") != (keyFile == "") {
		return nil, nil, errors.New("клиентский сертификат и ключ задаются вместе (-tls-cert и -tls-key)")
	}
	if certFile == "" {
		return cfg, nil, nil
	}
	ld := &certLoader{certFile: certFile, keyFile: keyFile}
	cert, err := ld.load()
	if err != nil {
		return nil, nil, err
	}
	ld.last = cert
	cfg.GetClientCertificate = ld.get
	return cfg, cert.Leaf, nil
}

// CheckKeyFile: ключ клиентского сертификата доступен только владельцу.
func CheckKeyFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("tls-key: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("tls-key: файл %s доступен не только владельцу (%v), нужны права 0600", path, st.Mode().Perm())
	}
	return nil
}

type certLoader struct {
	certFile, keyFile string
	mu                sync.Mutex
	last              *tls.Certificate
}

func (l *certLoader) load() (*tls.Certificate, error) {
	if err := CheckKeyFile(l.keyFile); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(l.certFile, l.keyFile)
	if err != nil {
		return nil, fmt.Errorf("клиентский сертификат: %w", err)
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, fmt.Errorf("клиентский сертификат: %w", err)
		}
	}
	return &cert, nil
}

func (l *certLoader) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cert, err := l.load()
	if err != nil {
		slog.Warn("клиентский сертификат не перечитан, используется прежний", "err", err)
		return l.last, nil
	}
	l.last = cert
	return cert, nil
}

func loadPool(file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("в %s нет PEM-сертификатов", file)
	}
	return pool, nil
}

// Identities: идентичности runner'а из сертификата, Common Name и
// DNS-имена из SAN.
func Identities(cert *x509.Certificate) []string {
	if cert == nil {
		return nil
	}
	var out []string
	if cert.Subject.CommonName != "" {
		out = append(out, cert.Subject.CommonName)
	}
	for _, d := range cert.DNSNames {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// Matches: сертификат выдан для runner'а id (точное совпадение с CN или
// DNS-именем из SAN).
func Matches(cert *x509.Certificate, id string) bool {
	return id != "" && slices.Contains(Identities(cert), id)
}

// PeerCertificate: проверенный клиентский сертификат соединения, лист
// цепочки, которую построил TLS. nil, если соединение без mTLS.
func PeerCertificate(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 || len(ti.State.VerifiedChains[0]) == 0 {
		return nil
	}
	return ti.State.VerifiedChains[0][0]
}

// LogHandshakeFailures оборачивает учётные данные сервера: неудачное
// TLS-рукопожатие (нет клиентского сертификата, чужой корень, истёк)
// пишется в журнал rivetd не чаще раза в handshakeLogWindow на адрес.
// gRPC сам такие отказы в журнал приложения не пишет, а в event log им не
// место: у них нет идентичности, и сканер порта засорил бы аудит.
func LogHandshakeFailures(c credentials.TransportCredentials) credentials.TransportCredentials {
	return &loggingCreds{TransportCredentials: c, lim: &addrLimiter{seen: map[string]time.Time{}}}
}

const (
	handshakeLogWindow = time.Minute
	handshakeLogMax    = 256
)

type loggingCreds struct {
	credentials.TransportCredentials
	lim *addrLimiter
}

func (c *loggingCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	out, ai, err := c.TransportCredentials.ServerHandshake(conn)
	if err != nil {
		host := conn.RemoteAddr().String()
		if h, _, e := net.SplitHostPort(host); e == nil {
			host = h
		}
		if c.lim.allow(host, time.Now()) {
			slog.Warn("TLS-рукопожатие с runner'ом не прошло", "peer", conn.RemoteAddr().String(), "err", err)
		}
	}
	return out, ai, err
}

func (c *loggingCreds) Clone() credentials.TransportCredentials {
	return &loggingCreds{TransportCredentials: c.TransportCredentials.Clone(), lim: c.lim}
}

type addrLimiter struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (l *addrLimiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.seen[addr]; ok && now.Sub(at) < handshakeLogWindow {
		return false
	}
	if len(l.seen) >= handshakeLogMax {
		for k, at := range l.seen {
			if now.Sub(at) >= handshakeLogWindow {
				delete(l.seen, k)
			}
		}
		if len(l.seen) >= handshakeLogMax {
			return false
		}
	}
	l.seen[addr] = now
	return true
}
