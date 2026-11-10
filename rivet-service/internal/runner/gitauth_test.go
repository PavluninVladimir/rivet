package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Отказ хостинга по авторизации в выводе git узнаётся на всех хостингах
// (fix-credential-state); прочие сбои git под него не попадают.
func TestGitAuthFailed(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"remote: Invalid username or password.\nfatal: Authentication failed for 'https://github.com/o/r.git/'", true},
		{"remote: HTTP Basic: Access denied. The provided password or token is incorrect\nfatal: Authentication failed for 'https://gitlab.com/o/r.git/'", true},
		{"fatal: could not read Username for 'https://github.com': terminal prompts disabled", true},
		{"remote: Permission to o/r.git denied to rivet-bot.\nfatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403", true},
		{"remote: You are not allowed to push code to this project.", true},
		{"fatal: unable to access 'https://git.example.com/o/r.git/': The requested URL returned error: 401", true},
		{"remote: User permission denied for writing.", true},
		// Доступ к приватному репозиторию отобран: GitHub и GitLab отвечают «не найден».
		{"remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found", true},
		{"remote: The project you were looking for could not be found or you don't have permission to view it.", true},
		{"fatal: repository 'https://git.example.com/o/r.git/' not found", true},
		{"fatal: unable to access 'https://github.com/o/r.git/': Could not resolve host: github.com", false},
		{"! [rejected] task-1 -> task-1 (non-fast-forward)", false},
		{"fatal: unable to access 'https://h/o/r.git/': The requested URL returned error: 502", false},
	} {
		if got := gitAuthFailed(tc.out); got != tc.want {
			t.Errorf("gitAuthFailed(%q) = %v, ожидалось %v", tc.out, got, tc.want)
		}
	}
}

// Хостинг отклоняет clone по авторизации: стадия CODING неуспешна с
// вердиктом scm_auth, control plane сверит его с учётными данными проекта.
func TestStageCloneRefusedReportsSCMAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	tmp := t.TempDir()
	cfg := Config{Workdir: filepath.Join(tmp, "work"), Adapter: AdapterWrap, AgentCmd: "true"}
	if err := os.MkdirAll(cfg.Workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &agent{cfg: cfg, adapter: newAdapter(cfg), cancel: map[string]*stageCancel{}}
	var mu sync.Mutex
	var res *pb.StageResult
	a.executeStage(context.Background(), &pb.Assignment{TaskId: "t8", TaskNum: 8, Stage: pb.StageResult_CODING,
		Title: "x", Description: "d", Repo: "o/r", RepoUrl: srv.URL + "/o/r", GitToken: "fake-revoked-token",
		Branch: "agent/task-8", BaseBranch: "main", SessionId: "s8"},
		func(m *pb.RunnerMsg) {
			mu.Lock()
			defer mu.Unlock()
			if sr := m.GetStageResult(); sr != nil {
				res = sr
			}
		})
	if res == nil || res.Ok || res.Verdict != verdictSCMAuth {
		t.Fatalf("ожидался неуспех с вердиктом scm_auth: %+v", res)
	}
	if strings.Contains(res.Detail, "fake-revoked-token") {
		t.Fatalf("токен не должен попадать в итог стадии: %s", res.Detail)
	}
}
