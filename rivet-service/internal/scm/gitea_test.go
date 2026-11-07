package scm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Адаптер Gitea на httptest (change add-gitea-adapter). Ответы взяты у
// настоящего Gitea 1.24 и 28.x: у старых версий причина 404 лежит в
// errors, у новых — в message.

// giteaStub — подставной инстанс: ответы по «МЕТОД путь?запрос», у одного
// маршрута может быть очередь ответов (последний повторяется).
type giteaStub struct {
	mu     sync.Mutex
	routes map[string][]giteaResp
	seen   map[string]string // маршрут → тело последнего запроса
	auth   string
}

type giteaResp struct {
	code int
	body string
	hdr  map[string]string
}

func newGiteaStub(t *testing.T, routes map[string][]giteaResp) (*giteaStub, *Gitea) {
	t.Helper()
	s := &giteaStub{routes: routes, seen: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.TrimPrefix(r.URL.EscapedPath(), "/api/v1")
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen[key] = string(body)
		s.auth = r.Header.Get("Authorization")
		queue, ok := s.routes[key]
		var resp giteaResp
		if ok && len(queue) > 0 {
			resp = queue[0]
			if len(queue) > 1 {
				s.routes[key] = queue[1:]
			}
		}
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found","url":"http://gitea/api/swagger"}`))
			return
		}
		for k, v := range resp.hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.code)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(srv.Close)
	return s, NewGitea(srv.URL+"/", "gitea-test-token")
}

func one(code int, body string) []giteaResp { return []giteaResp{{code: code, body: body}} }

func (s *giteaStub) body(t *testing.T, key string) map[string]any {
	t.Helper()
	s.mu.Lock()
	raw, ok := s.seen[key]
	s.mu.Unlock()
	if !ok {
		t.Fatalf("запроса %s не было", key)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("тело %s: %v", key, err)
	}
	return out
}

const giteaRepoJSON = `{"full_name":"own/proj","html_url":"http://gitea/own/proj","default_branch":"main","default_merge_style":"squash","empty":false,"permissions":{"admin":true,"push":true,"pull":true}}`

func TestGiteaProbe(t *testing.T) {
	ctx := context.Background()
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /user":                  one(200, `{"login":"rivet-bot"}`),
		"GET /repos/own/proj":        one(200, giteaRepoJSON),
		"POST /repos/own/proj/pulls": one(422, `{"message":"[Head]: Required"}`),
		"GET /repos/own/ro":          one(200, `{"full_name":"own/ro","default_branch":"main","permissions":{"push":false,"pull":true}}`),
		// Пользователь с правом записи, токен только на чтение (scope).
		"GET /repos/own/scoped":        one(200, `{"full_name":"own/scoped","default_branch":"main","permissions":{"push":true,"pull":true}}`),
		"POST /repos/own/scoped/pulls": one(403, `{"message":"token does not have at least one of required scope(s), required=[write:repository], token scope=read:repository,read:user"}`),
	})
	res := g.Probe(ctx, "own/proj")
	if !res.OK || res.TokenOwner != "rivet-bot" || res.RepoPath != "own/proj" || res.DefaultBranch != "main" || !res.CanPush || !res.CanMergeRequest {
		t.Fatalf("успешная проверка: %+v", res)
	}
	if s.auth != "token gitea-test-token" {
		t.Fatalf("токен уходит схемой token: %q", s.auth)
	}
	if res := g.Probe(ctx, ""); !res.OK || res.TokenOwner != "rivet-bot" {
		t.Fatalf("проверка только токена: %+v", res)
	}
	if res := g.Probe(ctx, "own/ro"); res.OK || res.Reason != ReasonNoScope {
		t.Fatalf("только чтение: %+v", res)
	}
	if res := g.Probe(ctx, "own/scoped"); res.OK || res.Reason != ReasonNoScope || !strings.Contains(res.Message, "write:repository") || res.CanPush {
		t.Fatalf("токен без scope записи: %+v", res)
	}
	if b := s.body(t, "POST /repos/own/proj/pulls"); len(b) != 0 {
		t.Fatalf("проверка scope шлёт пустое тело и PR не создаёт: %v", b)
	}
	// Запись подтверждает только 422: выключенные PR, архив и прочие ответы
	// — отказ с причиной.
	for code, want := range map[int]string{404: "pull request'ы", 423: "архиве", 500: "неожиданный ответ"} {
		_, gx := newGiteaStub(t, map[string][]giteaResp{
			"GET /user":                  one(200, `{"login":"rivet-bot"}`),
			"GET /repos/own/proj":        one(200, giteaRepoJSON),
			"POST /repos/own/proj/pulls": one(code, `{"message":"x"}`),
		})
		if res := gx.Probe(ctx, "own/proj"); res.OK || res.CanPush || !strings.Contains(res.Message, want) {
			t.Fatalf("ответ %d на проверку записи: %+v", code, res)
		}
	}
	if res := g.Probe(ctx, "own/nope"); res.OK || res.Reason != ReasonNotFound || res.TokenOwner != "rivet-bot" {
		t.Fatalf("нет репозитория: %+v", res)
	}
	_, bad := newGiteaStub(t, map[string][]giteaResp{
		"GET /user": one(401, `{"message":"user does not exist [uid: 0, name: ]"}`),
	})
	if res := bad.Probe(ctx, "own/proj"); res.OK || res.Reason != ReasonBadToken {
		t.Fatalf("токен не принят: %+v", res)
	}
	if res := NewGitea("http://127.0.0.1:1", "t").Probe(ctx, "own/proj"); res.OK || res.Reason != ReasonUnreachable {
		t.Fatalf("инстанс недоступен: %+v", res)
	}
}

