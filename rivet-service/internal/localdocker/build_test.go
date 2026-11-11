package localdocker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func files(t *testing.T, data map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, s := range data {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func TestPrepareRecipes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		data        map[string]string
		want, entry string
	}{
		{"go", map[string]string{"go.mod": "module app\ngo 1.26\n", "cmd/web/main.go": "package main", "package.json": "invalid unrelated frontend manifest"}, "go", "./cmd/web"},
		{"node", map[string]string{"package.json": `{"scripts":{"start":"node server.js","build":"echo build"}}`}, "node", "npm run build"},
		{"static", map[string]string{"index.html": "hello", "Dockerfile": "FROM malicious", ".env": "secret", ".git/config": "secret", "node_modules/a": "junk"}, "static", "nginx"},
		{"spa", map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`}, "static", "/src/dist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := files(t, tt.data)
			b, e := Prepare(root, domain.LocalDockerEnvironmentConfig{ContainerPort: 8080})
			if e != nil {
				t.Fatal(e)
			}
			defer b.Close()
			if b.Recipe != tt.want {
				t.Fatal(b.Recipe)
			}
			d, e := os.ReadFile(b.Dockerfile)
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(string(d), tt.entry) || !strings.Contains(string(d), "@sha256:") || !strings.Contains(string(d), "USER 65532:65532") {
				t.Fatal(string(d))
			}
			for _, p := range []string{".env", ".git", "node_modules"} {
				if _, e := os.Stat(filepath.Join(b.Dir, p)); e == nil {
					t.Fatalf("included %s", p)
				}
			}
			if _, e := os.Stat(filepath.Join(root, ".rivet.Dockerfile")); e == nil {
				t.Fatal("modified checkout")
			}
		})
	}
}
func TestPrepareRejectsSymlinkAndAmbiguousGo(t *testing.T) {
	root := files(t, map[string]string{"index.html": "hello"})
	if e := os.Symlink("/etc/passwd", filepath.Join(root, "leak")); e != nil {
		t.Fatal(e)
	}
	if b, e := Prepare(root, domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}); e == nil {
		b.Close()
		t.Fatal("accepted symlink")
	}
	root = files(t, map[string]string{"go.mod": "module app", "cmd/a/main.go": "package main", "cmd/b/main.go": "package main"})
	if b, e := Prepare(root, domain.LocalDockerEnvironmentConfig{ContainerPort: 8080}); e == nil {
		b.Close()
		t.Fatal("accepted ambiguous entry")
	}
	outside := files(t, map[string]string{"index.html": "outside"})
	root = files(t, map[string]string{})
	if e := os.Symlink(outside, filepath.Join(root, "outside")); e != nil {
		t.Fatal(e)
	}
	if b, e := Prepare(root, domain.LocalDockerEnvironmentConfig{ContainerPort: 8080, BuildContext: "outside"}); e == nil {
		b.Close()
		t.Fatal("escaped checkout")
	}
}
