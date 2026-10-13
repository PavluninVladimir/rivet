package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/architecture"
)

// runArch выполняет команду architecture в каталоге dir и возвращает вывод.
func runArch(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	var buf bytes.Buffer
	prev := output
	output = &buf
	defer func() { output = prev }()
	err := cmdArchitecture(args)
	return buf.String(), err
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Сценарии «Создание пустого описания» и «Проверка в CI проекта».
func TestArchitectureInitValidateViews(t *testing.T) {
	dir := t.TempDir()
	out, err := runArch(t, dir, "validate")
	if err != nil || !strings.Contains(out, "описания архитектуры нет") {
		t.Fatalf("без описания: %q %v", out, err)
	}
	if _, err := runArch(t, dir, "init", "-system", "Магазин"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"architecture.yaml", "model/business.yaml", "model/application.yaml", "model/data.yaml",
		"model/technology.yaml", "processes", "changes", "views/README.md"} {
		if _, err := os.Stat(filepath.Join(dir, architecture.DefaultDir, p)); err != nil {
			t.Errorf("init не создал %s", p)
		}
	}
	for _, args := range [][]string{{"validate"}, {"views", "--check"}} {
		if out, err := runArch(t, dir, args...); err != nil {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
	if _, err := runArch(t, dir, "init"); err == nil {
		t.Error("повторный init должен отказать")
	}
}

// Сценарий «Машиночитаемый вывод».
func TestArchitectureValidateJSON(t *testing.T) {
	dir := t.TempDir()
	if _, err := runArch(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, architecture.DefaultDir, "model/application.yaml")
	if err := os.WriteFile(app, []byte("elements:\n  - {id: app.a, type: component, name: A, relations: [{type: flow, to: app.b}]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runArch(t, dir, "validate", "-json")
	if !errors.Is(err, errDefects) {
		t.Fatalf("ожидали errDefects, получили %v", err)
	}
	var ds []map[string]string
	if err := json.Unmarshal([]byte(out), &ds); err != nil {
		t.Fatalf("вывод не JSON: %v\n%s", err, out)
	}
	if len(ds) != 1 || ds[0]["severity"] != "error" || ds[0]["rule"] != "ref.unresolved" ||
		ds[0]["file"] != "model/application.yaml" || ds[0]["location"] != "app.a" {
		t.Errorf("дефекты: %v", ds)
	}
	if out, err := runArch(t, dir, "views", "-check"); !errors.Is(err, errDefects) {
		t.Errorf("views --check на описании с ошибками: %v\n%s", err, out)
	}
}

func TestArchitectureApplyCommand(t *testing.T) {
	dir := t.TempDir()
	desc := filepath.Join(dir, architecture.DefaultDir)
	copyTree(t, "../../internal/architecture/testdata/shop", desc)
	if out, err := runArch(t, dir, "validate", "-change", "refunds"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := runArch(t, dir, "apply", "-change", "refunds")
	if err != nil || !strings.Contains(out, "изменение refunds применено") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := runArch(t, dir, "views", "-check"); err != nil {
		t.Errorf("представления после apply: %v\n%s", err, out)
	}
	out, err = runArch(t, dir, "apply", "-change", "refunds")
	if !errors.Is(err, errDefects) || !strings.Contains(out, "change.missing") {
		t.Errorf("повторное применение: %v\n%s", err, out)
	}
}

func TestArchitectureViewsStale(t *testing.T) {
	dir := t.TempDir()
	if _, err := runArch(t, dir, "init"); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, architecture.DefaultDir, "model/data.yaml")
	if err := os.WriteFile(data, []byte("elements:\n  - {id: data.a, type: data-object, name: A}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runArch(t, dir, "views", "-check")
	if !errors.Is(err, errDefects) || !strings.Contains(out, "views/data.md") {
		t.Fatalf("устаревшие представления: %v\n%s", err, out)
	}
	if _, err := runArch(t, dir, "views"); err != nil {
		t.Fatal(err)
	}
	if _, err := runArch(t, dir, "views", "-check"); err != nil {
		t.Errorf("после перегенерации: %v", err)
	}
}

func TestArchitectureUsage(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		nil,
		{"draw"},
		{"apply"},
		{"validate", "-change", "../x"},
		{"validate", "extra"},
		{"views", "-unknown"},
	} {
		_, err := runArch(t, dir, args...)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Errorf("%v: ожидали ошибку использования, получили %v", args, err)
		}
	}
	if _, err := runArch(t, dir, "validate", "-dir", "custom/arch"); err != nil {
		t.Errorf("-dir без описания: %v", err)
	}
}