func TestGiteaCreatePRAndDiff(t *testing.T) {
	ctx := context.Background()
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"POST /repos/own/proj/pulls":       one(201, `{"number":7,"html_url":"http://gitea/own/proj/pulls/7"}`),
		"GET /repos/own/proj/pulls/7.diff": one(200, "diff --git a/x b/x\n+x\n"),
		"GET /repos/own/proj/pulls/8.diff": one(200, strings.Repeat("x", MaxResponseBytes+10)),
	})
	pr, err := g.CreatePR(ctx, "own/proj", "agent/task-7", "main", "task-7: x", "описание")
	if err != nil || pr.Number != 7 || pr.URL != "http://gitea/own/proj/pulls/7" {
		t.Fatalf("PR: %+v %v", pr, err)
	}
	if b := s.body(t, "POST /repos/own/proj/pulls"); b["head"] != "agent/task-7" || b["base"] != "main" || b["title"] != "task-7: x" || b["body"] != "описание" {
		t.Fatalf("тело создания PR: %v", b)
	}
	if d, err := g.Diff(ctx, "own/proj", 7); err != nil || !strings.HasPrefix(d, "diff --git") {
		t.Fatalf("diff: %q %v", d, err)
	}
	if _, err := g.Diff(ctx, "own/proj", 8); !errors.Is(err, ErrDiffTruncated) {
		t.Fatalf("обрезанный diff: %v", err)
	}
	_, g409 := newGiteaStub(t, map[string][]giteaResp{
		"POST /repos/own/proj/pulls": one(409, `{"message":"pull request already exists for these targets"}`),
	})
	if _, err := g409.CreatePR(ctx, "own/proj", "b", "main", "t", ""); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("отказ хостинга с кодом: %v", err)
	}
}

