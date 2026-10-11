package scm

import "testing"

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
