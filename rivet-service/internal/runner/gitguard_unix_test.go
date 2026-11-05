//go:build unix

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// .git/config — FIFO, оставленный упавшей сессией: подготовка не читает
// его (чтение FIFO без писателя висело бы), а сразу отказывает, и
// рабочая копия клонируется заново.
func TestPrepareWorkspaceGitRejectsFIFOConfig(t *testing.T) {
	_, ws := gitRemote(t)
	origin := gitT(t, ws, "config", "--get", "remote.origin.url")
	cfg := filepath.Join(ws, ".git", "config")
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(cfg, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- prepareWorkspaceGit(context.Background(), ws, origin) }()
	select {
	case err := <-done:
		if !errors.Is(err, errUnsafeClone) || !strings.Contains(err.Error(), ".git/config не обычный файл") {
			t.Fatalf("ожидается errUnsafeClone для FIFO: %v", err)
		}
	case <-time.After(5 * time.Second):
		// Писатель снимает зависший open, чтобы тест не держал горутину.
		if f, err := os.OpenFile(cfg, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("подготовка зависла на чтении FIFO")
	}
}

// Рабочая копия с FIFO вместо .git/config пересоздаётся новым клоном.
func TestWorkspaceReclonesFIFOConfig(t *testing.T) {
	_, a, as := revAgent(t)
	dir, err := a.workspace(context.Background(), as, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, ".git", "config")
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(cfg, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var steps []string
	if _, err := a.workspace(context.Background(), as, func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatalf("подготовка после FIFO: %v", err)
	}
	if !slices.ContainsFunc(steps, func(s string) bool {
		return strings.HasPrefix(s, "рабочая копия клонируется заново")
	}) {
		t.Fatalf("клон не пересоздан: %v", steps)
	}
	if st, err := os.Lstat(cfg); err != nil || !st.Mode().IsRegular() {
		t.Fatalf(".git/config после пересоздания не обычный файл: %v", err)
	}
}