func TestGiteaMerge(t *testing.T) {
	ctx := context.Background()
	defer func(v time.Duration) { giteaMergeWait = v }(giteaMergeWait)
	giteaMergeWait = 60 * time.Millisecond
	merged := `{"number":7,"state":"closed","merged":true,"mergeable":false,"merge_commit_sha":"abc123"}`
	cases := []struct {
		name     string
		merge    []giteaResp
		pr       []giteaResp
		head     string
		wantSHA  string
		conflict bool
		moved    bool
	}{
		{"merge способом репозитория", one(200, ``), one(200, merged), "", "abc123", false, false},
		{"вершина сдвинулась", one(409, `{"message":"head out of date"}`), nil, "sha-old", "", false, true},
		{"конфликт по выводу git", one(409, `{"Style":"merge","StdOut":"Auto-merging README.md\nCONFLICT (content): Merge conflict in README.md\n","StdErr":"","Err":{}}`), nil, "", "", true, false},
		{"409 без вывода git — не конфликт", one(409, `{"message":"Merge Conflicts"}`), nil, "", "", false, false},
		{"база сдвинулась во время merge, повтор", []giteaResp{
			{code: 409, body: `{"message":"PushRejected with remote message: cannot lock ref 'refs/heads/main'"}`}, {code: 200}},
			[]giteaResp{{code: 200, body: `{"number":7,"state":"open","mergeable":true}`}, {code: 200, body: merged}},
			"sha-head", "abc123", false, false},
		{"Forgejo: push отклонён, повтор", []giteaResp{
			{code: 409, body: `{"message":"PushRejected without remote error message"}`}, {code: 200}},
			[]giteaResp{{code: 200, body: `{"number":7,"state":"open","mergeable":true}`}, {code: 200, body: merged}},
			"", "abc123", false, false},
		{"push отклонён хуком инстанса — без повтора", []giteaResp{
			{code: 409, body: `{"message":"PushRejected with remote message: hook declined to update refs/heads/main"}`}, {code: 200}},
			nil, "", "", false, false},
		{"повторный 405 после ожидания — отказ с пояснением", []giteaResp{
			{code: 405, body: `{"message":"Please try again later"}`}},
			[]giteaResp{{code: 200, body: `{"number":7,"state":"open","mergeable":true}`}},
			"", "", false, false},
		{"пересчёт PR, затем merge", []giteaResp{{code: 405, body: `{"message":"Please try again later"}`}, {code: 200}},
			[]giteaResp{{code: 200, body: `{"number":7,"state":"open","mergeable":false}`}, {code: 200, body: `{"number":7,"state":"open","mergeable":true}`}, {code: 200, body: merged}},
			"", "abc123", false, false},
		{"не мержится — не конфликт", one(405, `{"message":"Please try again later"}`),
			one(200, `{"number":7,"state":"open","mergeable":false}`), "", "", false, false},
		{"нет прав — не конфликт", one(405, `{"message":"User not allowed to merge PR"}`), nil, "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routes := map[string][]giteaResp{
				"GET /repos/own/proj":                one(200, giteaRepoJSON),
				"POST /repos/own/proj/pulls/7/merge": tc.merge,
			}
			if tc.pr != nil {
				routes["GET /repos/own/proj/pulls/7"] = tc.pr
			}
			s, g := newGiteaStub(t, routes)
			sha, err := g.MergeAt(ctx, "own/proj", 7, tc.head)
			if tc.wantSHA != "" {
				if err != nil || sha != tc.wantSHA {
					t.Fatalf("merge: %q %v", sha, err)
				}
				b := s.body(t, "POST /repos/own/proj/pulls/7/merge")
				if b["Do"] != "squash" || b["delete_branch_after_merge"] != false {
					t.Fatalf("способ merge из настроек репозитория: %v", b)
				}
				if tc.head != "" && b["head_commit_id"] != tc.head {
					t.Fatalf("повтор merge с той же проверкой вершины: %v", b)
				}
				return
			}
			if err == nil {
				t.Fatal("ожидался отказ merge")
			}
			if strings.Contains(tc.name, "пояснением") && !strings.Contains(err.Error(), "конфликт с базовой веткой") {
				t.Fatalf("отказ без пояснения: %v", err)
			}
			if strings.Contains(tc.name, "хуком") && (strings.Contains(err.Error(), "конфликт с базовой веткой") || !strings.Contains(err.Error(), "hook declined")) {
				t.Fatalf("отказ хука — с текстом хостинга, без догадки о гонке: %v", err)
			}
			if errors.Is(err, ErrMergeConflict) != tc.conflict || errors.Is(err, ErrHeadMoved) != tc.moved {
				t.Fatalf("конфликт=%v сдвиг=%v: %v", errors.Is(err, ErrMergeConflict), errors.Is(err, ErrHeadMoved), err)
			}
			if tc.head != "" {
				if b := s.body(t, "POST /repos/own/proj/pulls/7/merge"); b["head_commit_id"] != tc.head {
					t.Fatalf("merge проверенной вершины: %v", b)
				}
			}
		})
	}
}

func TestGiteaHeadSHA(t *testing.T) {
	ctx := context.Background()
	_, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj/branches/agent/task-1": one(200, `{"name":"agent/task-1","commit":{"id":"sha-1"}}`),
		"GET /repos/own/proj/branches/nope":         one(404, `{"errors":null,"message":"The target couldn't be found."}`),
		"GET /repos/own/proj":                       one(200, giteaRepoJSON),
	})
	if sha, err := g.HeadSHA(ctx, "own/proj", "agent/task-1"); err != nil || sha != "sha-1" {
		t.Fatalf("вершина ветки со слэшем: %q %v", sha, err)
	}
	if _, err := g.HeadSHA(ctx, "own/proj", "nope"); !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("нет ветки: %v", err)
	}
	// Нет самого репозитория: это сбой хостинга, а не «нет ветки».
	if _, err := g.HeadSHA(ctx, "own/gone", "main"); err == nil || errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("нет репозитория: %v", err)
	}
}

