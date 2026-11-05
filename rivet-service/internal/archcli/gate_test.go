package archcli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/architecture"
)

// gitT — git в каталоге теста с явной identity (на CI глобального config нет).
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWithDescription — репозиторий с корректным описанием и актуальными
// представлениями на main и веткой задачи task без изменений.
func repoWithDescription(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	repo := t.TempDir()
	gitT(t, repo, "init", "-q")
	dir := filepath.Join(repo, architecture.DefaultDir)
	if err := architecture.Init(dir, "Демо"); err != nil {
		t.Fatal(err)
	}
	if _, err := architecture.WriteViews(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "seed")
	gitT(t, repo, "checkout", "-q", "-b", "task")
	return repo
}

func gate(t *testing.T, repo string, force, onlyDocs bool) (int, string) {
	t.Helper()
	return gateAt(t, repo, "main", force, onlyDocs)
}

func gateAt(t *testing.T, repo, base string, force, onlyDocs bool) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := Gate(&out, repo, architecture.DefaultDir, base, force, onlyDocs)
	return code, out.String()
}

// Спека project-architecture «Проверка архитектуры на шаге test»: ветка без
// изменений описания проходит без разбора, сломанное описание и устаревшие
// представления проваливают проверку, задача описания не трогает код.
func TestGate(t *testing.T) {
	repo := repoWithDescription(t)

	if code, out := gate(t, repo, false, false); code != ExitOK || !strings.Contains(out, "не менялось") {
		t.Fatalf("без изменений описания: %d %s", code, out)
	}
	if code, out := gate(t, repo, true, true); code != ExitOK || !strings.Contains(out, "корректно") {
		t.Fatalf("корректное описание с -force: %d %s", code, out)
	}

	// Код вне описания: обычной задаче можно, задаче описания нельзя.
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := gate(t, repo, false, false); code != ExitOK {
		t.Fatal("правка кода без описания проверку проходит")
	}
	if code, out := gate(t, repo, true, true); code != ExitDefects || !strings.Contains(out, "main.go") {
		t.Fatalf("задача описания правит код: %d %s", code, out)
	}
	if err := os.Remove(filepath.Join(repo, "main.go")); err != nil {
		t.Fatal(err)
	}

	// Сломанное описание: модель ссылается на несуществующий элемент.
	manifest := filepath.Join(repo, architecture.DefaultDir, "architecture.yaml")
	orig, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("format: [сломано\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := gate(t, repo, false, false); code == ExitOK {
		t.Fatal("сломанное описание проходит проверку")
	}
	if err := os.WriteFile(manifest, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	// Устаревшие представления: описание поменяли, views не перегенерировали.
	views := filepath.Join(repo, architecture.DefaultDir, "views", "README.md")
	if err := os.WriteFile(views, []byte("устарело\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := gate(t, repo, false, false); code != ExitDefects || !strings.Contains(out, "устарели") {
		t.Fatalf("устаревшие представления: %d %s", code, out)
	}
}

// Переименование не обходит гейт: перенос кода в каталог описания
// проваливает -only-docs, перенос файла описания наружу считается
// изменением описания (ревью Opus, круг 1).
func TestGateRenames(t *testing.T) {
	repo := repoWithDescription(t)
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n\nfunc main() { println(\"service\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "checkout", "-q", "main")
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "код")
	gitT(t, repo, "checkout", "-q", "-B", "task")

	gitT(t, repo, "mv", "main.go", architecture.DefaultDir+"/main.go")
	gitT(t, repo, "commit", "-q", "-m", "перенос кода в описание")
	if code, out := gate(t, repo, true, true); code != ExitDefects || !strings.Contains(out, "main.go") {
		t.Fatalf("перенос кода в каталог описания: %d %s", code, out)
	}

	gitT(t, repo, "reset", "-q", "--hard", "main")
	gitT(t, repo, "mv", architecture.DefaultDir+"/architecture.yaml", "architecture.yaml")
	gitT(t, repo, "commit", "-q", "-m", "перенос описания наружу")
	if _, out := gate(t, repo, false, false); strings.Contains(out, "не менялось") {
		t.Fatalf("перенос описания наружу выглядит как отсутствие изменений: %s", out)
	}
}

// Состояние .git, оставленное агентом, не ослепляет гейт (ревью Opus,
// круг 2, репро r2/gate_replace2.sh): B — git replace общего предка
// коммитом с деревом ветки задачи, C — refspec, который кладёт ветку
// задачи в origin/main. Подготовка workspace как у runner'а их не
// убирает, гейт всё равно видит src/main.go.
func TestGateIgnoresTamperedGitState(t *testing.T) {
	for _, variant := range []string{"replace", "refspec"} {
		t.Run(variant, func(t *testing.T) {
			if _, err := exec.LookPath("git"); err != nil {
				t.Skip("git недоступен")
			}
			root := t.TempDir()
			origin := filepath.Join(root, "origin.git")
			gitT(t, root, "init", "-q", "--bare", origin)
			ws := filepath.Join(root, "ws")
			gitT(t, root, "clone", "-q", origin, ws)
			if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitT(t, ws, "add", "-A")
			gitT(t, ws, "commit", "-q", "-m", "init")
			if err := os.WriteFile(filepath.Join(ws, "README"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitT(t, ws, "add", "-A")
			gitT(t, ws, "commit", "-q", "-m", "readme")
			gitT(t, ws, "push", "-q", "origin", "main")
			gitT(t, ws, "checkout", "-q", "-b", "agent/task-1")

			// Шаг describe: описание и заодно правка кода.
			dir := filepath.Join(ws, architecture.DefaultDir)
			if err := architecture.Init(dir, "Демо"); err != nil {
				t.Fatal(err)
			}
			if _, err := architecture.WriteViews(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main\n// backdoor\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitT(t, ws, "add", "-A")
			gitT(t, ws, "commit", "-q", "-m", "describe")
			gitT(t, ws, "push", "-q", "origin", "agent/task-1")

			if code, out := gateAt(t, ws, "origin/main", true, true); code != ExitDefects || !strings.Contains(out, "src/main.go") {
				t.Fatalf("без подмены: %d %s", code, out)
			}
			mb := strings.TrimSpace(gitOut(t, ws, "merge-base", "origin/main", "HEAD"))
			switch variant {
			case "replace":
				fake := strings.TrimSpace(gitOut(t, ws, "commit-tree", "HEAD^{tree}", "-p", mb+"^", "-m", "readme"))
				gitT(t, ws, "replace", mb, fake)
			case "refspec":
				gitT(t, ws, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/agent/task-1:refs/remotes/origin/main")
				gitT(t, ws, "config", "--add", "remote.origin.fetch", "+refs/heads/agent/*:refs/remotes/origin/agent/*")
			}
			// Подготовка workspace к шагу check, как у прежнего runner'а.
			gitT(t, ws, "fetch", "-q", "--prune", "origin")
			gitT(t, ws, "reset", "-q", "--hard")
			if code, out := gateAt(t, ws, "origin/main", true, true); code != ExitDefects || !strings.Contains(out, "src/main.go") {
				t.Fatalf("после подмены %s гейт пропустил ветку: %d %s", variant, code, out)
			}
		})
	}
}

// describedWorkspace — origin и рабочая копия, где ветка задачи описания
// заодно правит src/main.go и уже запушена.
func describedWorkspace(t *testing.T) (ws, origin string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git недоступен")
	}
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	gitT(t, root, "init", "-q", "--bare", origin)
	ws = filepath.Join(root, "ws")
	gitT(t, root, "clone", "-q", origin, ws)
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "init")
	gitT(t, ws, "push", "-q", "origin", "main")
	gitT(t, ws, "checkout", "-q", "-b", "agent/task-1")
	dir := filepath.Join(ws, architecture.DefaultDir)
	if err := architecture.Init(dir, "Демо"); err != nil {
		t.Fatal(err)
	}
	if _, err := architecture.WriteViews(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "main.go"), []byte("package main\n// backdoor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, ws, "add", "-A")
	gitT(t, ws, "commit", "-q", "-m", "describe")
	gitT(t, ws, "push", "-q", "origin", "agent/task-1")
	return ws, origin
}

// Репро ревью r3 (gate_bypass3.sh): .git агента подменяет адрес origin (D),
// переписывает его через url.*.insteadOf (E) или ставит хук
// reference-transaction, который двигает ref базы гейта (F). Гейт берёт
// адрес из конфигурации проекта, не читает чужую конфигурацию и хуки и
// отказывает при переписывании адресов.
func TestGateIgnoresTamperedRemote(t *testing.T) {
	for _, variant := range []string{"url", "insteadOf", "include", "hook"} {
		t.Run(variant, func(t *testing.T) {
			ws, origin := describedWorkspace(t)
			gd := strings.TrimSpace(gitOut(t, ws, "rev-parse", "--absolute-git-dir"))
			fake := filepath.Join(gd, "fake.git")
			switch variant {
			case "url":
				gitT(t, ws, "init", "-q", "--bare", fake)
				gitT(t, ws, "push", "-q", fake, "HEAD:refs/heads/main", "HEAD:refs/heads/agent/task-1")
				gitT(t, ws, "config", "remote.origin.pushurl", origin)
				gitT(t, ws, "config", "remote.origin.url", fake)
			case "insteadOf":
				gitT(t, ws, "init", "-q", "--bare", fake)
				gitT(t, ws, "push", "-q", fake, "HEAD:refs/heads/main", "HEAD:refs/heads/agent/task-1")
				gitT(t, ws, "config", "url."+fake+".insteadOf", origin)
			case "include":
				// Вариант G финальной проверки: insteadOf в подключённом файле.
				gitT(t, ws, "init", "-q", "--bare", fake)
				gitT(t, ws, "push", "-q", fake, "HEAD:refs/heads/main", "HEAD:refs/heads/agent/task-1")
				inc := filepath.Join(gd, "evil.config")
				if err := os.WriteFile(inc, []byte("[url \""+fake+"\"]\n\tinsteadOf = "+origin+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitT(t, ws, "config", "include.path", inc)
			case "hook":
				hook := "#!/bin/sh\n[ \"$1\" = committed ] || exit 0\n[ -n \"${RIVET_R3_HOOK:-}\" ] && exit 0\ncat >/dev/null\n" +
					"RIVET_R3_HOOK=1 git update-ref refs/rivet/gate-base HEAD\nexit 0\n"
				if err := os.WriteFile(filepath.Join(gd, "hooks", "reference-transaction"), []byte(hook), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Подготовка workspace к шагу check, как у прежнего runner'а.
			gitT(t, ws, "fetch", "-q", "--prune", "origin")
			gitT(t, ws, "reset", "-q", "--hard")
			var out bytes.Buffer
			code := GateOrigin(&out, ws, architecture.DefaultDir, "origin/main", origin, true, true)
			if code == ExitOK {
				t.Fatalf("гейт пропустил ветку с подменой %s: %s", variant, out.String())
			}
			if variant != "insteadOf" && variant != "include" && !strings.Contains(out.String(), "src/main.go") {
				t.Fatalf("подмена %s: ожидался src/main.go в отчёте: %d %s", variant, code, out.String())
			}
		})
	}
}

// Символическая ссылка внутри каталога описания проваливает гейт: снимок
// через API хостинга её не примет (ревью Opus, финальная проверка, P3-5).
func TestGateRejectsSymlink(t *testing.T) {
	repo := repoWithDescription(t)
	model := filepath.Join(repo, architecture.DefaultDir, "model")
	if err := os.Symlink("business.yaml", filepath.Join(model, "alias.yaml")); err != nil {
		t.Skipf("символические ссылки недоступны: %v", err)
	}
	if code, out := gate(t, repo, true, true); code != ExitDefects || !strings.Contains(out, "file.symlink") {
		t.Fatalf("симлинк в описании: %d %s", code, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// Main: неизвестная подкоманда и gate без базы — ошибка аргументов.
func TestMainUsage(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"nope"}, &out); code != ExitUsage {
		t.Fatalf("неизвестная подкоманда: %d", code)
	}
	if code := Main([]string{"gate"}, &out); code != ExitUsage {
		t.Fatalf("gate без -base: %d", code)
	}
}
