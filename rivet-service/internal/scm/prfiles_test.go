package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// githubPRServer — GitHub с PR #5: changed записей в files и заявленное
// changedFiles в самом PR.
func githubPRServer(t *testing.T, changed, changedFiles int) *httptest.Server {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/5":
			_, _ = fmt.Fprintf(w, `{"changed_files":%d,"head":{"sha":"head-5"}}`, changedFiles)
		case "/repos/o/r/pulls/5/files":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			files := []map[string]string{}
			for i := (page - 1) * 100; i < page*100 && i < changed; i++ {
				f := map[string]string{"filename": fmt.Sprintf("docs/architecture/f%04d.md", i)}
				if i == changed-1 {
					f = map[string]string{"filename": "docs/architecture/main.go", "previous_filename": "src/main.go"}
				}
				files = append(files, f)
			}
			_ = json.NewEncoder(w).Encode(files)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	return gh
}

// Список файлов PR GitHub: постраничное чтение, оба пути переименования и
// вершина ветки PR (граница задачи описания архитектуры).
func TestGitHubPRFiles(t *testing.T) {
	a := NewGitHub("t")
	a.Base = githubPRServer(t, 102, 102).URL
	got, err := a.PRFiles(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 103 || got.HeadSHA != "head-5" || !strings.Contains(strings.Join(got.Files, ","), "src/main.go") {
		t.Fatalf("файлы PR: %d %q", len(got.Files), got.HeadSHA)
	}
}

// GitHub отдал меньше файлов, чем changed_files PR: список неполный (ревью
// Opus, круг 3).
func TestGitHubPRFilesCountMismatch(t *testing.T) {
	a := NewGitHub("t")
	a.Base = githubPRServer(t, 40, 41).URL
	if _, err := a.PRFiles(context.Background(), "o/r", 5); !errors.Is(err, ErrPRFilesTruncated) {
		t.Fatalf("расхождение с changed_files: %v", err)
	}
}

// Push между чтением PR и списка файлов: счётчик и список расходятся
// один раз, повтор даёт согласованный список (ревью Opus, финальная проверка).
func TestGitHubPRFilesRetryOnMismatch(t *testing.T) {
	old := prFilesRetryPause
	prFilesRetryPause = 0
	defer func() { prFilesRetryPause = old }()
	reads := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/5":
			reads++
			changed := 2
			if reads > 1 {
				changed = 1
			}
			_, _ = fmt.Fprintf(w, `{"changed_files":%d,"head":{"sha":"h%d"}}`, changed, reads)
		case "/repos/o/r/pulls/5/files":
			_, _ = w.Write([]byte(`[{"filename":"docs/architecture/a.md"}]`))
		}
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	got, err := a.PRFiles(context.Background(), "o/r", 5)
	if err != nil || got.HeadSHA != "h2" || reads != 2 {
		t.Fatalf("повтор при расхождении: %+v %v, чтений PR %d", got, err, reads)
	}
}

