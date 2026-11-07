package scm

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		name, in string
		provider Provider
		base     string
		path     string
	}{
		{"github", "https://github.com/owner/name", ProviderGitHub, "https://github.com", "owner/name"},
		{"github .git", "https://github.com/owner/name.git", ProviderGitHub, "https://github.com", "owner/name"},
		{"github с хвостом", "https://github.com/owner/name/tree/main", ProviderGitHub, "https://github.com", "owner/name/tree/main"},
		{"gitlab.com", "https://gitlab.com/group/name", ProviderGitLab, "https://gitlab.com", "group/name"},
		{"gitlab вложенные группы", "https://gitlab.com/group/sub/name", ProviderGitLab, "https://gitlab.com", "group/sub/name"},
		{"gitlab с /-/", "https://gitlab.com/group/sub/name/-/tree/main", ProviderGitLab, "https://gitlab.com", "group/sub/name"},
		{"self-hosted без провайдера", "https://git.example.com/team/svc", "", "https://git.example.com", "team/svc"},
		{"порт сохраняется", "https://git.example.com:8443/team/svc", "", "https://git.example.com:8443", "team/svc"},
		{"http для внутренней сети", "http://git.internal/team/svc", "", "http://git.internal", "team/svc"},
		{"регистр хоста", "https://GitHub.com/Owner/Name", ProviderGitHub, "https://github.com", "Owner/Name"},
		{"хвостовой слэш", "https://github.com/owner/name/", ProviderGitHub, "https://github.com", "owner/name"},
		{"codeberg", "https://codeberg.org/owner/name.git", ProviderGitea, "https://codeberg.org", "owner/name"},
		{"gitea.com с хвостом страницы", "https://gitea.com/owner/name/src/branch/main", ProviderGitea, "https://gitea.com", "owner/name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, err := ParseRepoURL(c.in)
			if err != nil {
				t.Fatalf("%q: %v", c.in, err)
			}
			if ref.Provider != c.provider || ref.BaseURL != c.base || ref.Path != c.path {
				t.Fatalf("%q -> %+v, ожидалось %s/%s/%s", c.in, ref, c.provider, c.base, c.path)
			}
		})
	}
}

func TestParseRepoURLRejects(t *testing.T) {
	bad := []struct{ name, in string }{
		{"пусто", ""},
		{"без схемы", "github.com/owner/name"},
		{"ssh", "git@github.com:owner/name.git"},
		{"file", "file:///tmp/repo"},
		{"userinfo", "https://user:token@github.com/owner/name"},
		{"только владелец", "https://github.com/owner"},
		{"без хоста", "https:///owner/name"},
		{"двойной слэш в пути", "https://github.com/owner//name"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if ref, err := ParseRepoURL(c.in); err == nil {
				t.Fatalf("%q должен отклоняться, получено %+v", c.in, ref)
			}
		})
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	got, err := NormalizeBaseURL("https://GitLab.example.com/")
	if err != nil || got != "https://gitlab.example.com" {
		t.Fatalf("%q, %v", got, err)
	}
	for _, in := range []string{"", "ftp://x", "https://user:pw@host", "not a url at all::"} {
		if _, err := NormalizeBaseURL(in); err == nil {
			t.Fatalf("%q должен отклоняться", in)
		}
	}
}

func TestWebURL(t *testing.T) {
	ref, err := ParseRepoURL("https://gitlab.example.com/group/sub/name.git")
	if err != nil {
		t.Fatal(err)
	}
	if got := ref.WebURL(); got != "https://gitlab.example.com/group/sub/name" {
		t.Fatalf("WebURL = %q", got)
	}
}

// FuzzParseRepoURL — разбор внешнего ввода: паника недопустима, успешный
// разбор обязан давать непустые инстанс и путь без ведущих слэшей.
func FuzzParseRepoURL(f *testing.F) {
	for _, s := range []string{
		"https://github.com/owner/name",
		"https://gitlab.com/a/b/c.git",
		"http://x:1/o/n",
		"https://user@host/o/n",
		"://",
		"https://github.com/owner/name/-/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		ref, err := ParseRepoURL(raw)
		if err != nil {
			return
		}
		if ref.BaseURL == "" || ref.Path == "" {
			t.Fatalf("успешный разбор с пустыми полями: %q -> %+v", raw, ref)
		}
		if ref.Path[0] == '/' || ref.Path[len(ref.Path)-1] == '/' {
			t.Fatalf("путь со слэшем по краям: %q -> %q", raw, ref.Path)
		}
		if ref.Provider != "" && !ValidProvider(string(ref.Provider)) {
			t.Fatalf("неизвестный провайдер: %q -> %q", raw, ref.Provider)
		}
	})
}

