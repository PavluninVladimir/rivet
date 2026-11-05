//go:build linux

package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// В песочнице команда агента не читает файлы runner'а: его $HOME закрыт
// пустым tmpfs, файл токена вне $HOME — пустышкой, а кеш зависимостей под
// $HOME остаётся читаемым (только на чтение).
func TestSandboxHidesRunnerFiles(t *testing.T) {
	if !NetworkIsolationAvailable() {
		t.Skip("песочница недоступна: " + SandboxUnavailableReason())
	}
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".ssh", "id_rsa"), "SECRET-SSH-KEY\n")
	cache := filepath.Join(home, "go", "pkg", "mod")
	mustWrite(t, filepath.Join(cache, "mod.txt"), "модуль\n")
	tokenDir := t.TempDir()
	tokenFile := filepath.Join(tokenDir, "runner.token")
	mustWrite(t, tokenFile, "SECRET-TOKEN-VALUE\n")

	dir := t.TempDir()
	cmd := "cat '" + filepath.Join(home, ".ssh", "id_rsa") + "' 2>/dev/null; echo \"ssh-rc=$?\"; " +
		"cat '" + tokenFile + "' 2>/dev/null; echo \"token-rc=$?\"; " +
		"cat \"$GOMODCACHE/mod.txt\"; " +
		"(touch \"$GOMODCACHE/new\" 2>/dev/null && echo CACHE-WRITABLE) || echo CACHE-READONLY"
	sp := spec(dir, dir, cmd)
	sp.isolate = true
	sp.readOnly = []CacheDir{{Env: "GOMODCACHE", Dir: cache}}
	sp.hidePaths = []string{home, tokenFile}
	res := runCommand(context.Background(), sp)
	if res.err != nil || !res.isolated {
		t.Fatalf("песочница: %+v", res)
	}
	if strings.Contains(res.output, "SECRET-SSH-KEY") {
		t.Errorf("команда прочитала ~/.ssh/id_rsa runner'а:\n%s", res.output)
	}
	if strings.Contains(res.output, "SECRET-TOKEN-VALUE") {
		t.Errorf("команда прочитала файл токена runner'а:\n%s", res.output)
	}
	for _, want := range []string{"модуль", "CACHE-READONLY"} {
		if !strings.Contains(res.output, want) {
			t.Errorf("нет %q в выводе (кеш под $HOME должен остаться читаемым только на чтение):\n%s", want, res.output)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "new")); err == nil {
		t.Error("кеш зависимостей изменён из песочницы")
	}
}

// Файл токена с относительным путём вне $HOME скрывается: путь
// абсолютизируется от каталога runner'а, а не от рабочей копии помощника.
func TestSandboxHidesRelativeTokenPath(t *testing.T) {
	if !NetworkIsolationAvailable() {
		t.Skip("песочница недоступна: " + SandboxUnavailableReason())
	}
	tokenDir := t.TempDir()
	mustWrite(t, filepath.Join(tokenDir, "runner.token"), "REL-TOKEN-MARKER\n")
	t.Chdir(tokenDir) // runner запущен из каталога вне $HOME
	dir := t.TempDir()
	cmd := `if grep -q REL-TOKEN-MARKER '` + filepath.Join(tokenDir, "runner.token") + `' 2>/dev/null; then echo TOKEN-VISIBLE; else echo TOKEN-HIDDEN; fi`
	sp := spec(dir, dir, cmd)
	sp.isolate = true
	sp.hidePaths = []string{"runner.token"}
	res := runCommand(context.Background(), sp)
	if res.err != nil || !strings.Contains(res.output, "TOKEN-HIDDEN") {
		t.Fatalf("файл токена с относительным путём должен быть скрыт: %+v", res)
	}
}
