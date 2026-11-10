package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// configRepo — bare-репозиторий конфигурации с файлом версии на main.
func configRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "gitops.git")
	gitT(t, tmp, "init", "-q", "--bare", "-b", "main", bare)
	work := filepath.Join(tmp, "work")
	gitT(t, tmp, "clone", "-q", bare, work)
	if err := os.MkdirAll(filepath.Join(work, "envs", "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "envs", "prod", "version"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "-A")
	gitT(t, work, "commit", "-q", "-m", "v1")
	gitT(t, work, "push", "-q", "origin", "main")
	return bare
}

// Окружение отвечает файлом версии из ветки конфигурации, без файла — 404,
// на некорректный путь — 400, на запись — 405.
func TestHandler(t *testing.T) {
	srv := httptest.NewServer(newHandler(configRepo(t), "main"))
	defer srv.Close()

	cases := []struct {
		method, path string
		code         int
		body         string
	}{
		{http.MethodGet, "/envs/prod/version", http.StatusOK, "v1\n"},
		{http.MethodGet, "/envs/stage/version", http.StatusNotFound, ""},
		{http.MethodGet, "/envs/prod", http.StatusNotFound, ""},
		{http.MethodGet, "/envs/prod/../prod/version", http.StatusBadRequest, ""},
		{http.MethodGet, "/-x", http.StatusBadRequest, ""},
		{http.MethodGet, "/envs//version", http.StatusBadRequest, ""},
		{http.MethodGet, "/", http.StatusBadRequest, ""},
		{http.MethodPost, "/envs/prod/version", http.StatusMethodNotAllowed, ""},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, srv.URL+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Путь с «..» клиент нормализовал бы сам: отправляем как есть.
		req.URL.Opaque = c.path
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Fatalf("%s %s: код %d, ожидался %d (%s)", c.method, c.path, resp.StatusCode, c.code, buf)
		}
		if c.body != "" && buf.String() != c.body {
			t.Fatalf("%s %s: тело %q, ожидалось %q", c.method, c.path, buf, c.body)
		}
	}
}

// Адрес пишется в файл целиком, повторная запись заменяет прежний.
func TestWriteAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gitops-env.addr")
	for _, addr := range []string{"127.0.0.1:1\n", "127.0.0.1:2\n"} {
		if err := writeAtomic(p, addr); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(p); err != nil || string(raw) != addr {
			t.Fatalf("файл адреса: %q %v", raw, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Fatalf("временные файлы остались: %d записей", len(entries))
	}
}