func TestGiteaRepoPath(t *testing.T) {
	for in, want := range map[string]string{
		"owner/name":                  "owner/name",
		"/owner/name.git/":            "owner/name",
		"owner/name/pulls/3":          "owner/name",
		"owner/name/src/branch/main/": "owner/name",
	} {
		if got, err := GiteaRepoPath(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v, ожидалось %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "owner", "owner/", "/name", "owner/.git", "//"} {
		if got, err := GiteaRepoPath(bad); err == nil {
			t.Fatalf("%q должен отклоняться, получено %q", bad, got)
		}
	}
}

// Инстанс Gitea в подкаталоге находится вопросом к самому инстансу, а не
// по словам адреса: хвост страницы любой, репозиторий может называться
// wiki или src.
func TestLocateGitea(t *testing.T) {
	ctx := context.Background()
	version := func(prefix string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == prefix+"/api/v1/version" {
				_, _ = w.Write([]byte(`{"version":"28.3.0"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	root, sub, none := version(""), version("/gitea"), version("/nowhere")
	for _, tc := range []struct {
		srv        *httptest.Server
		path       string
		base, repo string
	}{
		{root, "team/svc", "", "team/svc"},
		{root, "team/svc/labels", "", "team/svc"},
		{root, "team/svc/milestones", "", "team/svc"},
		{root, "team/svc/packages", "", "team/svc"},
		{root, "team/svc/forks", "", "team/svc"},
		{root, "team/svc/blame/branch/main/a.go", "", "team/svc"},
		{root, "team/svc/find/branch/main", "", "team/svc"},
		{root, "team/svc.git", "", "team/svc"},
		{sub, "gitea/team/wiki", "/gitea", "team/wiki"},
		{sub, "gitea/platform/src", "/gitea", "platform/src"},
		{sub, "gitea/team/releases/tag/v1", "/gitea", "team/releases"},
		{sub, "gitea/team/svc/src/branch/main", "/gitea", "team/svc"},
		// Адрес глубокого файла: ветка feature/x/y и путь пакета. Хвост
		// не разбирается, длина адреса не ограничена.
		{sub, "gitea/team/svc/src/branch/feature/x/y/src/main/java/com/example/app/service/impl/Handler.java", "/gitea", "team/svc"},
		{root, "team/svc/src/branch/main/" + strings.Repeat("a/", 5000) + "b.go", "", "team/svc"},
		// Хвост страницы с пустыми сегментами или «..» не мешает.
		{root, "team/svc/src/branch/main//a/../b", "", "team/svc"},
		// Инстанс не ответил как Gitea: корень и первые два сегмента.
		{none, "team/svc/pulls/3", "", "team/svc"},
	} {
		base, repo, found, err := LocateGitea(ctx, tc.srv.URL, tc.path)
		if err != nil || base != tc.srv.URL+tc.base || repo != tc.repo || found != (tc.srv != none) {
			t.Fatalf("%q -> %q %q %v %v, ожидалось %q %q", tc.path, base, repo, found, err, tc.srv.URL+tc.base, tc.repo)
		}
	}
	for _, bad := range []string{"", "team", "../team/svc", "team/./svc", "a//team/svc", "team/.git", "a/b/c//team/svc"} {
		if b, r, _, err := LocateGitea(ctx, root.URL, bad); err == nil {
			t.Fatalf("%q должен отклоняться: %q %q", bad, b, r)
		}
	}
	// Gitea с обязательным входом отвечает на API без токена 403 со своим
	// текстом — это тоже Gitea.
	signin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/git/api/v1/version" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Only signed in user is allowed to call APIs.","url":"x"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(signin.Close)
	if base, repo, found, err := LocateGitea(ctx, signin.URL, "git/team/svc"); err != nil || base != signin.URL+"/git" || repo != "team/svc" || !found {
		t.Fatalf("Gitea с обязательным входом: %q %q %v %v", base, repo, found, err)
	}
	// API подкаталога закрыт авторизацией прокси: инстанс не опознан,
	// запасной вариант помечен.
	proxied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(proxied.Close)
	if base, repo, found, err := LocateGitea(ctx, proxied.URL, "gitea/team/svc"); err != nil || base != proxied.URL || repo != "gitea/team" || found {
		t.Fatalf("закрытый прокси: %q %q %v %v", base, repo, found, err)
	}
	if got, err := NormalizeInstanceURL("https://Git.Example.com/gitea/"); err != nil || got != "https://git.example.com/gitea" {
		t.Fatalf("инстанс с подкаталогом: %q %v", got, err)
	}
	if got, err := NormalizeInstanceURL("https://git.example.com"); err != nil || got != "https://git.example.com" {
		t.Fatalf("инстанс без подкаталога: %q %v", got, err)
	}
	if _, err := NormalizeInstanceURL("https://git.example.com/a/../b"); err == nil {
		t.Fatal("путь инстанса с .. отклоняется")
	}
}

// Признак Gitea — точная форма ответа самого кандидата: чужой сервис с
// похожим полем, лишние ключи и редирект не принимаются.
func TestLocateGiteaStrictAnswer(t *testing.T) {
	ctx := context.Background()
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"28.3.0"}`))
	}))
	t.Cleanup(gitea.Close)
	for name, body := range map[string]string{
		"ключ в другом регистре": `{"Version":"2.9.0"}`,
		"лишние ключи":           `{"version":"2.9.0","git_version":"x"}`,
		"пустая версия":          `{"version":""}`,
		"не объект":              `["version"]`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/version" {
				_, _ = w.Write([]byte(body))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		if giteaAnswers(ctx, srv.URL) {
			t.Fatalf("%s: чужой ответ принят за Gitea", name)
		}
		srv.Close()
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, gitea.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	if giteaAnswers(ctx, redirect.URL) {
		t.Fatal("ответ после редиректа на другой хост не принимается")
	}
	if !giteaAnswers(ctx, gitea.URL) {
		t.Fatal("настоящий ответ Gitea")
	}
}

// Поиск подкаталога ограничен: не больше giteaLocatePrefixes запросов и
// общий срок, иначе адрес с длинным путём или молчащий хост держали бы
// подключение и слали лавину запросов.
func TestLocateGiteaBounded(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(counting.Close)
	path := strings.TrimSuffix(strings.Repeat("s/", 100), "/")
	base, repo, found, err := LocateGitea(ctx, counting.URL, path)
	if err != nil || base != counting.URL || repo != "s/s" || found {
		t.Fatalf("запасной вариант: %q %q %v %v", base, repo, found, err)
	}
	if n := hits.Load(); n > int32(giteaLocatePrefixes) {
		t.Fatalf("запросов к хосту не больше %d: %d", giteaLocatePrefixes, n)
	}
	// Хост молчит: весь поиск укладывается в общий срок.
	defer func(total, one time.Duration) { giteaLocateTotal, giteaLocateTimeout = total, one }(giteaLocateTotal, giteaLocateTimeout)
	giteaLocateTotal, giteaLocateTimeout = 300*time.Millisecond, 200*time.Millisecond
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hang.Close() })
	start := time.Now()
	if _, repo, found, err := LocateGitea(ctx, hang.URL, "a/b/c/d/e/f"); err != nil || repo != "a/b" || found {
		t.Fatalf("молчащий хост: %q %v %v", repo, found, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("поиск дольше общего срока: %v", d)
	}
}

