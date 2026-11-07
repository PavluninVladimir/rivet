package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Ссылка на исходники (спека source-availability): тег, иначе коммит, у
// сборки без версии корень, RIVET_SOURCE_URL меняет оба адреса.
func TestNewAbout(t *testing.T) {
	const src = "https://github.com/PavluninVladimir/rivet"
	const sha = "ad10d8e3f1c2b4a5968778695a4b3c2d1e0f9a8b"
	cases := []struct {
		name                     string
		version, commit, over    string
		wantVersion, wantSrc     string
		wantRef, wantCommitField string
	}{
		{"тег", "v0.12.0", sha, "", "v0.12.0", src, src + "/tree/v0.12.0", sha},
		{"тег предрелиза", "v1.0.0-rc.1", sha, "", "v1.0.0-rc.1", src, src + "/tree/v1.0.0-rc.1", sha},
		{"между тегами: коммит", "v0.12.0-3-gad10d8e", sha, "", "v0.12.0-3-gad10d8e", src, src + "/tree/" + sha, sha},
		{"грязное дерево: коммит", "v0.12.0-dirty", sha, "", "v0.12.0-dirty", src, src + "/tree/" + sha, sha},
		{"git describe без тегов: коммит", "ad10d8e", sha, "", "ad10d8e", src, src + "/tree/" + sha, sha},
		{"dev без коммита: корень", "dev", "", "", "dev", src, src, ""},
		{"пустая версия: dev", "", "", "", "dev", src, src, ""},
		{"dev с коммитом: коммит", "dev", sha, "", "dev", src, src + "/tree/" + sha, sha},
		{"не SHA в коммите отбрасывается", "dev", "main; rm -rf", "", "dev", src, src, ""},
		{"форк", "v0.12.0", sha, "https://git.example.org/acme/rivet/", "v0.12.0", "https://git.example.org/acme/rivet", "https://git.example.org/acme/rivet/tree/v0.12.0", sha},
		{"форк, dev", "dev", "", "https://git.example.org/acme/rivet", "dev", "https://git.example.org/acme/rivet", "https://git.example.org/acme/rivet", ""},
		{"версия без v не тег: коммит", "1.2.3", sha, "", "1.2.3", src, src + "/tree/" + sha, sha},
		{"адрес с токеном не отдаётся", "dev", "", "https://oauth2:glpat-fake@gitlab.example.org/acme/rivet", "dev", src, src, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NewAbout(c.version, c.commit, src, c.over)
			if a.Version != c.wantVersion || a.SourceURL != c.wantSrc || a.SourceRefURL != c.wantRef ||
				a.Commit != c.wantCommitField || a.License != "AGPL-3.0-only" {
				t.Fatalf("%+v", a)
			}
		})
	}
}

// Адрес из сборки (-X main.sourceURL) проверяется так же, как настройка:
// без схемы, с учётными данными или пустой он заменяется публичным
// репозиторием, иначе ссылки не было бы вовсе.
func TestNewAboutRejectsBadBuildURL(t *testing.T) {
	for _, bad := range []string{"", "github.com/acme/rivet", "javascript:alert(1)", "https://user:token@github.com/acme/rivet"} {
		if a := NewAbout("dev", "", bad, ""); a.SourceURL != DefaultSourceURL || a.SourceRefURL != DefaultSourceURL {
			t.Fatalf("%q: %+v", bad, a)
		}
	}
}

// GET /about отвечает без входа и без базы, только пятью полями сборки.
func TestAboutWithoutAuthAndDB(t *testing.T) {
	srv := &Server{About: NewAbout("v0.12.0", "ad10d8e3f1c2b4a5968778695a4b3c2d1e0f9a8b", DefaultSourceURL, "")}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/about", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("статус %d: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": "v0.12.0", "commit": "ad10d8e3f1c2b4a5968778695a4b3c2d1e0f9a8b",
		"source_url": DefaultSourceURL, "source_ref_url": DefaultSourceURL + "/tree/v0.12.0", "license": "AGPL-3.0-only",
	}
	if len(body) != len(want) {
		t.Fatalf("лишние поля: %v", body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("%s = %v, ожидалось %v", k, body[k], v)
		}
	}
}

// Ответ не зависит от данных установки: пустая и заполненная установка
// той же сборки отвечают одинаково, cookie и токен ничего не меняют.
func TestAboutSameOnFilledInstallation(t *testing.T) {
	st, srv := testServer(t)
	get := func(session string) []byte {
		t.Helper()
		resp, body := call(t, "GET", srv.URL+"/api/v1/about", session, "", nil)
		mustStatus(t, resp, http.StatusOK, "about")
		return body
	}
	empty := get("")
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice", "Alice", "pw-testpass", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject(ctx, "Секретный проект", "acme/secret", nil, u.ID); err != nil {
		t.Fatal(err)
	}
	filled := get("")
	if !bytes.Equal(empty, filled) {
		t.Fatalf("ответ зависит от данных установки:\n%s\n%s", empty, filled)
	}
	if bytes.Contains(filled, []byte("alice")) || bytes.Contains(filled, []byte("acme/secret")) {
		t.Fatalf("ответ раскрывает установку: %s", filled)
	}
	var a About
	if err := json.Unmarshal(filled, &a); err != nil || a.Version != "dev" || a.SourceRefURL != DefaultSourceURL {
		t.Fatalf("сборка без версии: %s, %v", filled, err)
	}
}