func TestGiteaReadFile(t *testing.T) {
	ctx := context.Background()
	b64 := base64.StdEncoding.EncodeToString
	big := strings.Repeat("y", 3000)
	_, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj/contents/docs/a.md?ref=main": one(200, `{"type":"file","path":"docs/a.md","name":"a.md","sha":"blob-1","size":5,"encoding":"base64","content":"`+b64([]byte("hello"))+`"}`),
		// Gitea 28: причина в message.
		"GET /repos/own/proj/contents/nope.md?ref=main": one(404, `{"message":"object does not exist [id: , rel_path: nope.md]"}`),
		// Gitea 1.24: причина в errors.
		"GET /repos/own/proj/contents/old.md?ref=main":  one(404, `{"errors":["object does not exist [id: , rel_path: old.md]"],"message":"GetContentsOrList"}`),
		"GET /repos/own/proj/contents/a.md?ref=nosuch":  one(404, `{"message":"object does not exist [id: nosuch, rel_path: ]"}`),
		"GET /repos/own/gone/contents/a.md?ref=main":    one(404, `{"errors":null,"message":"The target couldn't be found."}`),
		"GET /repos/own/proj/contents/link?ref=main":    one(200, `{"type":"symlink","path":"link","target":"../etc/passwd","sha":"s","size":13}`),
		"GET /repos/own/proj/contents/docs?ref=main":    one(200, `[{"type":"file","name":"a.md"}]`),
		"GET /repos/own/proj/contents/big.txt?ref=main": one(200, `{"type":"file","path":"big.txt","sha":"blob-big","size":3000,"encoding":null,"content":null}`),
		"GET /repos/own/proj/git/blobs/blob-big":        one(200, `{"sha":"blob-big","size":3000,"encoding":"base64","content":"`+b64([]byte(big))+`"}`),
	})
	f, err := g.ReadFile(ctx, "own/proj", "main", "docs/a.md")
	if err != nil || f.Content != "hello" || f.FileID != "blob-1" {
		t.Fatalf("файл: %+v %v", f, err)
	}
	for _, p := range []string{"nope.md", "old.md"} {
		if _, err := g.ReadFile(ctx, "own/proj", "main", p); !errors.Is(err, ErrFileNotFound) {
			t.Fatalf("%s: нет файла: %v", p, err)
		}
	}
	if _, err := g.ReadFile(ctx, "own/proj", "nosuch", "a.md"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("нет версии: %v", err)
	}
	if _, err := g.ReadFile(ctx, "own/gone", "main", "a.md"); !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("нет репозитория — не «нет файла»: %v", err)
	}
	if _, err := g.ReadFile(ctx, "own/proj", "main", "link"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("символическая ссылка: %v", err)
	}
	if _, err := g.ReadFile(ctx, "own/proj", "main", "docs"); err == nil || errors.Is(err, ErrFileNotFound) {
		t.Fatalf("каталог: %v", err)
	}
	if f, err := g.ReadFile(ctx, "own/proj", "main", "big.txt"); err != nil || f.Content != big || f.FileID != "blob-big" {
		t.Fatalf("большой файл через блоб: %d %v", len(f.Content), err)
	}
}

func TestGiteaWriteFile(t *testing.T) {
	ctx := context.Background()
	resp := `{"content":{"sha":"blob-2"},"commit":{"sha":"commit-2","html_url":"http://gitea/own/proj/commit/commit-2"}}`
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"POST /repos/own/proj/contents/deploy/values.yaml": one(201, resp),
		"PUT /repos/own/proj/contents/deploy/values.yaml":  one(200, resp),
	})
	c, err := g.WriteFile(ctx, "own/proj", "main", "deploy/values.yaml", "", "a: 1\n", "msg")
	if err != nil || c.SHA != "commit-2" || c.URL == "" {
		t.Fatalf("новый файл: %+v %v", c, err)
	}
	if b := s.body(t, "POST /repos/own/proj/contents/deploy/values.yaml"); b["branch"] != "main" || b["sha"] != nil || b["message"] != "msg" {
		t.Fatalf("создание файла: %v", b)
	}
	if _, err := g.WriteFile(ctx, "own/proj", "main", "deploy/values.yaml", "blob-1", "a: 2\n", "msg"); err != nil {
		t.Fatal(err)
	}
	b := s.body(t, "PUT /repos/own/proj/contents/deploy/values.yaml")
	if b["sha"] != "blob-1" {
		t.Fatalf("обновление по sha прочитанной версии: %v", b)
	}
	if got, _ := base64.StdEncoding.DecodeString(b["content"].(string)); string(got) != "a: 2\n" {
		t.Fatalf("содержимое: %q", got)
	}
}