// Пустой changes_count у свежего MR — временное состояние: повтор, затем
// временный сбой, а не «список неполный».
func TestGitLabChangesCountPending(t *testing.T) {
	old := prFilesRetryPause
	prFilesRetryPause = 0
	defer func() { prFilesRetryPause = old }()
	for _, filled := range []bool{false, true} {
		reads := 0
		gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/merge_requests/8"):
				reads++
				if filled && reads > 1 {
					_, _ = w.Write([]byte(`{"iid":8,"sha":"h","changes_count":"1"}`))
					return
				}
				_, _ = w.Write([]byte(`{"iid":8,"sha":"h","changes_count":null}`))
			case strings.HasSuffix(r.URL.Path, "/merge_requests/8/diffs"):
				_, _ = w.Write([]byte(`[{"old_path":"docs/architecture/a.md","new_path":"docs/architecture/a.md"}]`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		g := NewGitLab(gl.URL, "t")
		got, err := g.PRFiles(context.Background(), "g/p", 8)
		gl.Close()
		switch {
		case filled && (err != nil || len(got.Files) != 1):
			t.Fatalf("счётчик посчитан при повторе: %+v %v", got, err)
		case !filled && (err == nil || errors.Is(err, ErrPRFilesTruncated)):
			t.Fatalf("непосчитанный счётчик: ожидался временный сбой, получено %v", err)
		}
	}
}

// GitLab: /diffs, а на старых инстансах /changes; неполный список — ошибка.
func TestGitLabPRFiles(t *testing.T) {
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/5"):
			_, _ = w.Write([]byte(`{"iid":5,"sha":"mr-head","changes_count":"1"}`))
		case strings.HasSuffix(r.URL.Path, "/merge_requests/5/diffs"):
			_, _ = w.Write([]byte(`[{"old_path":"src/a.go","new_path":"docs/architecture/a.go"}]`))
		case strings.HasSuffix(r.URL.Path, "/merge_requests/6"):
			_, _ = w.Write([]byte(`{"iid":6,"sha":"x","changes_count":"1"}`))
		case strings.HasSuffix(r.URL.Path, "/merge_requests/6/diffs"):
			w.WriteHeader(http.StatusNotFound)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/6/changes"):
			_, _ = w.Write([]byte(`{"changes":[{"old_path":"x","new_path":"x"}],"overflow":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	got, err := g.PRFiles(context.Background(), "g/p", 5)
	if err != nil || strings.Join(got.Files, ",") != "docs/architecture/a.go,src/a.go" || got.HeadSHA != "mr-head" {
		t.Fatalf("файлы MR: %+v %v", got, err)
	}
	if _, err := g.PRFiles(context.Background(), "g/p", 6); !errors.Is(err, ErrPRFilesTruncated) {
		t.Fatalf("неполный список MR: %v", err)
	}
}

// Репро ревью r3: MR больше diff limits GitLab (по умолчанию 1000 файлов
// или 50 000 строк). /diffs отдаёт сохранённый урезанный diff без признака
// overflow, а changes_count MR при этом "1000+". Файл кода за границей
// среза в список не попадает, поэтому такой список — отказ.
func TestGitLabDiffsTruncatedSilently(t *testing.T) {
	const stored = 1000 // столько файлов GitLab сохранил в diff MR
	for _, count := range []string{"1000+", "1001"} {
		gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/merge_requests/7/diffs"):
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				files := []map[string]string{}
				for i := (page - 1) * 100; i < page*100 && i < stored; i++ {
					p := fmt.Sprintf("docs/architecture/adr/%04d.md", i)
					files = append(files, map[string]string{"old_path": p, "new_path": p})
				}
				_ = json.NewEncoder(w).Encode(files)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
				_, _ = fmt.Fprintf(w, `{"iid":7,"sha":"h","changes_count":%q}`, count)
			case strings.HasSuffix(r.URL.Path, "/merge_requests/7/changes"):
				_, _ = w.Write([]byte(`{"changes":[],"overflow":true}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		g := NewGitLab(gl.URL, "t")
		got, err := g.PRFiles(context.Background(), "g/p", 7)
		gl.Close()
		if !errors.Is(err, ErrPRFilesTruncated) {
			t.Fatalf("changes_count %s: урезанный список принят как полный: %d файлов, %v", count, len(got.Files), err)
		}
	}
}

// Merge проверенной вершины: 409 хостинга при сдвинутой ветке — ErrHeadMoved,
// sha уходит в тело запроса.
func TestMergeAtHeadMoved(t *testing.T) {
	var ghBody, glBody map[string]any
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&ghBody)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Head branch was modified. Review and try the merge again."}`))
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	if _, err := a.MergeAt(context.Background(), "o/r", 5, "head-5"); !errors.Is(err, ErrHeadMoved) || ghBody["sha"] != "head-5" {
		t.Fatalf("github: %v %+v", err, ghBody)
	}
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&glBody)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"SHA does not match HEAD of source branch"}`))
	}))
	defer gl.Close()
	g := NewGitLab(gl.URL, "t")
	if _, err := g.MergeAt(context.Background(), "g/p", 5, "mr-head"); !errors.Is(err, ErrHeadMoved) || glBody["sha"] != "mr-head" {
		t.Fatalf("gitlab: %v %+v", err, glBody)
	}
}

// Fake в git-режиме: настоящие файлы ветки PR относительно базы, вершина,
// merge только этой вершины.
func TestFakeGitPRFiles(t *testing.T) {
	repos, work := gitStand(t)
	f := NewFakeGit(repos, "own/proj")
	ctx := context.Background()
	branchT(t, work, "agent/task-1", "src/main.go", "package main\n")
	pr, err := f.CreatePR(ctx, "own/proj", "agent/task-1", "main", "task-1", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.PRFiles(ctx, "own/proj", pr.Number)
	if err != nil || strings.Join(got.Files, ",") != "src/main.go" || got.HeadSHA != remoteSHA(t, work, "agent/task-1") {
		t.Fatalf("файлы PR fake: %+v %v", got, err)
	}
	if _, err := f.MergeAt(ctx, "own/proj", pr.Number, "0000000000000000000000000000000000000000"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("merge чужой вершины: %v", err)
	}
	if _, err := f.MergeAt(ctx, "own/proj", pr.Number, got.HeadSHA); err != nil {
		t.Fatalf("merge проверенной вершины: %v", err)
	}
}

// Симлинк GitHub: contents API отдаёт содержимое цели с её путём, такой
// файл не читается (ревью Opus, круг 3).
func TestGitHubReadFileSymlink(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"type":"file","path":"config/prod.env","encoding":"base64","content":"U0VDUkVUPTE=","sha":"x"}`))
	}))
	defer gh.Close()
	a := NewGitHub("t")
	a.Base = gh.URL
	if _, err := a.ReadFile(context.Background(), "o/r", "main", "docs/architecture/adr/0009.md"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("симлинк на файл вне каталога: %v", err)
	}
}
