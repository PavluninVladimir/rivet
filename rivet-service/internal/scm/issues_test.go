package scm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubIssuesSelectionAndETag(t *testing.T) {
	var more atomic.Bool
	more.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("labels") != "rivet & bugs" || r.URL.Query().Get("state") != "open" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("If-None-Match") == `"old"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"new"`)
		if more.Load() {
			w.Header().Set("Link", `<https://hostile.invalid/secret>; rel="next"`)
		}
		fmt.Fprint(w, `[{"number":1,"title":"real","body":"task data","state":"open","labels":[{"name":"RIVET & BUGS"}]},
		{"number":2,"title":"pr","state":"open","labels":[{"name":"rivet & bugs"}],"pull_request":{}},
		{"number":3,"title":"no label","state":"open","labels":[]},
		{"number":4,"title":"closed","state":"closed","labels":[{"name":"rivet & bugs"}]}]`)
	}))
	defer srv.Close()
	g := NewGitHub("test")
	g.Base = srv.URL
	p, err := g.ListIssues(context.Background(), "o/r", "rivet & bugs", 1, "")
	if err != nil || len(p.Issues) != 1 || p.Issues[0].Number != 1 || p.NextPage != 2 || p.ETag != "" {
		t.Fatalf("selection/page: %+v %v", p, err)
	}
	more.Store(false)
	p, err = g.ListIssues(context.Background(), "o/r", "rivet & bugs", 1, "")
	if err != nil || p.ETag != `"new"` || p.NextPage != 0 {
		t.Fatalf("single-page validator: %+v %v", p, err)
	}
	p, err = g.ListIssues(context.Background(), "o/r", "rivet & bugs", 1, `"old"`)
	if err != nil || !p.NotModified || len(p.Issues) != 0 {
		t.Fatalf("304: %+v %v", p, err)
	}
}

func TestGitHubIssuesSafeErrors(t *testing.T) {
	for _, code := range []int{401, 403, 404, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if code == 429 {
					w.Header().Set("Retry-After", "180")
				}
				w.WriteHeader(code)
				fmt.Fprint(w, "SECRET and issue body")
			}))
			defer srv.Close()
			g := NewGitHub("SECRET")
			g.Base = srv.URL
			_, err := g.ListIssues(context.Background(), "o/r", "rivet", 1, "")
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe error %v", err)
			}
			if code == 429 {
				var rate *RateLimitError
				if !errors.As(err, &rate) || rate.After != 180*time.Second {
					t.Fatalf("backoff %v", err)
				}
			}
		})
	}
}