func TestGiteaBranchProtected(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		rules  string
		branch string
		want   bool
	}{
		{"одобрение и запрет push", `[{"rule_name":"main","required_approvals":1,"enable_push":false}]`, "main", true},
		{"push разрешён всем с правом записи", `[{"rule_name":"main","required_approvals":1,"enable_push":true}]`, "main", false},
		{"белый список без бота", `[{"rule_name":"main","required_approvals":1,"enable_push":true,"enable_push_whitelist":true,"push_whitelist_usernames":["alice"]}]`, "main", true},
		{"бот в белом списке", `[{"rule_name":"main","required_approvals":1,"enable_push":true,"enable_push_whitelist":true,"push_whitelist_usernames":["Rivet-Bot"]}]`, "main", false},
		{"белый список команд проверить нельзя", `[{"rule_name":"main","required_approvals":1,"enable_push":true,"enable_push_whitelist":true,"push_whitelist_teams":["devs"]}]`, "main", false},
		{"без одобрений", `[{"rule_name":"main","required_approvals":0,"enable_push":false}]`, "main", false},
		// Gitea применяет одно из подходящих правил, порядок у версий разный:
		// защищена, только если защищает каждое подходящее.
		{"точное правило без одобрений и шаблон с одобрением", `[{"rule_name":"main","required_approvals":0},{"rule_name":"*","required_approvals":1}]`, "main", false},
		{"шаблон", `[{"rule_name":"release/*","required_approvals":2}]`, "release/1.0", true},
		{"шаблон не переходит через /", `[{"rule_name":"release/*","required_approvals":2}]`, "release/a/b", false},
		{"правило старой формы", `[{"branch_name":"legacy","required_approvals":1}]`, "legacy", true},
		{"нет подходящего правила", `[{"rule_name":"main","required_approvals":1}]`, "feature", false},
		// Точное имя Gitea сравнивает без учёта регистра.
		{"мягкое правило в другом регистре", `[{"rule_name":"Main","required_approvals":0,"enable_push":true},{"rule_name":"*","required_approvals":1}]`, "main", false},
		{"строгое правило в другом регистре", `[{"rule_name":"MAIN","required_approvals":1}]`, "main", true},
		// Шаблон тоже без учёта регистра.
		{"мягкий шаблон в другом регистре", `[{"rule_name":"Main*","required_approvals":0,"enable_push":true},{"rule_name":"*","required_approvals":1}]`, "main", false},
		{"строгий шаблон в другом регистре", `[{"rule_name":"Release/*","required_approvals":1}]`, "release/1", true},
		// Синтаксис gobwas, которого path.Match не знает, подходит к любой ветке.
		{"мягкое правило-шаблон с {}", `[{"rule_name":"{main,dev}","required_approvals":0,"enable_push":true},{"rule_name":"*","required_approvals":1}]`, "main", false},
		{"мягкое правило-шаблон с **", `[{"rule_name":"release/**","required_approvals":0},{"rule_name":"*","required_approvals":1}]`, "main", false},
		// Файлы из unprotected_file_patterns меняются прямым push.
		{"незащищённые файлы", `[{"rule_name":"main","required_approvals":1,"enable_push":false,"unprotected_file_patterns":".rivet/**"}]`, "main", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, g := newGiteaStub(t, map[string][]giteaResp{
				"GET /repos/own/proj/branch_protections": one(200, tc.rules),
				"GET /user":                              one(200, `{"login":"rivet-bot"}`),
			})
			if got, err := g.BranchProtected(ctx, "own/proj", tc.branch); err != nil || got != tc.want {
				t.Fatalf("%v %v, ожидалось %v", got, err, tc.want)
			}
		})
	}
	// Белый список не сверить без логина бота: сбой — ошибка, а не «не защищена».
	_, noUser := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj/branch_protections": one(200, `[{"rule_name":"main","required_approvals":1,"enable_push":true,"enable_push_whitelist":true,"push_whitelist_usernames":["alice"]}]`),
		"GET /user":                              one(502, `bad gateway`),
	})
	if got, err := noUser.BranchProtected(ctx, "own/proj", "main"); err == nil || got {
		t.Fatalf("сбой чтения бота — ошибка: %v %v", got, err)
	}
	_, noRights := newGiteaStub(t, map[string][]giteaResp{"GET /repos/own/proj/branch_protections": one(403, `{"message":"forbidden"}`)})
	if got, err := noRights.BranchProtected(ctx, "own/proj", "main"); err != nil || got {
		t.Fatalf("нет прав — false без ошибки: %v %v", got, err)
	}
}

