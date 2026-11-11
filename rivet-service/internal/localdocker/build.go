// Package localdocker исполняет ограниченные рецепты локальной публикации.
// Shell-команд от пользователя здесь нет: Docker вызывается фиксированным argv.
package localdocker

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

const NodeImage = "node:24-bookworm@sha256:8fa96264f302f58bbb13aa7339c541547b56975766ce956c875da149452a3e40"
const NginxImage = "nginx:alpine@sha256:713fc2ebfd92e8217db451477182ee91944b123d2b1ee9fd5a8c6e91ca6c3a11"
const GoImage = "golang:1.26.11-alpine@sha256:ae52b5bc6ed045a9ed26ab03dc19327a802a9cc9aa8b27ed57f5c6b60579b0bb"
const AlpineImage = "alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"

// Failure — машинный код публикации и понятное описание без вывода секретов.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Failure) Error() string      { return e.Message }
func fail(code, message string) error { return &Failure{Code: code, Message: message} }

type BuildContext struct{ Dir, Dockerfile, Recipe string }

func (b *BuildContext) Close() { _ = os.RemoveAll(b.Dir) }

type packageJSON struct {
	Scripts map[string]string `json:"scripts"`
}

func regular(p string) bool { s, e := os.Stat(p); return e == nil && s.Mode().IsRegular() }

func recipe(root string, selected string) (string, string, bool, error) {
	var pkg packageJSON
	hasPackage := regular(filepath.Join(root, "package.json"))
	if hasPackage && selected != "go" && !(selected == "auto" && regular(filepath.Join(root, "go.mod"))) {
		b, e := os.ReadFile(filepath.Join(root, "package.json"))
		if e != nil {
			return "", "", false, e
		}
		if e = json.Unmarshal(b, &pkg); e != nil {
			return "", "", false, fail("recipe_not_detected", "package.json не является корректным JSON")
		}
	}
	build := pkg.Scripts["build"] != ""
	if selected == "auto" {
		switch {
		case regular(filepath.Join(root, "go.mod")):
			selected = "go"
		case hasPackage && pkg.Scripts["start"] != "":
			selected = "node"
		case hasPackage && build:
			selected = "static"
		case regular(filepath.Join(root, "index.html")):
			selected = "static"
		default:
			return "", "", false, fail("recipe_not_detected", "добавьте go.mod, package.json или index.html")
		}
	}
	switch selected {
	case "go":
		if !regular(filepath.Join(root, "go.mod")) {
			return "", "", false, fail("recipe_not_detected", "для рецепта go нужен go.mod")
		}
		dirs, _ := os.ReadDir(filepath.Join(root, "cmd"))
		targets := []string{}
		for _, d := range dirs {
			if d.IsDir() {
				files, _ := filepath.Glob(filepath.Join(root, "cmd", d.Name(), "*.go"))
				if len(files) > 0 {
					targets = append(targets, "./cmd/"+d.Name())
				}
			}
		}
		if len(targets) == 1 {
			return selected, targets[0], false, nil
		}
		if regular(filepath.Join(root, "main.go")) {
			return selected, ".", false, nil
		}
		if len(targets) > 1 {
			return "", "", false, fail("recipe_not_detected", "найдено несколько cmd-приложений: выделите приложение в отдельный build_context")
		}
		return "", "", false, fail("recipe_not_detected", "не найдена точка входа Go-приложения")
	case "node":
		if !hasPackage || pkg.Scripts["start"] == "" {
			return "", "", false, fail("recipe_not_detected", "для рецепта node нужен package.json со скриптом start")
		}
		return selected, "", build, nil
	case "static":
		if !build && !regular(filepath.Join(root, "index.html")) {
			return "", "", false, fail("recipe_not_detected", "для static нужен index.html или скрипт build в package.json")
		}
		return selected, "", build, nil
	default:
		return "", "", false, fail("recipe_not_detected", "неизвестный рецепт приложения")
	}
}

