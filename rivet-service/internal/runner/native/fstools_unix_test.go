//go:build unix

package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// within — вызов инструмента обязан вернуться быстро: зависание на FIFO
// или огромном файле останавливало бы всю сессию.
func within(t *testing.T, d time.Duration, f func() toolOutput) toolOutput {
	t.Helper()
	ch := make(chan toolOutput, 1)
	go func() { ch <- f() }()
	select {
	case out := <-ch:
		return out
	case <-time.After(d):
		t.Fatalf("инструмент не вернулся за %s", d)
		return toolOutput{}
	}
}

// Не обычные файлы (FIFO) не читаются и не правятся: инструмент отвечает
// ошибкой сразу, search их пропускает.
func TestFileToolsRejectFIFO(t *testing.T) {
	root, _, ft := testRepo(t)
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	mustWrite(t, filepath.Join(root, "found.txt"), "needle\n")
	out := within(t, 3*time.Second, func() toolOutput { return ft.readFile(bg, args(t, map[string]any{"path": "pipe"})) })
	if !out.isErr || !strings.Contains(out.text, "не обычный файл") {
		t.Fatalf("read_file FIFO: %q", out.text)
	}
	out = within(t, 3*time.Second, func() toolOutput {
		return ft.editFile(bg, args(t, map[string]any{"path": "pipe", "old": "a", "new": "b"}))
	})
	if !out.isErr {
		t.Fatalf("edit_file FIFO: %q", out.text)
	}
	out = within(t, 3*time.Second, func() toolOutput { return ft.writeFile(bg, args(t, map[string]any{"path": "pipe", "content": "x"})) })
	if !out.isErr {
		t.Fatalf("write_file FIFO: %q", out.text)
	}
	out = within(t, 3*time.Second, func() toolOutput { return ft.search(bg, args(t, map[string]any{"pattern": "needle"})) })
	if out.isErr || !strings.Contains(out.text, "found.txt:1: needle") {
		t.Fatalf("search рядом с FIFO: %q", out.text)
	}
}

// Длинная строка показывается началом с пометкой, вывод в пределах лимита.
func TestReadFileLongLine(t *testing.T) {
	root, _, ft := testRepo(t)
	mustWrite(t, filepath.Join(root, "min.js"), "start\n"+strings.Repeat("x", 200_000)+"\nend\n")
	out := ft.readFile(bg, args(t, map[string]any{"path": "min.js"}))
	if out.isErr || !strings.Contains(out.text, "строка обрезана: 200001 байт") {
		t.Fatalf("пометка обрезки: %q…", clip(out.text, 200))
	}
	if !strings.Contains(out.text, "     3\tend") || len(out.text) > readMaxBytes {
		t.Fatalf("строки после длинной и лимит: %d байт", len(out.text))
	}
}

// Большой файл читается частью до лимита, быстро и без чтения целиком;
// правка файла больше предела отклоняется до чтения.
func TestLargeFileLimits(t *testing.T) {
	root, _, ft := testRepo(t)
	big := filepath.Join(root, "big.log")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("y", 99) + "\n"
	for i := 0; i < 200_000; i++ { // 20 МБ — больше readCountLimit
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	out := within(t, 5*time.Second, func() toolOutput { return ft.readFile(bg, args(t, map[string]any{"path": "big.log"})) })
	if out.isErr || !strings.Contains(out.text, "файл 20000000 байт, продолжи с offset=") {
		t.Fatalf("часть большого файла: %q", out.text[len(out.text)-200:])
	}
	out = ft.editFile(bg, args(t, map[string]any{"path": "big.log", "old": "y", "new": "z"}))
	if !out.isErr || !strings.Contains(out.text, "больше предела правки") {
		t.Fatalf("правка большого файла: %q", out.text)
	}
	// Отменённая сессия не дочитывает файл.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := ft.readFile(ctx, args(t, map[string]any{"path": "big.log"})); !out.isErr {
		t.Fatal("чтение после отмены")
	}
}

// search не обходит каталоги зависимостей и окружений (если путь поиска не
// указан внутри них явно), останавливается по отмене.
func TestSearchSkipsDependencyDirs(t *testing.T) {
	root, _, ft := testRepo(t)
	for _, d := range []string{"node_modules/lib", "vendor/pkg", ".venv/lib", "web/node_modules/x"} {
		mustWrite(t, filepath.Join(root, d, "f.js"), "needle\n")
	}
	mustWrite(t, filepath.Join(root, "app.js"), "needle\n")
	out := ft.search(bg, args(t, map[string]any{"pattern": "needle"}))
	if out.text != "app.js:1: needle" {
		t.Fatalf("каталоги зависимостей не ищутся: %q", out.text)
	}
	out = ft.search(bg, args(t, map[string]any{"pattern": "needle", "path": "node_modules"}))
	if !strings.Contains(out.text, "node_modules/lib/f.js:1: needle") {
		t.Fatalf("явный путь внутрь: %q", out.text)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := ft.search(ctx, args(t, map[string]any{"pattern": "needle"})); !out.isErr {
		t.Fatal("поиск после отмены")
	}
}

// Запись через висячий симлинк наружу и в .git в другом регистре
// отклоняется, файл снаружи не появляется, служебные файлы git целы.
// Права существующего файла сохраняются.
func TestWriteBoundariesHardening(t *testing.T) {
	root, outside, ft := testRepo(t)
	if err := os.Symlink(filepath.Join(outside, "created-by-agent.txt"), filepath.Join(root, "dangling.txt")); err != nil {
		t.Fatal(err)
	}
	if out := ft.writeFile(bg, args(t, map[string]any{"path": "dangling.txt", "content": "x"})); !out.isErr {
		t.Fatal("запись через висячий симлинк должна быть отклонена")
	}
	if _, err := os.Stat(filepath.Join(outside, "created-by-agent.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("файл снаружи рабочей копии не должен появиться")
	}
	for _, p := range []string{".GIT/config", ".Git/hooks/pre-commit", "sub/.GiT/hooks/x"} {
		if out := ft.writeFile(bg, args(t, map[string]any{"path": p, "content": "x"})); !out.isErr {
			t.Errorf("запись %q должна быть отклонена", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".git", "config")); string(b) != "[core]\n" {
		t.Fatalf(".git/config изменён: %q", b)
	}
	mustWrite(t, filepath.Join(root, "private.env"), "a=1\n")
	if err := os.Chmod(filepath.Join(root, "private.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := ft.editFile(bg, args(t, map[string]any{"path": "private.env", "old": "a=1", "new": "a=2"})); out.isErr {
		t.Fatal(out.text)
	}
	if st, _ := os.Stat(filepath.Join(root, "private.env")); st.Mode().Perm() != 0o600 {
		t.Fatalf("права файла: %v", st.Mode().Perm())
	}
}