func TestGiteaRegisterWebhook(t *testing.T) {
	ctx := context.Background()
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj":                       one(200, giteaRepoJSON),
		"GET /repos/own/proj/hooks?limit=50&page=1": {{code: 200, body: `[{"id":3,"config":{"url":"https://other/hook"}}]`, hdr: map[string]string{"X-Total-Count": "1"}}},
		"POST /repos/own/proj/hooks":                one(201, `{"id":4}`),
	})
	ok, err := g.RegisterWebhook(ctx, "own/proj", "https://rivet/api/v1/webhooks/gitea", "hook-secret")
	if err != nil || !ok {
		t.Fatalf("регистрация: %v %v", ok, err)
	}
	b := s.body(t, "POST /repos/own/proj/hooks")
	cfg, _ := b["config"].(map[string]any)
	if b["type"] != "gitea" || b["active"] != true || b["branch_filter"] != "main" ||
		cfg["url"] != "https://rivet/api/v1/webhooks/gitea" || cfg["secret"] != "hook-secret" || cfg["content_type"] != "json" {
		t.Fatalf("тело хука: %v", b)
	}
	events, _ := b["events"].([]any)
	want := map[string]bool{"push": true, "pull_request": true, "pull_request_review": true, "workflow_run": true}
	for _, e := range events {
		delete(want, e.(string))
	}
	if len(want) != 0 {
		t.Fatalf("не подписаны события %v: %v", want, events)
	}

	// Хук с нашим URL уже есть: обновляется с новым секретом и фильтром.
	s2, g2 := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj":                       one(200, giteaRepoJSON),
		"GET /repos/own/proj/hooks?limit=50&page=1": one(200, `[{"id":5,"config":{"url":"https://rivet/api/v1/webhooks/gitea"}}]`),
		"PATCH /repos/own/proj/hooks/5":             one(200, `{"id":5}`),
	})
	if ok, err := g2.RegisterWebhook(ctx, "own/proj", "https://rivet/api/v1/webhooks/gitea", "new-secret"); err != nil || !ok {
		t.Fatalf("обновление: %v %v", ok, err)
	}
	if b := s2.body(t, "PATCH /repos/own/proj/hooks/5"); b["branch_filter"] != "main" || b["config"].(map[string]any)["secret"] != "new-secret" {
		t.Fatalf("PATCH обновляет секрет и фильтр: %v", b)
	}

	// Нет прав на хуки — подключение не блокируется.
	_, g3 := newGiteaStub(t, map[string][]giteaResp{"GET /repos/own/proj/hooks?limit=50&page=1": one(403, `{"message":"forbidden"}`)})
	if ok, err := g3.RegisterWebhook(ctx, "own/proj", "https://rivet/hook", "s"); err != nil || ok {
		t.Fatalf("нет прав: %v %v", ok, err)
	}
}

// Хук ищется по всем страницам списка: с не первой страницы он
// обновляется, а не дублируется.
func TestGiteaWebhookOnSecondPage(t *testing.T) {
	ctx := context.Background()
	full := make([]map[string]any, giteaHooksPage)
	for i := range full {
		full[i] = map[string]any{"id": i + 1, "config": map[string]string{"url": "https://other/" + string(rune('a'+i%26))}}
	}
	raw, _ := json.Marshal(full)
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj":                       one(200, giteaRepoJSON),
		"GET /repos/own/proj/hooks?limit=50&page=1": one(200, string(raw)),
		"GET /repos/own/proj/hooks?limit=50&page=2": one(200, `[{"id":77,"config":{"url":"https://rivet/api/v1/webhooks/gitea"}}]`),
		"PATCH /repos/own/proj/hooks/77":            one(200, `{"id":77}`),
	})
	if ok, err := g.RegisterWebhook(ctx, "own/proj", "https://rivet/api/v1/webhooks/gitea", "s2"); err != nil || !ok {
		t.Fatalf("обновление хука со второй страницы: %v %v", ok, err)
	}
	s.mu.Lock()
	_, created := s.seen["POST /repos/own/proj/hooks"]
	s.mu.Unlock()
	if created {
		t.Fatal("второй хук с тем же URL не создаётся")
	}
}