// FuzzGiteaParse — разбор недоверенных ответов и адресов Gitea
// (add-gitea-adapter): путь репозитория всегда ровно owner/name, причина
// 404 разбирается без паники и только из строки Gitea.
func FuzzGiteaParse(f *testing.F) {
	f.Add("owner/name/src/branch/main", []byte(`{"message":"object does not exist [id: , rel_path: a.md]"}`))
	f.Add("o/n.git", []byte(`{"errors":["object does not exist [id: main, rel_path: ]"]}`))
	f.Add("/", []byte(`{"message":"not found"}`))
	f.Add("a//b", []byte(`object does not exist [id: ]`))
	f.Fuzz(func(t *testing.T, path string, body []byte) {
		if got, err := GiteaRepoPath(path); err == nil {
			parts := strings.Split(got, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				t.Fatalf("%q -> %q: путь не owner/name", path, got)
			}
		}
		if segs, err := giteaPathSegments(path); err == nil {
			if len(segs) < 2 || len(segs) > giteaLocatePrefixes+1 {
				t.Fatalf("%q: %d сегментов, нужно от 2 до %d", path, len(segs), giteaLocatePrefixes+1)
			}
			for _, s := range segs {
				if s == "" || s == "." || s == ".." {
					t.Fatalf("%q: недопустимый сегмент %q прошёл", path, s)
				}
			}
		}
		_ = giteaRuleMatches(path, "main")
		_ = giteaMergeConflict(body)
		id, rel, ok := giteaMissing(body)
		if !ok && (id != "" || rel != "") {
			t.Fatalf("без строки Gitea разбор вернул данные: %q %q", id, rel)
		}
		if ok && !bytes.Contains(body, []byte("object does not exist")) {
			t.Fatalf("причина найдена в чужом ответе: %q", body)
		}
	})
}
