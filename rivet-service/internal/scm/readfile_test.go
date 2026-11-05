package scm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Чтение файла по SHA (снимок архитектуры, add-architecture-c4): оба
// адаптера передают SHA как версию и отдают содержимое и идентификатор
// версии файла, отсутствие файла даёт ErrFileNotFound.
func TestAdapterReadFileBySHA(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("version: v1\n"))
	cases := bothAdapters(
		map[string]stubResp{
			"GET /repos/own/proj/contents/docs/architecture/architecture.yaml?ref=abc123": {200,
				`{"type":"file","encoding":"base64","content":"` + enc + `","sha":"blob1","size":12}`},
		},
		map[string]stubResp{
			"GET /api/v4/projects/own%2Fproj/repository/files/docs%2Farchitecture%2Farchitecture.yaml?ref=abc123": {200,
				`{"encoding":"base64","content":"` + enc + `","last_commit_id":"blob1"}`},
		},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			a := c.build(srv.URL, "t")
			fc, err := a.ReadFile(context.Background(), "own/proj", "abc123", "docs/architecture/architecture.yaml")
			if err != nil || fc.Content != "version: v1\n" || fc.FileID != "blob1" {
				t.Fatalf("ReadFile: %+v %v", fc, err)
			}
			_, err = a.ReadFile(context.Background(), "own/proj", "abc123", "docs/architecture/model/data.yaml")
			if !errors.Is(err, ErrFileNotFound) {
				t.Fatalf("нет файла: %v", err)
			}
			// RepoFS поверх адаптера: те же файлы через fs.FS.
			fsys := NewRepoFS(context.Background(), a, "own/proj", "abc123", "docs/architecture", FSLimits{})
			if raw, err := fs.ReadFile(fsys, "architecture.yaml"); err != nil || string(raw) != "version: v1\n" {
				t.Fatalf("RepoFS: %q %v", raw, err)
			}
			if _, err := fs.ReadFile(fsys, "model/data.yaml"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("RepoFS нет файла: %v", err)
			}
		})
	}
}

// Файл больше 1 МБ GitHub отдаёт без содержимого (encoding none): адаптер
// дочитывает сырой блоб, а не возвращает пустой файл.
func TestGitHubReadFileLarge(t *testing.T) {
	body := strings.Repeat("a", 1<<20+10)
	var metaAccept, blobAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/own/proj/contents/big.yaml" && r.URL.Query().Get("ref") == "agent/task-1":
			// Метаданные файла больше 1 МБ доступны только в media type object.
			metaAccept = r.Header.Get("Accept")
			if !strings.Contains(metaAccept, "object") {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"too large"}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"none","content":"","sha":"blob2","size":%d}`, len(body))
		case r.URL.Path == "/repos/own/proj/git/blobs/blob2":
			// Блоб по sha, а не по ветке: версия закреплена за FileID.
			blobAccept = r.Header.Get("Accept")
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	g := &GitHub{Token: "t", Client: httpClient(), Base: srv.URL, Web: srv.URL}
	fc, err := g.ReadFile(context.Background(), "own/proj", "agent/task-1", "big.yaml")
	if err != nil || fc.Content != body || fc.FileID != "blob2" {
		t.Fatalf("большой файл: %d байт, %v", len(fc.Content), err)
	}
	if !strings.Contains(blobAccept, "raw") {
		t.Fatalf("блоб запрошен не сырым: %q", blobAccept)
	}

	// Размер не сошёлся: ответ обрезан, ошибка вместо неполного файла.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git/blobs/") {
			_, _ = w.Write([]byte("short"))
			return
		}
		_, _ = w.Write([]byte(`{"type":"file","encoding":"none","content":"","sha":"blob3","size":9000000}`))
	}))
	t.Cleanup(srv2.Close)
	g2 := &GitHub{Token: "t", Client: httpClient(), Base: srv2.URL, Web: srv2.URL}
	if _, err := g2.ReadFile(context.Background(), "own/proj", "abc", "huge.yaml"); err == nil {
		t.Fatal("обрезанный файл должен давать ошибку")
	}
}

// Нет версии (ветки или коммита) — ErrRefNotFound, а не ErrFileNotFound:
// снимок не должен принять отсутствие версии за отсутствие описания, а
// политика из репозитория сообщает «доверенной ветки нет».
// Fake ведёт себя так же (TestFakeGitHeadAndRead).
func TestAdapterReadFileRefMissing(t *testing.T) {
	cases := bothAdapters(
		map[string]stubResp{
			"GET /repos/own/proj/contents/a.yaml?ref=nope": {404, `{"message":"No commit found for the ref nope"}`},
			"GET /repos/own/proj/contents/b.yaml?ref=main": {404, `{"message":"Not Found"}`},
		},
		map[string]stubResp{
			"GET /api/v4/projects/own%2Fproj/repository/files/a.yaml?ref=nope": {404, `{"message":"404 Commit Not Found"}`},
			"GET /api/v4/projects/own%2Fproj/repository/files/b.yaml?ref=main": {404, `{"message":"404 File Not Found"}`},
		},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			a := c.build(srv.URL, "t")
			_, err := a.ReadFile(context.Background(), "own/proj", "nope", "a.yaml")
			if !errors.Is(err, ErrRefNotFound) || errors.Is(err, ErrFileNotFound) {
				t.Fatalf("нет версии: %v", err)
			}
			if _, err := a.ReadFile(context.Background(), "own/proj", "main", "b.yaml"); !errors.Is(err, ErrFileNotFound) {
				t.Fatalf("нет файла: %v", err)
			}
		})
	}
}

// Блоб большого файла: ответ не 200 — ошибка; файл больше лимита чтения
// отклоняется без запроса блоба.
func TestGitHubReadFileLargeFailures(t *testing.T) {
	var blobCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			blobCalls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		case strings.HasSuffix(r.URL.Path, "/contents/mid.yaml"):
			_, _ = w.Write([]byte(`{"type":"file","encoding":"none","content":"","sha":"b1","size":2000000}`))
		case strings.HasSuffix(r.URL.Path, "/contents/huge.yaml"):
			_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"none","content":"","sha":"b2","size":%d}`, MaxResponseBytes+1)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	g := &GitHub{Token: "t", Client: httpClient(), Base: srv.URL, Web: srv.URL}
	if _, err := g.ReadFile(context.Background(), "own/proj", "main", "mid.yaml"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("блоб не 200: %v", err)
	}
	if blobCalls != 1 {
		t.Fatalf("запросы блоба: %d", blobCalls)
	}
	if _, err := g.ReadFile(context.Background(), "own/proj", "main", "huge.yaml"); err == nil || !strings.Contains(err.Error(), "лимита") {
		t.Fatalf("файл больше лимита: %v", err)
	}
	if blobCalls != 1 {
		t.Fatalf("файл больше лимита не должен запрашиваться: %d", blobCalls)
	}
}

