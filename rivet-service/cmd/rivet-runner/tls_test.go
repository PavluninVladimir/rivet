package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/runner"
	"github.com/PavluninVladimir/rivet/internal/runnertls/tlstest"
)

// Клиентский сертификат runner'а (add-runner-identity): флаги только
// вместе и только с -tls, ключ 0600, каталог ключа не открывается
// песочнице через -sandbox-ro. Ошибки возникают до подключения к control plane.
func TestRunnerClientCertFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ca := tlstest.NewCA(t, "runners")
	certFile, keyFile := ca.Issue(t, "r1", tlstest.Leaf{CN: "r1"})
	secrets := map[string]string{tokenEnv: "test-token"}
	workdir := filepath.Join(home, ".rivet-runner")

	err := run([]string{"-workdir", workdir, "-tls-cert", certFile, "-tls-key", keyFile}, secrets)
	if err == nil || !strings.Contains(err.Error(), "требует TLS") {
		t.Fatalf("сертификат без -tls: %v", err)
	}
	err = run([]string{"-workdir", workdir, "-tls", "-tls-cert", certFile}, secrets)
	if err == nil || !strings.Contains(err.Error(), "задаются вместе") {
		t.Fatalf("сертификат без ключа: %v", err)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-workdir", workdir, "-tls", "-tls-ca", ca.File, "-tls-cert", certFile, "-tls-key", keyFile}, secrets)
	if err == nil || !strings.Contains(err.Error(), "нужны права 0600") {
		t.Fatalf("ключ 0644: %v", err)
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	// Ключ тоже секрет runner'а, его каталог нельзя открыть песочнице.
	keyDir := filepath.Dir(keyFile)
	err = run([]string{"-workdir", workdir, "-tls", "-tls-ca", ca.File, "-tls-cert", certFile, "-tls-key", keyFile, "-sandbox-ro", keyDir}, secrets)
	if err == nil || !strings.Contains(err.Error(), "-sandbox-ro "+keyDir+": каталог открыл бы песочнице") {
		t.Fatalf("-sandbox-ro с каталогом ключа: %v", err)
	}
}

// checkClientCert возвращает абсолютный путь ключа для SecretFiles, даже
// если ключ указан относительно каталога запуска.
func TestCheckClientCertAbsoluteKey(t *testing.T) {
	ca := tlstest.NewCA(t, "runners")
	certFile, keyFile := ca.Issue(t, "r1", tlstest.Leaf{CN: "r1"})
	t.Chdir(filepath.Dir(keyFile))
	got, err := checkClientCert(runner.Config{ID: "r1", TLS: true, TLSCA: ca.File, TLSCert: certFile, TLSKey: filepath.Base(keyFile)})
	if err != nil {
		t.Fatal(err)
	}
	if got != keyFile {
		// macOS: TempDir через /var, Abs через /private/var.
		if real, _ := filepath.EvalSymlinks(keyFile); real != got {
			t.Fatalf("путь ключа %q, ждём %q", got, keyFile)
		}
	}
	if got, err := checkClientCert(runner.Config{ID: "r1"}); err != nil || got != "" {
		t.Fatalf("без сертификата: %q %v", got, err)
	}
}

// Недопустимый -id: ошибка запуска до подключения, с тем же правилом, что
// у control plane.
func TestRunnerRejectsInvalidID(t *testing.T) {
	for _, id := range []string{"local worker", strings.Repeat("r", 129), "w\n1"} {
		err := run([]string{"-id", id, "-workdir", t.TempDir()}, map[string]string{tokenEnv: "test-token"})
		if err == nil || !strings.HasPrefix(err.Error(), "-id \"") {
			t.Errorf("-id %q: %v", id, err)
		}
	}
}

// Невидимый \r из env-файла виден в тексте ошибки.
func TestRunnerInvalidIDShowsValue(t *testing.T) {
	err := run([]string{"-id", "local-worker\r", "-workdir", t.TempDir()}, map[string]string{tokenEnv: "test-token"})
	if err == nil || !strings.Contains(err.Error(), `-id "local-worker\r"`) {
		t.Fatalf("ошибка без значения: %v", err)
	}
}
