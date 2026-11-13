package localdocker

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

func TestAutoPythonDetection(t *testing.T) {
	for _, tt := range []struct {
		name, selected, context, want string
		data                          map[string]string
	}{
		{"root server before index", "auto", ".", "python", map[string]string{"app.py": "pass", "index.html": "placeholder"}},
		{"root server before static build", "auto", ".", "python", map[string]string{"app.py": "pass", "package.json": `{"scripts":{"build":"vite build"}}`}},
		{"Go retains priority", "auto", ".", "go", map[string]string{"go.mod": "module app", "main.go": "package main", "app.py": "pass"}},
		{"Node server retains priority", "auto", ".", "node", map[string]string{"package.json": `{"scripts":{"start":"node server.js"}}`, "app.py": "pass"}},
		{"explicit static retained", "static", ".", "static", map[string]string{"app.py": "pass", "index.html": "hello"}},
		{"relative build context", "auto", "server", "python", map[string]string{"server/app.py": "pass"}},
		{"library is not a server", "auto", ".", "", map[string]string{"src/model/result.py": "pass", "requirements.txt": "idna==3.10"}},
		{"nested app is not a root server", "auto", ".", "", map[string]string{"src/app.py": "pass"}},
		{"directory is not an entry point", "auto", ".", "static", map[string]string{"app.py/nested": "pass", "index.html": "hello"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := Prepare(files(t, tt.data), domain.LocalDockerEnvironmentConfig{ContainerPort: 8080, Recipe: tt.selected, BuildContext: tt.context})
			if tt.want == "" {
				var failure *Failure
				if !errors.As(err, &failure) || failure.Code != "recipe_not_detected" {
					t.Fatalf("expected missing recipe, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if b.Recipe != tt.want {
				t.Fatalf("got %s, want %s", b.Recipe, tt.want)
			}
		})
	}
}

func TestPythonBuildUsesFixedRuntimeAndOptionalRequirements(t *testing.T) {
	for _, dependencies := range []bool{false, true} {
		name := "stdlib"
		if dependencies {
			name = "requirements"
		}
		t.Run(name, func(t *testing.T) {
			data := map[string]string{"app.py": "pass", "Dockerfile": "FROM user-supplied-image"}
			if dependencies {
				data["requirements.txt"] = "idna==3.10"
			}
			b, err := Prepare(files(t, data), domain.LocalDockerEnvironmentConfig{ContainerPort: 8097, Recipe: "auto"})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			raw, err := os.ReadFile(b.Dockerfile)
			if err != nil {
				t.Fatal(err)
			}
			dockerfile := string(raw)
			for _, required := range []string{PythonImage, "@sha256:", "WORKDIR /app", "PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1", "PORT=8097 HOST=0.0.0.0", "USER 65532:65532", `CMD ["python3","app.py"]`} {
				if !strings.Contains(dockerfile, required) {
					t.Fatalf("missing %s in generated recipe", required)
				}
			}
			if strings.Contains(dockerfile, "user-supplied-image") || strings.Contains(dockerfile, "nginx") {
				t.Fatal("user Dockerfile or static fallback selected")
			}
			if strings.Contains(dockerfile, "python3 -m pip install --no-cache-dir -r requirements.txt") != dependencies {
				t.Fatal("requirements installation did not match source tree")
			}
		})
	}
	// Internal auto detection must not extend the public enum or job config.
	if b, err := Prepare(files(t, map[string]string{"app.py": "pass"}), domain.LocalDockerEnvironmentConfig{ContainerPort: 8080, Recipe: "python"}); err == nil {
		b.Close()
		t.Fatal("accepted a new public recipe value")
	}
}