// Инстанс отдаёт меньше хуков на страницу, чем просили (MAX_RESPONSE_ITEMS):
// короткая страница не последняя, поиск идёт до пустой.
func TestGiteaWebhookSmallPages(t *testing.T) {
	ctx := context.Background()
	page := make([]map[string]any, 30)
	for i := range page {
		page[i] = map[string]any{"id": i + 1, "config": map[string]string{"url": fmt.Sprintf("https://other/%d", i)}}
	}
	raw, _ := json.Marshal(page)
	_, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj":                       one(200, giteaRepoJSON),
		"GET /repos/own/proj/hooks?limit=50&page=1": one(200, string(raw)),
		"GET /repos/own/proj/hooks?limit=50&page=2": one(200, `[{"id":99,"config":{"url":"https://rivet/api/v1/webhooks/gitea"}}]`),
		"PATCH /repos/own/proj/hooks/99":            one(200, `{"id":99}`),
	})
	if ok, err := g.RegisterWebhook(ctx, "own/proj", "https://rivet/api/v1/webhooks/gitea", "s"); err != nil || !ok {
		t.Fatalf("хук со второй короткой страницы: %v %v", ok, err)
	}
}

func TestGiteaPRFiles(t *testing.T) {
	ctx := context.Background()
	page := func(n int, more string) giteaResp {
		files := make([]map[string]string, n)
		for i := range files {
			files[i] = map[string]string{"filename": "docs/f" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + ".md"}
		}
		raw, _ := json.Marshal(files)
		return giteaResp{code: 200, body: string(raw), hdr: map[string]string{"X-HasMore": more}}
	}
	_, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj/pulls/3":                       one(200, `{"number":3,"changed_files":52,"head":{"sha":"head-3"}}`),
		"GET /repos/own/proj/pulls/3/files?limit=50&page=1": {page(50, "true")},
		"GET /repos/own/proj/pulls/3/files?limit=50&page=2": {page(2, "false")},
		"GET /repos/own/proj/pulls/4":                       one(200, `{"number":4,"changed_files":2,"head":{"sha":"head-4"}}`),
		"GET /repos/own/proj/pulls/4/files?limit=50&page=1": one(200, `[{"filename":"b.go","previous_filename":"a.go"}]`),
	})
	l, err := g.PRFiles(ctx, "own/proj", 3)
	if err != nil || l.HeadSHA != "head-3" || len(l.Files) == 0 {
		t.Fatalf("файлы PR: %+v %v", l, err)
	}
	defer func(v time.Duration) { prFilesRetryPause = v }(prFilesRetryPause)
	prFilesRetryPause = time.Millisecond
	if _, err := g.PRFiles(ctx, "own/proj", 4); !errors.Is(err, ErrPRFilesTruncated) {
		t.Fatalf("неполный список: %v", err)
	}
}

func TestGiteaPathSHA(t *testing.T) {
	ctx := context.Background()
	_, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /repos/own/proj/contents/docs?ref=sha-1": one(200, `[{"name":"architecture","type":"dir","sha":"tree-1"},{"name":"x.md","type":"file","sha":"b"}]`),
		"GET /repos/own/proj/contents/docs?ref=sha-2": one(404, `{"message":"object does not exist [id: sha-2, rel_path: docs]"}`),
	})
	if sha, err := g.PathSHA(ctx, "own/proj", "sha-1", "docs/architecture"); err != nil || sha != "tree-1" {
		t.Fatalf("дерево каталога: %q %v", sha, err)
	}
	if sha, err := g.PathSHA(ctx, "own/proj", "sha-2", "docs/architecture"); err != nil || sha != "" {
		t.Fatalf("нет каталога: %q %v", sha, err)
	}
}

