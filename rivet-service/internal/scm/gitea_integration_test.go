//go:build gitea_integration

package scm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Интеграционный тест адаптера Gitea против настоящего инстанса в Docker
// (change add-gitea-adapter). В обычный go test ./... не входит:
//
//	go test -tags gitea_integration -run TestGiteaIntegration -v ./internal/scm/
//
// Образ задаёт RIVET_GITEA_IMAGE (по умолчанию gitea/gitea:28.3; подходит и
// codeberg.org/forgejo/forgejo). Нужен docker. Контейнер удаляется в конце.

func TestGiteaIntegration(t *testing.T) {
	image := os.Getenv("RIVET_GITEA_IMAGE")
	if image == "" {
		image = "gitea/gitea:28.3"
	}
	it := startGitea(t, image)
	ctx := context.Background()
	g := NewGitea(it.base, it.token)
	const repo = "rivet-bot/rivet-it"

	t.Run("проверка токена и создание репозитория", func(t *testing.T) {
		if res := g.Probe(ctx, ""); !res.OK || res.TokenOwner != "rivet-bot" {
			t.Fatalf("проверка токена: %+v", res)
		}
		info, err := g.CreateRepo(ctx, NewRepo{Name: "rivet-it", Private: true})
		if err != nil || info.Path != repo || info.DefaultBranch != "main" || info.WebURL == "" {
			t.Fatalf("создание: %+v %v", info, err)
		}
		if _, err := g.CreateRepo(ctx, NewRepo{Name: "rivet-it"}); !errors.Is(err, ErrRepoExists) {
			t.Fatalf("имя занято: %v", err)
		}
		res := g.Probe(ctx, repo)
		if !res.OK || !res.CanPush || res.DefaultBranch != "main" || res.RepoPath != repo {
			t.Fatalf("проверка репозитория: %+v", res)
		}
		if res := g.Probe(ctx, "rivet-bot/nope"); res.OK || res.Reason != ReasonNotFound {
			t.Fatalf("нет репозитория: %+v", res)
		}
		if res := NewGitea(it.base, "0000000000000000000000000000000000000000").Probe(ctx, repo); res.OK || res.Reason != ReasonBadToken {
			t.Fatalf("чужой токен: %+v", res)
		}
		it.api(t, http.MethodPost, "/orgs", map[string]any{"username": "team"}, http.StatusCreated)
		if info, err := g.CreateRepo(ctx, NewRepo{Owner: "team", Name: "svc"}); err != nil || info.Path != "team/svc" {
			t.Fatalf("репозиторий организации: %+v %v", info, err)
		}
	})

	t.Run("ветки и файлы", func(t *testing.T) {
		if sha, err := g.HeadSHA(ctx, repo, "main"); err != nil || len(sha) != 40 {
			t.Fatalf("вершина main: %q %v", sha, err)
		}
		if _, err := g.HeadSHA(ctx, repo, "nope"); !errors.Is(err, ErrBranchNotFound) {
			t.Fatalf("нет ветки: %v", err)
		}
		it.api(t, http.MethodPost, "/user/repos", map[string]any{"name": "empty"}, http.StatusCreated)
		if _, err := g.HeadSHA(ctx, "rivet-bot/empty", "main"); !errors.Is(err, ErrBranchNotFound) {
			t.Fatalf("пустой репозиторий: %v", err)
		}
		if _, err := g.HeadSHA(ctx, "rivet-bot/gone", "main"); err == nil || errors.Is(err, ErrBranchNotFound) {
			t.Fatalf("нет репозитория — не «нет ветки»: %v", err)
		}
		readme, err := g.ReadFile(ctx, repo, "main", "README.md")
		if err != nil || !strings.Contains(readme.Content, "rivet-it") || readme.FileID == "" {
			t.Fatalf("README: %+v %v", readme, err)
		}
		if _, err := g.ReadFile(ctx, repo, "main", "nope.md"); !errors.Is(err, ErrFileNotFound) {
			t.Fatalf("нет файла: %v", err)
		}
		if _, err := g.ReadFile(ctx, repo, "nosuch", "README.md"); !errors.Is(err, ErrRefNotFound) {
			t.Fatalf("нет версии: %v", err)
		}
		c, err := g.WriteFile(ctx, repo, "main", "docs/architecture/architecture.yaml", "", "name: it\n", "описание")
		if err != nil || len(c.SHA) != 40 || c.URL == "" {
			t.Fatalf("новый файл: %+v %v", c, err)
		}
		f, err := g.ReadFile(ctx, repo, c.SHA, "docs/architecture/architecture.yaml")
		if err != nil || f.Content != "name: it\n" {
			t.Fatalf("чтение по SHA: %+v %v", f, err)
		}
		if _, err := g.WriteFile(ctx, repo, "main", "docs/architecture/architecture.yaml", f.FileID, "name: it2\n", "правка"); err != nil {
			t.Fatalf("обновление: %v", err)
		}
		if _, err := g.WriteFile(ctx, repo, "main", "docs/architecture/architecture.yaml", f.FileID, "name: it3\n", "устаревшая правка"); err == nil {
			t.Fatal("запись по устаревшей версии отклоняется")
		}
		big := strings.Repeat("0123456789abcdef", 80<<10) // 1.25 МБ: больше порога contents API
		if _, err := g.WriteFile(ctx, repo, "main", "big.txt", "", big, "большой файл"); err != nil {
			t.Fatalf("большой файл: %v", err)
		}
		if f, err := g.ReadFile(ctx, repo, "main", "big.txt"); err != nil || f.Content != big {
			t.Fatalf("чтение большого файла: %d %v", len(f.Content), err)
		}
		head, _ := g.HeadSHA(ctx, repo, "main")
		if tree, err := g.PathSHA(ctx, repo, head, "docs/architecture"); err != nil || len(tree) != 40 {
			t.Fatalf("дерево каталога: %q %v", tree, err)
		}
		if tree, err := g.PathSHA(ctx, repo, head, "docs/none"); err != nil || tree != "" {
			t.Fatalf("нет каталога: %q %v", tree, err)
		}
	})

	t.Run("PR, diff, файлы и merge проверенной вершины", func(t *testing.T) {
		head := it.pushBranch(t, repo, "agent/task-1", map[string]string{"src/a.txt": "a\n"})
		if sha, err := g.HeadSHA(ctx, repo, "agent/task-1"); err != nil || sha != head {
			t.Fatalf("вершина ветки задачи: %q %v, push дал %q", sha, err, head)
		}
		pr, err := g.CreatePR(ctx, repo, "agent/task-1", "main", "task-1: a", "задача")
		if err != nil || pr.Number == 0 || !strings.HasSuffix(pr.URL, fmt.Sprintf("/pulls/%d", pr.Number)) {
			t.Fatalf("PR: %+v %v", pr, err)
		}
		// Повтор создания PR той же ветки (ответ прошлого запроса потерялся):
		// отказ «PR уже есть», открытый PR находится по ветке и базе.
		_, dup := g.CreatePR(ctx, repo, "agent/task-1", "main", "task-1: a", "задача")
		t.Logf("повтор создания PR: %v", dup)
		if !errors.Is(dup, ErrPRExists) {
			t.Fatalf("повтор создания PR — ErrPRExists: %v", dup)
		}
		// OpenPR сначала спрашивает PR по базе и ветке: эндпоинт есть в
		// Gitea 28 и Forgejo 15, ветка со слешем проходит.
		if _, code, _, err := g.do(ctx, http.MethodGet, giteaRepo(repo)+"/pulls/main/agent/task-1", nil, ""); err != nil || code != http.StatusOK {
			t.Fatalf("PR по базе и ветке: %d %v", code, err)
		}
		if open, ok, err := g.OpenPR(ctx, repo, "agent/task-1", "main"); err != nil || !ok || open.Number != pr.Number || open.URL != pr.URL {
			t.Fatalf("открытый PR ветки: %+v %v %v", open, ok, err)
		}
		if _, ok, err := g.OpenPR(ctx, repo, "agent/task-1", "release"); err != nil || ok {
			t.Fatalf("PR в другую базовую ветку не подходит: %v %v", ok, err)
		}
		d, err := g.Diff(ctx, repo, pr.Number)
		if err != nil || !strings.Contains(d, "diff --git a/src/a.txt b/src/a.txt") {
			t.Fatalf("diff: %q %v", d, err)
		}
		files, err := g.PRFiles(ctx, repo, pr.Number)
		if err != nil || len(files.Files) != 1 || files.Files[0] != "src/a.txt" || files.HeadSHA != head {
			t.Fatalf("файлы PR: %+v %v", files, err)
		}
		if _, err := g.MergeAt(ctx, repo, pr.Number, strings.Repeat("0", 40)); !errors.Is(err, ErrHeadMoved) {
			t.Fatalf("сдвиг вершины: %v", err)
		}
		sha, err := g.MergeAt(ctx, repo, pr.Number, head)
		if err != nil || len(sha) != 40 {
			t.Fatalf("merge: %q %v", sha, err)
		}
		if main, _ := g.HeadSHA(ctx, repo, "main"); main != sha {
			t.Fatalf("merge-коммит на main: %q, merge дал %q", main, sha)
		}
	})

	t.Run("merge после сдвига базы и конфликт", func(t *testing.T) {
		it.pushBranch(t, repo, "agent/task-2", map[string]string{"src/b.txt": "b\n"})
		it.pushBranch(t, repo, "agent/task-3", map[string]string{"src/c.txt": "c\n"})
		it.pushBranch(t, repo, "agent/task-4", map[string]string{"README.md": "# task-4\n"})
		it.pushBranch(t, repo, "agent/task-5", map[string]string{"README.md": "# task-5\n"})
		var prs []PR
		for _, b := range []string{"agent/task-2", "agent/task-3", "agent/task-4", "agent/task-5"} {
			pr, err := g.CreatePR(ctx, repo, b, "main", b, "")
			if err != nil {
				t.Fatal(err)
			}
			prs = append(prs, pr)
		}
		// Соседние задачи мержатся одновременно: push одного merge Gitea
		// отклоняет (база сдвинулась), остальные PR пересчитываются, адаптер
		// дожидается пересчёта и повторяет merge.
		errs := make(chan error, 3)
		for _, pr := range prs[:3] {
			go func(n int) {
				_, err := g.Merge(ctx, repo, n)
				if err != nil {
					err = fmt.Errorf("merge #%d вместе с соседями: %w", n, err)
				}
				errs <- err
			}(pr.Number)
		}
		for range 3 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		_, err := g.Merge(ctx, repo, prs[3].Number)
		if err == nil || errors.Is(err, ErrHeadMoved) {
			t.Fatalf("конфликтующий PR не мержится: %v", err)
		}
		// Gitea не называет причину отказа: конфликтом считается только
		// явный ответ, иначе обычный отказ с пояснением.
		if !errors.Is(err, ErrMergeConflict) && !strings.Contains(err.Error(), "конфликт") {
			t.Fatalf("отказ без пояснения: %v", err)
		}
		t.Logf("конфликт: %v (ErrMergeConflict=%v)", err, errors.Is(err, ErrMergeConflict))
	})

	t.Run("webhook", func(t *testing.T) {
		const hook = "http://127.0.0.1:9/api/v1/webhooks/gitea"
		for _, secret := range []string{"secret-1", "secret-2"} {
			if ok, err := g.RegisterWebhook(ctx, repo, hook, secret); err != nil || !ok {
				t.Fatalf("регистрация (%s): %v %v", secret, ok, err)
			}
		}
		var hooks []struct {
			BranchFilter string   `json:"branch_filter"`
			Events       []string `json:"events"`
			Active       bool     `json:"active"`
			Config       struct {
				URL string `json:"url"`
			} `json:"config"`
		}
		it.apiJSON(t, http.MethodGet, "/repos/"+repo+"/hooks", nil, http.StatusOK, &hooks)
		if len(hooks) != 1 || hooks[0].Config.URL != hook || hooks[0].BranchFilter != "main" || !hooks[0].Active {
			t.Fatalf("один хук с фильтром основной ветки: %+v", hooks)
		}
		got := strings.Join(hooks[0].Events, ",")
		for _, e := range []string{"push", "pull_request", "pull_request_review"} {
			if !strings.Contains(got, e) {
				t.Fatalf("событие %s не подписано: %v", e, hooks[0].Events)
			}
		}
	})

	t.Run("пайплайн доставки", func(t *testing.T) {
		wf := "name: deploy\non:\n  workflow_dispatch:\n    inputs:\n      version:\n        required: false\njobs:\n  deploy:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ok\n"
		if _, err := g.WriteFile(ctx, repo, "main", ".gitea/workflows/deploy.yml", "", wf, "workflow"); err != nil {
			t.Fatal(err)
		}
		since := time.Now().Add(-time.Minute)
		var run PipelineRun
		var err error
		// Workflow появляется в Actions не сразу после коммита.
		for i := 0; i < 20; i++ {
			if run, err = g.TriggerPipeline(ctx, repo, "deploy.yml", "main", map[string]string{"version": "abc"}); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("запуск workflow: %v", err)
		}
		if run.State != PipelineStarting {
			t.Fatalf("запуск: %+v", run)
		}
		for i := 0; i < 20 && run.RunID == ""; i++ {
			if run, err = g.PipelineRun(ctx, repo, "deploy.yml", "main", "", since); err != nil {
				if strings.Contains(err.Error(), "404") {
					t.Skipf("у инстанса нет API прогонов Actions: %v", err)
				}
				t.Fatal(err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		if run.RunID == "" || run.State != PipelineRunning {
			t.Fatalf("прогон без runner'а ждёт: %+v", run)
		}
		again, err := g.PipelineRun(ctx, repo, "deploy.yml", "main", run.RunID, since)
		if err != nil || again.RunID != run.RunID || again.URL == "" {
			t.Fatalf("прогон по идентификатору: %+v %v", again, err)
		}
		// Окно свежести по created_at: прогон, начавшийся до запуска
		// публикации, чужой.
		if old, err := g.PipelineRun(ctx, repo, "deploy.yml", "main", "", time.Now().Add(time.Minute)); err != nil || old.RunID != "" {
			t.Fatalf("прошлый прогон не берётся: %+v %v", old, err)
		}
	})

	t.Run("scope токена", func(t *testing.T) {
		ro := it.token2(t, []string{"read:repository", "read:user"})
		res := NewGitea(it.base, ro).Probe(ctx, repo)
		if res.OK || res.Reason != ReasonNoScope || res.CanPush || !strings.Contains(res.Message, "write:repository") {
			t.Fatalf("токен только на чтение: %+v", res)
		}
		rw := it.token2(t, []string{"write:repository", "read:user"})
		if res := NewGitea(it.base, rw).Probe(ctx, repo); !res.OK {
			t.Fatalf("токен с записью: %+v", res)
		}
		// Проверка scope не создаёт PR.
		var open []json.RawMessage
		it.apiJSON(t, http.MethodGet, "/repos/"+repo+"/pulls?state=open", nil, http.StatusOK, &open)
		for _, raw := range open {
			if strings.Contains(string(raw), `"title":""`) {
				t.Fatalf("проверка scope создала PR: %s", raw)
			}
		}
	})

	t.Run("инстанс в подкаталоге", func(t *testing.T) {
		target, _ := url.Parse(it.base)
		proxy := httputil.NewSingleHostReverseProxy(target)
		srv := httptest.NewServer(http.StripPrefix("/gitea", proxy))
		defer srv.Close()
		sub := NewGitea(srv.URL+"/gitea", it.token)
		if res := sub.Probe(ctx, repo); !res.OK || res.RepoPath != repo {
			t.Fatalf("проверка через подкаталог: %+v", res)
		}
		if sha, err := sub.HeadSHA(ctx, repo, "main"); err != nil || len(sha) != 40 {
			t.Fatalf("вершина через подкаталог: %q %v", sha, err)
		}
		// Подкаталог находится вопросом к инстансу, хвост страницы любой.
		for _, tail := range []string{"", "/src/branch/main", "/labels", "/blame/branch/main/README.md"} {
			base, got, found, err := LocateGitea(ctx, srv.URL, "gitea/"+repo+tail)
			if err != nil || base != srv.URL+"/gitea" || got != repo || !found {
				t.Fatalf("адрес %q: %q %q %v %v", tail, base, got, found, err)
			}
		}
		if base, got, found, err := LocateGitea(ctx, it.base, repo+"/milestones"); err != nil || base != it.base || got != repo || !found {
			t.Fatalf("инстанс в корне: %q %q %v %v", base, got, found, err)
		}
	})

	t.Run("защита ветки", func(t *testing.T) {
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || ok {
			t.Fatalf("без правила: %v %v", ok, err)
		}
		it.api(t, http.MethodPost, "/repos/"+repo+"/branch_protections", map[string]any{"rule_name": "main", "required_approvals": 0}, http.StatusCreated)
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || ok {
			t.Fatalf("защита без обязательного одобрения: %v %v", ok, err)
		}
		it.api(t, http.MethodPatch, "/repos/"+repo+"/branch_protections/main", map[string]any{"required_approvals": 1}, http.StatusOK)
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || !ok {
			t.Fatalf("защита с одобрением: %v %v", ok, err)
		}
		// Прямой push разрешён всем с правом записи — ветка не защищена.
		it.api(t, http.MethodPatch, "/repos/"+repo+"/branch_protections/main", map[string]any{"enable_push": true}, http.StatusOK)
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || ok {
			t.Fatalf("push разрешён всем: %v %v", ok, err)
		}
		// Белый список с ботом — тоже нет, без бота — защищена.
		it.api(t, http.MethodPatch, "/repos/"+repo+"/branch_protections/main", map[string]any{
			"enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{"rivet-bot"}}, http.StatusOK)
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || ok {
			t.Fatalf("бот в белом списке push: %v %v", ok, err)
		}
		it.api(t, http.MethodPatch, "/repos/"+repo+"/branch_protections/main", map[string]any{
			"enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{}}, http.StatusOK)
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || !ok {
			t.Fatalf("белый список без бота: %v %v", ok, err)
		}
		it.api(t, http.MethodPost, "/repos/"+repo+"/branch_protections", map[string]any{"rule_name": "release/*", "required_approvals": 1}, http.StatusCreated)
		if ok, err := g.BranchProtected(ctx, repo, "release/1.0"); err != nil || !ok {
			t.Fatalf("правило-шаблон: %v %v", ok, err)
		}
		// Отдельный репозиторий: регистр точного правила и незащищённые
		// файлы.
		const prot = "rivet-bot/rivet-prot"
		if _, err := g.CreateRepo(ctx, NewRepo{Name: "rivet-prot"}); err != nil {
			t.Fatal(err)
		}
		it.api(t, http.MethodPost, "/repos/"+prot+"/branch_protections", map[string]any{"rule_name": "MAIN", "required_approvals": 1}, http.StatusCreated)
		if ok, err := g.BranchProtected(ctx, prot, "main"); err != nil || !ok {
			t.Fatalf("строгое правило в другом регистре: %v %v", ok, err)
		}
		it.api(t, http.MethodPatch, "/repos/"+prot+"/branch_protections/MAIN", map[string]any{"unprotected_file_patterns": ".rivet/**"}, http.StatusOK)
		if ok, err := g.BranchProtected(ctx, prot, "main"); err != nil || ok {
			t.Fatalf("прямой push в незащищённые файлы: %v %v", ok, err)
		}
		// Правило-шаблон без одобрений поверх точного: Gitea применит одно
		// из них, адаптер в сомнении отвечает «не защищена».
		it.api(t, http.MethodPost, "/repos/"+repo+"/branch_protections", map[string]any{"rule_name": "*", "required_approvals": 0}, http.StatusCreated)
		if ok, err := g.BranchProtected(ctx, repo, "main"); err != nil || ok {
			t.Fatalf("подходящее правило без одобрений: %v %v", ok, err)
		}
	})
}

// giteaIT — запущенный инстанс.
type giteaIT struct {
	base, token, password string
}

func startGitea(t *testing.T, image string) giteaIT {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("нет docker")
	}
	name := fmt.Sprintf("rivet-gitea-it-%d", time.Now().UnixNano())
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::3000",
		"-e", "GITEA__security__INSTALL_LOCK=true", "-e", "GITEA__database__DB_TYPE=sqlite3",
		"-e", "FORGEJO__security__INSTALL_LOCK=true", "-e", "FORGEJO__database__DB_TYPE=sqlite3",
		"-e", "GITEA__repository__DEFAULT_BRANCH=main", "-e", "GITEA__actions__ENABLED=true",
		"-e", "GITEA__webhook__ALLOWED_HOST_LIST=*",
		image).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s: %v: %s", image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	out, err = exec.Command("docker", "port", name, "3000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	base := "http://" + strings.Replace(addr, "0.0.0.0", "127.0.0.1", 1)
	deadline := time.Now().Add(90 * time.Second)
	for {
		resp, err := http.Get(base + "/api/v1/version")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("инстанс %s не поднялся", image)
		}
		time.Sleep(time.Second)
	}
	const password = "it-password-123456"
	var created bool
	for _, bin := range []string{"gitea", "forgejo"} {
		cmd := exec.Command("docker", "exec", "-u", "git", name, bin, "admin", "user", "create",
			"--username", "rivet-bot", "--password", password, "--email", "bot@example.com",
			"--admin", "--must-change-password=false")
		if out, err := cmd.CombinedOutput(); err == nil {
			created = true
			break
		} else {
			t.Logf("%s admin user create: %v: %s", bin, err, out)
		}
	}
	if !created {
		t.Fatal("не удалось создать администратора")
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/users/rivet-bot/tokens",
		strings.NewReader(`{"name":"it","scopes":["all"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("rivet-bot:"+password)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.SHA1 == "" {
		t.Fatalf("токен: %v %d", err, resp.StatusCode)
	}
	var ver struct {
		Version string `json:"version"`
	}
	if r, err := http.Get(base + "/api/v1/version"); err == nil {
		_ = json.NewDecoder(r.Body).Decode(&ver)
		_ = r.Body.Close()
	}
	t.Logf("инстанс %s, версия %s, %s", image, ver.Version, base)
	return giteaIT{base: base, token: tok.SHA1, password: password}
}

// token2 — ещё один токен бота с заданными scope'ами.
func (it giteaIT) token2(t *testing.T, scopes []string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": fmt.Sprintf("it-%d", time.Now().UnixNano()), "scopes": scopes})
	req, _ := http.NewRequest(http.MethodPost, it.base+"/api/v1/users/rivet-bot/tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("rivet-bot:"+it.password)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.SHA1 == "" {
		t.Fatalf("токен %v: %v %d", scopes, err, resp.StatusCode)
	}
	return tok.SHA1
}

// api — запрос к API инстанса от имени администратора с ожидаемым кодом.
func (it giteaIT) api(t *testing.T, method, path string, body any, want int) {
	t.Helper()
	it.apiJSON(t, method, path, body, want, nil)
}

func (it giteaIT) apiJSON(t *testing.T, method, path string, body any, want int, out any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, it.base+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "token "+it.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d, ожидался %d: %s", method, path, resp.StatusCode, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
}

// pushBranch — ветка задачи от main с файлами, отправленная git'ом по
// HTTP так же, как это делает runner: логин служебный, пароль — токен.
// Возвращает вершину ветки.
func (it giteaIT) pushBranch(t *testing.T, repo, branch string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	askpass := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\ncase \"$1\" in *Username*) echo rivet;; *) echo \"$RIVET_GIT_TOKEN\";; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Окружение без чужих настроек git (GIT_CONFIG_*, askpass оболочки):
	// у runner'а оно тоже собирается заново.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.Contains(kv, "ASKPASS=") {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_ASKPASS="+askpass, "GIT_TERMINAL_PROMPT=0", "RIVET_GIT_TOKEN="+it.token,
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = filepath.Join(dir, "w"), env
		if args[0] == "clone" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("clone", "-q", it.base+"/"+repo+".git", "w")
	git("checkout", "-q", "-B", branch, "origin/main")
	for p, content := range files {
		full := filepath.Join(dir, "w", p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("-c", "user.name=rivet-runner", "-c", "user.email=runner@rivet.local", "commit", "-q", "-m", branch)
	git("push", "-q", "origin", branch)
	return git("rev-parse", "HEAD")
}
