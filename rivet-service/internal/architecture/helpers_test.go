package architecture

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// update перезаписывает golden-файлы: go test ./internal/architecture -update.
var update = flag.Bool("update", false, "перезаписать golden-файлы")

const shopDir = "testdata/shop"

// shopFS — эталонное описание в памяти с заменами файлов. Пустая строка в
// overrides удаляет файл.
func shopFS(t *testing.T, overrides map[string]string) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	err := fs.WalkDir(os.DirFS(shopDir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(shopDir, p))
		if err != nil {
			return err
		}
		m[p] = &fstest.MapFile{Data: raw}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, content := range overrides {
		if content == "" {
			delete(m, p)
			continue
		}
		m[p] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// minimalFS — минимальное описание: манифест и файлы из files.
func minimalFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{ManifestFile: {Data: []byte("format: rivet.architecture/v1\nsystem: Тест\n")}}
	for p, content := range files {
		m[p] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func mustValidate(t *testing.T, fsys fs.FS) Defects {
	t.Helper()
	ds, err := Validate(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func rules(ds Defects) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Rule
	}
	return out
}

// find возвращает дефект по правилу и месту или проваливает тест.
func find(t *testing.T, ds Defects, rule, file, loc string) Defect {
	t.Helper()
	for _, d := range ds {
		if d.Rule == rule && d.File == file && d.Location == loc {
			return d
		}
	}
	t.Fatalf("нет дефекта %s в %s %s, есть:\n%s", rule, file, loc, dump(ds))
	return Defect{}
}

func dump(ds Defects) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString("  " + d.String() + "\n")
	}
	return b.String()
}

func noErrors(t *testing.T, ds Defects) {
	t.Helper()
	if ds.HasErrors() {
		t.Fatalf("ожидали отсутствие ошибок:\n%s", dump(ds))
	}
}