func TestGiteaPipeline(t *testing.T) {
	ctx := context.Background()
	since := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"POST /repos/own/proj/actions/workflows/deploy.yml/dispatches": one(204, ``),
		"GET /repos/own/proj/actions/workflows/deploy.yml/runs?branch=main&event=workflow_dispatch&limit=10": one(200,
			`{"workflow_runs":[{"id":12,"html_url":"http://gitea/own/proj/actions/runs/12","status":"in_progress","created_at":"2026-11-07T10:00:05Z"},
			{"id":11,"status":"completed","conclusion":"success","created_at":"2026-11-07T09:00:00Z"}],"total_count":2}`),
		"GET /repos/own/proj/actions/runs/12": one(200, `{"id":12,"html_url":"http://gitea/own/proj/actions/runs/12","status":"completed","conclusion":"failure"}`),
	})
	run, err := g.TriggerPipeline(ctx, "own/proj", "deploy.yml", "main", map[string]string{"version": "abc"})
	if err != nil || run.State != PipelineStarting {
		t.Fatalf("запуск: %+v %v", run, err)
	}
	if b := s.body(t, "POST /repos/own/proj/actions/workflows/deploy.yml/dispatches"); b["ref"] != "main" || b["inputs"].(map[string]any)["version"] != "abc" {
		t.Fatalf("тело запуска: %v", b)
	}
	run, err = g.PipelineRun(ctx, "own/proj", "deploy.yml", "main", "", since)
	if err != nil || run.RunID != "12" || run.State != PipelineRunning || run.URL == "" {
		t.Fatalf("свежий прогон: %+v %v", run, err)
	}
	run, err = g.PipelineRun(ctx, "own/proj", "deploy.yml", "main", "12", since)
	if err != nil || run.State != PipelineFailed {
		t.Fatalf("прогон по id: %+v %v", run, err)
	}
	if _, err := g.TriggerPipeline(ctx, "own/proj", "", "main", nil); err == nil {
		t.Fatal("без файла workflow запуск отклоняется")
	}
}

func TestGiteaCreateRepo(t *testing.T) {
	ctx := context.Background()
	created := `{"full_name":"%s/svc","html_url":"http://gitea/%s/svc","default_branch":"main"}`
	s, g := newGiteaStub(t, map[string][]giteaResp{
		"GET /user":             one(200, `{"login":"rivet-bot"}`),
		"POST /user/repos":      one(201, strings.ReplaceAll(created, "%s", "rivet-bot")),
		"POST /orgs/team/repos": one(201, strings.ReplaceAll(created, "%s", "team")),
	})
	info, err := g.CreateRepo(ctx, NewRepo{Name: "svc", Private: true})
	if err != nil || info.Path != "rivet-bot/svc" || info.DefaultBranch != "main" {
		t.Fatalf("личный репозиторий: %+v %v", info, err)
	}
	if b := s.body(t, "POST /user/repos"); b["auto_init"] != true || b["private"] != true || b["default_branch"] != "main" {
		t.Fatalf("репозиторий с начальным коммитом: %v", b)
	}
	if info, err := g.CreateRepo(ctx, NewRepo{Owner: "team", Name: "svc"}); err != nil || info.Path != "team/svc" {
		t.Fatalf("репозиторий организации: %+v %v", info, err)
	}
	_, taken := newGiteaStub(t, map[string][]giteaResp{
		"GET /user":        one(200, `{"login":"rivet-bot"}`),
		"POST /user/repos": one(409, `{"message":"The repository with the same name already exists."}`),
	})
	if _, err := taken.CreateRepo(ctx, NewRepo{Name: "svc"}); !errors.Is(err, ErrRepoExists) {
		t.Fatalf("имя занято: %v", err)
	}
}

// Fabric строит адаптер Gitea по инстансу проекта; без инстанса — отказ.
func TestFactoryGitea(t *testing.T) {
	f := &Factory{}
	a, err := f.For(ProviderGitea, "https://gitea.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := a.(*Gitea); !ok || g.Base != "https://gitea.example.com" {
		t.Fatalf("адаптер: %#v", a)
	}
	if _, err := f.For(ProviderGitea, "", "tok"); err == nil {
		t.Fatal("Gitea без URL инстанса")
	}
	var _ PRFileLister = a.(*Gitea)
	var _ SHAMerger = a.(*Gitea)
	var _ TreeHasher = a.(*Gitea)
}

func TestGiteaMissing(t *testing.T) {
	for _, tc := range []struct {
		body    string
		id, rel string
		ok      bool
	}{
		{`{"message":"object does not exist [id: , rel_path: a/b.md]"}`, "", "a/b.md", true},
		{`{"errors":["object does not exist [id: main, rel_path: ]"],"message":"x"}`, "main", "", true},
		{`{"message":"not found"}`, "", "", false},
		{`не json`, "", "", false},
	} {
		id, rel, ok := giteaMissing([]byte(tc.body))
		if id != tc.id || rel != tc.rel || ok != tc.ok {
			t.Fatalf("%s: %q %q %v", tc.body, id, rel, ok)
		}
	}
}