// Prepare копирует только дерево приложения, не меняя checkout и не включая
// .git, локальные .env и симлинки, через которые контекст вышел бы из checkout.
func Prepare(root string, config domain.LocalDockerEnvironmentConfig) (*BuildContext, error) {
	if e := (domain.EnvConfig{LocalDocker: &config}).Validate(domain.ExecLocalDocker); e != nil {
		return nil, e
	}
	base, e := filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	source, e := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(config.BuildContext)))
	if e != nil {
		return nil, fail("recipe_not_detected", "build_context не существует")
	}
	rel, e := filepath.Rel(base, source)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fail("config_invalid", "build_context выходит за пределы репозитория")
	}
	kind, entry, build, e := recipe(source, config.Recipe)
	if e != nil {
		return nil, e
	}
	dir, e := os.MkdirTemp("", "rivet-build-")
	if e != nil {
		return nil, e
	}
	out := &BuildContext{Dir: dir, Dockerfile: filepath.Join(dir, ".rivet.Dockerfile"), Recipe: kind}
	if e = copyTree(source, dir); e != nil {
		out.Close()
		return nil, e
	}
	port := config.ContainerPort
	common := fmt.Sprintf("ENV PORT=%d HOST=0.0.0.0 HOME=/tmp RIVET_DATA_DIR=/data\nEXPOSE %d\nUSER 65532:65532\n", port, port)
	var dockerfile string
	switch kind {
	case "go":
		// entry — известный относительный каталог, передаваемый Go без shell.
		b, _ := json.Marshal([]string{"go", "build", "-trimpath", "-o", "/out/app", entry})
		dockerfile = fmt.Sprintf("FROM %s AS build\nWORKDIR /src\nCOPY . .\nENV CGO_ENABLED=0\nRUN %s\nFROM %s\nRUN apk add --no-cache ca-certificates && mkdir -p /data && chown 65532:65532 /data\nCOPY --from=build /out/app /app/run\n%sENTRYPOINT [\"/app/run\"]\n", GoImage, b, AlpineImage, common)
	case "node":
		dockerfile = fmt.Sprintf("FROM %s\nWORKDIR /app\nCOPY . .\nRUN npm ci\n", NodeImage)
		if build {
			dockerfile += "RUN npm run build\n"
		}
		dockerfile += "RUN mkdir -p /data && chown 65532:65532 /data\n" + common + "CMD [\"npm\",\"start\"]\n"
	case "static":
		prefix := ""
		copyFrom := "COPY . /app/public\n"
		if build {
			prefix = fmt.Sprintf("FROM %s AS build\nWORKDIR /src\nCOPY . .\nRUN npm ci && npm run build\n", NodeImage)
			copyFrom = "COPY --from=build /src/dist /app/public\n"
		}
		dockerfile = prefix + fmt.Sprintf("FROM %s\n", NginxImage) + copyFrom + "COPY .rivet.nginx.conf /app/nginx.conf\nRUN mkdir -p /data && chown 65532:65532 /data\n" + common + "ENTRYPOINT [\"nginx\"]\nCMD [\"-c\",\"/app/nginx.conf\",\"-g\",\"daemon off;\"]\n"
		nginx := fmt.Sprintf("worker_processes 1;\npid /tmp/nginx.pid;\nerror_log /dev/stderr;\nevents { worker_connections 1024; }\nhttp { include /etc/nginx/mime.types; access_log /dev/stdout; client_body_temp_path /tmp/client; proxy_temp_path /tmp/proxy; fastcgi_temp_path /tmp/fastcgi; uwsgi_temp_path /tmp/uwsgi; scgi_temp_path /tmp/scgi; server { listen %d; root /app/public; location / { try_files $uri $uri/ /index.html; } } }\n", port)
		if e = os.WriteFile(filepath.Join(dir, ".rivet.nginx.conf"), []byte(nginx), 0644); e != nil {
			out.Close()
			return nil, e
		}
	}
	if e = os.WriteFile(out.Dockerfile, []byte(dockerfile), 0600); e != nil {
		out.Close()
		return nil, e
	}
	return out, nil
}

func copyTree(source, dest string) error {
	var total int64
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, e := filepath.Rel(source, p)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		name := d.Name()
		if name == ".git" || name == "node_modules" || name == ".rivet.Dockerfile" || name == ".rivet.nginx.conf" || (strings.HasPrefix(name, ".env") && name != ".env.example") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fail("config_invalid", "симлинки в контексте сборки не поддерживаются: "+rel)
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fail("config_invalid", "в контексте допускаются только обычные файлы")
		}
		total += info.Size()
		if info.Size() > 100<<20 || total > 1<<30 {
			return fail("config_invalid", "контекст сборки превышает допустимый размер")
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		mode := os.FileMode(0644)
		if info.Mode()&0111 != 0 {
			mode = 0755
		}
		out, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		closeErr := out.Close()
		if e != nil {
			return e
		}
		return closeErr
	})
}
