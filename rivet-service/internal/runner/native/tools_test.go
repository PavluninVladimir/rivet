package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bg — контекст файловых инструментов в тестах.
var bg = context.Background()

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func args(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testRepo — рабочая копия с .git, src/ и каталогом снаружи с «секретом».
func testRepo(t *testing.T) (root, outside string, ft *fsTools) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "ws")
	outside = filepath.Join(base, "outside")
	mustWrite(t, filepath.Join(root, "src", "main.js"), "const a = 1\nconst b = 2\nconst a2 = 1\n")
	mustWrite(t, filepath.Join(root, ".git", "config"), "[core]\n")
	mustWrite(t, filepath.Join(outside, "secret.txt"), "ключ\n")
	ws, err := newWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, outside, &fsTools{ws: ws}
}

// Границы рабочей копии (спека native-agent «Инструменты и границы рабочей
// копии»): выход через .., абсолютные пути снаружи, симлинки наружу и запись
// в .git отклоняются ошибкой инструмента.
func TestPathBoundaries(t *testing.T) {
	root, outside, ft := testRepo(t)
	if err := os.Symlink(outside, filepath.Join(root, "outlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "secret-link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	read := []struct {
		path string
		ok   bool
	}{
		{"src/main.js", true},
		{filepath.Join(root, "src/main.js"), true}, // абсолютный путь внутри
		{"../outside/secret.txt", false},
		{"../../.ssh/id_rsa", false},
		{filepath.Join(outside, "secret.txt"), false},
		{"/etc/passwd", false},
		{"outlink/secret.txt", false},
		{"secret-link.txt", false},
		{"etc/hosts", false},
		{"src/../../outside/secret.txt", false},
		{".git/config", true}, // .git читать можно
	}
	for _, c := range read {
		out := ft.readFile(bg, args(t, map[string]any{"path": c.path}))
		if c.ok == out.isErr {
			t.Errorf("read_file %q: ok=%v, получено %q", c.path, c.ok, out.text)
		}
		if !c.ok && strings.Contains(out.text, "ключ") {
			t.Errorf("read_file %q прочитал файл снаружи", c.path)
		}
	}
	write := []string{".git/hooks/pre-commit", ".git/config", "sub/.git/x", "outlink/new.txt", "../escape.txt", "/tmp/escape.txt", "etc/rivet"}
	for _, p := range write {
		out := ft.writeFile(bg, args(t, map[string]any{"path": p, "content": "x"}))
		if !out.isErr {
			t.Errorf("write_file %q должен быть отклонён", p)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "pre-commit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("хук не должен появиться")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("запись через симлинк наружу не должна пройти")
	}
	if out := ft.editFile(bg, args(t, map[string]any{"path": "secret-link.txt", "old": "ключ", "new": "x"})); !out.isErr {
		t.Fatal("правка через симлинк наружу должна быть отклонена")
	}
	// Ошибка — текст для модели, сессия продолжается.
	out := ft.readFile(bg, args(t, map[string]any{"path": "../outside/secret.txt"}))
	if !strings.Contains(out.text, "путь вне рабочей копии") {
		t.Fatalf("текст ошибки: %q", out.text)
	}
}

func TestReadFile(t *testing.T) {
	root, _, ft := testRepo(t)
	out := ft.readFile(bg, args(t, map[string]any{"path": "src/main.js"}))
	if out.isErr || !strings.Contains(out.text, "     1\tconst a = 1\n") || !strings.Contains(out.text, "     3\tconst a2 = 1") {
		t.Fatalf("строки с номерами: %q", out.text)
	}
	var lines []string
	for i := 1; i <= 2500; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	mustWrite(t, filepath.Join(root, "big.txt"), strings.Join(lines, "\n"))
	out = ft.readFile(bg, args(t, map[string]any{"path": "big.txt"}))
	if !strings.Contains(out.text, "показаны строки 1–2000 из 2500, продолжи с offset=2001") {
		t.Fatalf("обрезка по строкам: %q", out.text[len(out.text)-120:])
	}
	out = ft.readFile(bg, args(t, map[string]any{"path": "big.txt", "offset": 2001, "limit": 10}))
	if !strings.HasPrefix(out.text, "  2001\tline 2001") || !strings.Contains(out.text, "продолжи с offset=2011") {
		t.Fatalf("чтение частью: %q", out.text)
	}
	mustWrite(t, filepath.Join(root, "wide.txt"), strings.Repeat(strings.Repeat("я", 500)+"\n", 200))
	out = ft.readFile(bg, args(t, map[string]any{"path": "wide.txt"}))
	if len(out.text) > readMaxBytes+200 || !strings.Contains(out.text, "продолжи с offset=") {
		t.Fatalf("обрезка по байтам: %d", len(out.text))
	}
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{1, 0, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := ft.readFile(bg, args(t, map[string]any{"path": "bin.dat"})); !out.isErr || !strings.Contains(out.text, "двоичный") {
		t.Fatalf("двоичный файл: %q", out.text)
	}
	if out := ft.readFile(bg, args(t, map[string]any{"path": "src"})); !out.isErr || !strings.Contains(out.text, "каталог") {
		t.Fatalf("каталог: %q", out.text)
	}
	if out := ft.readFile(bg, args(t, map[string]any{"path": "nope.txt"})); !out.isErr || !strings.Contains(out.text, "не существует") {
		t.Fatalf("нет файла: %q", out.text)
	}
}

func TestListDirAndSearch(t *testing.T) {
	root, _, ft := testRepo(t)
	mustWrite(t, filepath.Join(root, "src", "deep", "x", "y.js"), "needle\n")
	mustWrite(t, filepath.Join(root, "README.md"), "needle in readme\n")
	out := ft.listDir(bg, args(t, map[string]any{}))
	if out.isErr || strings.Contains(out.text, ".git") || out.text != "README.md\nsrc/" {
		t.Fatalf("глубина 1 без .git: %q", out.text)
	}
	out = ft.listDir(bg, args(t, map[string]any{"path": "src", "depth": 3}))
	if !strings.Contains(out.text, "deep/x/y.js") || !strings.Contains(out.text, "main.js") {
		t.Fatalf("глубина 3: %q", out.text)
	}
	out = ft.search(bg, args(t, map[string]any{"pattern": "needle"}))
	if !strings.Contains(out.text, "src/deep/x/y.js:1: needle") || !strings.Contains(out.text, "README.md:1:") {
		t.Fatalf("поиск: %q", out.text)
	}
	out = ft.search(bg, args(t, map[string]any{"pattern": "needle", "glob": "*.js"}))
	if strings.Contains(out.text, "README") {
		t.Fatalf("glob: %q", out.text)
	}
	out = ft.search(bg, args(t, map[string]any{"pattern": `\[core\]`}))
	if out.text != "совпадений нет" {
		t.Fatalf(".git не ищется: %q", out.text)
	}
	if out := ft.search(bg, args(t, map[string]any{"pattern": "("})); !out.isErr {
		t.Fatal("невалидное выражение")
	}
	var many []string
	for i := 0; i < 300; i++ {
		many = append(many, "hit")
	}
	mustWrite(t, filepath.Join(root, "many.txt"), strings.Join(many, "\n"))
	if out := ft.search(bg, args(t, map[string]any{"pattern": "hit"})); !strings.Contains(out.text, "показаны первые 200 совпадений") {
		t.Fatal("лимит совпадений")
	}
}

func TestWriteAndEdit(t *testing.T) {
	root, _, ft := testRepo(t)
	out := ft.writeFile(bg, args(t, map[string]any{"path": "test/new/companies.test.js", "content": "ok\n"}))
	if out.isErr || len(out.files) != 1 || out.files[0] != "test/new/companies.test.js" {
		t.Fatalf("запись с каталогами: %+v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "test/new/companies.test.js")); string(b) != "ok\n" {
		t.Fatal("содержимое")
	}
	if out := ft.writeFile(bg, args(t, map[string]any{"path": "big", "content": strings.Repeat("x", writeMaxBytes+1)})); !out.isErr {
		t.Fatal("предел размера")
	}
	if out := ft.writeFile(bg, args(t, map[string]any{"path": "src", "content": "x"})); !out.isErr {
		t.Fatal("каталог вместо файла")
	}

	out = ft.editFile(bg, args(t, map[string]any{"path": "src/main.js", "old": "= 1", "new": "= 10"}))
	if !out.isErr || !strings.Contains(out.text, "2 раз") {
		t.Fatalf("неоднозначная правка: %q", out.text)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "src/main.js")); !strings.Contains(string(b), "const a = 1") {
		t.Fatal("файл не должен измениться")
	}
	out = ft.editFile(bg, args(t, map[string]any{"path": "src/main.js", "old": "const b = 2", "new": "const b = 3"}))
	if out.isErr || out.files[0] != "src/main.js" {
		t.Fatalf("точная правка: %+v", out)
	}
	out = ft.editFile(bg, args(t, map[string]any{"path": "src/main.js", "old": "= 1", "new": "= 10", "replace_all": true}))
	if out.isErr || !strings.Contains(out.text, ": 2") {
		t.Fatalf("replace_all: %q", out.text)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "src/main.js")); string(b) != "const a = 10\nconst b = 3\nconst a2 = 10\n" {
		t.Fatalf("итог: %q", b)
	}
	if out := ft.editFile(bg, args(t, map[string]any{"path": "src/main.js", "old": "нет такого", "new": "x"})); !out.isErr || !strings.Contains(out.text, "не найден") {
		t.Fatal("фрагмент не найден")
	}
	if out := ft.editFile(bg, args(t, map[string]any{"path": "src/main.js", "old": "", "new": "x"})); !out.isErr {
		t.Fatal("пустой фрагмент")
	}
	if out := ft.writeFile(bg, json.RawMessage(`{"path":`)); !out.isErr {
		t.Fatal("битые аргументы")
	}
}