// GitLab: ответ, упёршийся в лимит чтения, и размер, не совпавший с
// прочитанным, — понятные ошибки вместо сбоя разбора или неполного файла.
func TestGitLabReadFileLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.EscapedPath(), "big.yaml"):
			_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + strings.Repeat("A", MaxResponseBytes) + `"}`))
		case strings.Contains(r.URL.EscapedPath(), "short.yaml"):
			_, _ = fmt.Fprintf(w, `{"encoding":"base64","content":"%s","size":10,"last_commit_id":"c"}`, base64.StdEncoding.EncodeToString([]byte("abc")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	g := &GitLab{Token: "t", Client: httpClient(), Base: srv.URL}
	if _, err := g.ReadFile(context.Background(), "own/proj", "main", "big.yaml"); err == nil || !strings.Contains(err.Error(), "лимита") {
		t.Fatalf("ответ больше лимита: %v", err)
	}
	if _, err := g.ReadFile(context.Background(), "own/proj", "main", "short.yaml"); err == nil || !strings.Contains(err.Error(), "из 10") {
		t.Fatalf("размер не совпал: %v", err)
	}
}

// Каталог по пути: понятная ошибка, а не ErrFileNotFound и не сбой разбора.
func TestGitHubReadFileDirectory(t *testing.T) {
	srv := newStub(map[string]stubResp{
		"GET /repos/own/proj/contents/docs?ref=abc": {200, `[{"type":"file","name":"a.md"}]`},
		"GET /repos/own/proj/contents/view?ref=abc": {200, `{"type":"dir","entries":[{"type":"file","name":"a.md"}]}`},
	}).server(t)
	g := &GitHub{Token: "t", Client: httpClient(), Base: srv.URL, Web: srv.URL}
	for _, p := range []string{"docs", "view"} {
		_, err := g.ReadFile(context.Background(), "own/proj", "abc", p)
		if err == nil || errors.Is(err, ErrFileNotFound) || !strings.Contains(err.Error(), "каталог") {
			t.Fatalf("каталог %s: %v", p, err)
		}
	}
}

// Ветки нет (в том числе пустой репозиторий) отличается от недоступного
// репозитория и сбоя хостинга.
func TestAdapterHeadSHABranchNotFound(t *testing.T) {
	cases := bothAdapters(
		map[string]stubResp{
			"GET /repos/own/proj/branches/main":  {200, `{"commit":{"sha":"abc123"}}`},
			"GET /repos/own/empty/branches/main": {404, `{"message":"Branch not found"}`},
			"GET /repos/own/gone/branches/main":  {404, `{"message":"Not Found"}`},
		},
		map[string]stubResp{
			"GET /api/v4/projects/own%2Fproj/repository/branches/main":  {200, `{"commit":{"id":"abc123"}}`},
			"GET /api/v4/projects/own%2Fempty/repository/branches/main": {404, `{"message":"404 Branch Not Found"}`},
			"GET /api/v4/projects/own%2Fgone/repository/branches/main":  {404, `{"message":"404 Project Not Found"}`},
		},
	)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newStub(c.routes).server(t)
			a := c.build(srv.URL, "t")
			if sha, err := a.HeadSHA(context.Background(), "own/proj", "main"); err != nil || sha != "abc123" {
				t.Fatalf("HeadSHA: %q %v", sha, err)
			}
			if _, err := a.HeadSHA(context.Background(), "own/empty", "main"); !errors.Is(err, ErrBranchNotFound) {
				t.Fatalf("нет ветки: %v", err)
			}
			_, err := a.HeadSHA(context.Background(), "own/gone", "main")
			if err == nil || errors.Is(err, ErrBranchNotFound) {
				t.Fatalf("нет репозитория не равно нет ветки: %v", err)
			}
		})
	}
}
