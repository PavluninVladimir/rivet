package orchestrator

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Состояние учётных данных хостинга (change fix-credential-state, спеки
// scm-integration «Учётные данные хостинга», human-escalation «Причины
// эскалации»): отказ по авторизации делает подключение нерабочим с
// причиной и эскалацией, задачи ждут без расхода попыток, восстановление
// подключения возвращает их в конвейер.

// authSCM — хостинг, который отказывает по авторизации на выбранных
// операциях и отвечает заданной проверкой подключения.
type authSCM struct {
	diffSCM
	amu       sync.Mutex
	createErr error
	mergeErr  error
	headErr   error
	probe     *scm.ProbeResult
	creates   int
	probes    int
}

func (a *authSCM) CreatePR(ctx context.Context, repo, branch, base, title, body string) (scm.PR, error) {
	a.amu.Lock()
	a.creates++
	err := a.createErr
	a.amu.Unlock()
	if err != nil {
		return scm.PR{}, err
	}
	return a.diffSCM.CreatePR(ctx, repo, branch, base, title, body)
}

func (a *authSCM) Merge(ctx context.Context, repo string, number int) (string, error) {
	a.amu.Lock()
	err := a.mergeErr
	a.amu.Unlock()
	if err != nil {
		return "", err
	}
	return a.diffSCM.Merge(ctx, repo, number)
}

func (a *authSCM) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	a.amu.Lock()
	err := a.headErr
	a.amu.Unlock()
	if err != nil {
		return "", err
	}
	return "sha-head", nil
}

func (a *authSCM) Probe(ctx context.Context, repo string) scm.ProbeResult {
	a.amu.Lock()
	defer a.amu.Unlock()
	a.probes++
	if a.probe != nil {
		return *a.probe
	}
	return a.diffSCM.Probe(ctx, repo)
}

func (a *authSCM) set(fn func(a *authSCM)) {
	a.amu.Lock()
	defer a.amu.Unlock()
	fn(a)
}

var (
	err401 = fmt.Errorf("github create PR: 401: Bad credentials: %w", scm.ErrUnauthorized)
	err403 = fmt.Errorf("github merge: 403: forbidden: %w", scm.ErrForbidden)
	// probeRevoked — проверка подключения отозванного токена.
	probeRevoked = scm.ProbeResult{Reason: scm.ReasonBadToken, Message: "токен не принят хостингом"}
)

type credFixture struct {
	policyFixture
	sc  *authSCM
	box *secretbox.Box
}

// newCredFixture — задача A проекта с собственными учётными данными:
// адаптер берётся из Factory (Force — запасной адаптер теста).
func newCredFixture(t *testing.T) credFixture {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	sc := &authSCM{diffSCM: diffSCM{diff: "diff --git a/src/main.go b/src/main.go\n"}}
	out := &capture{}
	e := New(st, sc, nil, out, 90*time.Second)
	e.Adapters.Force = true
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	e.Box = box
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceCredential(ctx, p.ID, store.NewRepoConnection{Provider: "github", BaseURL: "https://github.com",
		Token: "fake-test-token-not-real", TokenOwner: "bot"}, box); err != nil {
		t.Fatal(err)
	}
	epic, _ := st.CreateEpic(ctx, p.ID, "Epic", "")
	task, err := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "worker", Agent: store.AgentRivet, Capabilities: []string{"coding"}})
	_ = st.UpsertRunner(ctx, domain.Runner{ID: "reviewer", Agent: store.AgentRivet, Capabilities: []string{"coding", "review"}})
	if err := st.TransitionEpic(ctx, epic.ID, domain.EpicRunning,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: "test", Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	return credFixture{policyFixture: policyFixture{st: st, out: out, e: e, owner: owner, p: p, epic: epic, task: task}, sc: sc, box: box}
}

func (f credFixture) credential(t *testing.T) domain.ScmCredential {
	t.Helper()
	c, err := f.st.ProjectCredential(context.Background(), f.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// credEscalations — открытые эскалации SCM_CREDENTIALS проекта.
func (f credFixture) credEscalations(t *testing.T) []domain.Attention {
	t.Helper()
	atts, err := f.st.ListAttention(context.Background(), f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Attention
	for _, a := range atts {
		if a.Reason == domain.AttScmCredentials && a.Status != "resolved" {
			out = append(out, a)
		}
	}
	return out
}

func (f credFixture) projectEvents(t *testing.T, typ string) []domain.Event {
	t.Helper()
	evs, err := f.st.Events(context.Background(), store.EventFilter{ProjectID: f.p.ID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (f credFixture) sentAssigns() int {
	f.out.mu.Lock()
	defer f.out.mu.Unlock()
	n := 0
	for _, m := range f.out.sent {
		if m.GetAssign() != nil {
			n++
		}
	}
	return n
}

// Токен отозван между push и созданием PR: задача не падает и не теряет
// исход проверок, подключение нерабочее с причиной, эскалация одна,
// назначений нет. Замена токена возвращает задачу в конвейер, PR
// создаётся, попытка не потрачена.
func TestCredentialsRevokedOnCreatePR(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	before, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatalf("итог проверок не должен падать: %v", err)
	}
	c := f.credential(t)
	if c.State != store.CredentialInvalid || c.StateReason != scm.ReasonBadToken || c.StateMessage == "" {
		t.Fatalf("подключение должно стать нерабочим с причиной: %+v", c)
	}
	if esc := f.credEscalations(t); len(esc) != 1 || esc[0].TaskID != "" {
		t.Fatalf("ожидалась одна эскалация уровня проекта: %+v", esc)
	}
	if evs := f.projectEvents(t, "project.credentials"); len(evs) != 1 || evs[0].Payload["state"] != "invalid" || evs[0].Payload["reason"] != scm.ReasonBadToken {
		t.Fatalf("событие project.credentials: %+v", evs)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status == domain.TaskFailed || task.PRURL != "" || task.AttemptUsed != before.AttemptUsed {
		t.Fatalf("задача не должна падать и тратить попытку: %+v (было %d)", task, before.AttemptUsed)
	}
	if task.WaitReason == "" {
		t.Fatal("у задачи должна быть причина ожидания")
	}
	if w, _ := f.st.GetRunner(ctx, worker); w.TaskID != "" {
		t.Fatalf("runner должен освободиться: %+v", w)
	}
	// Пока подключение нерабочее, проекту ничего не назначается и PR не
	// пытаются создать на каждом тике.
	sent := f.sentAssigns()
	for range 3 {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if f.sentAssigns() != sent {
		t.Fatal("проекту с нерабочими учётными данными ничего не назначается")
	}
	f.sc.set(func(a *authSCM) {
		if a.creates != 1 {
			t.Fatalf("повторов создания PR при нерабочем подключении быть не должно: %d", a.creates)
		}
		a.createErr = nil
	})
	// Участник заменяет токен: подключение рабочее, эскалация снята.
	if err := f.st.ReplaceCredential(ctx, f.p.ID, store.NewRepoConnection{Provider: "github", BaseURL: "https://github.com",
		Token: "fake-test-token-renewed", TokenOwner: "bot"}, f.box); err != nil {
		t.Fatal(err)
	}
	if esc := f.credEscalations(t); len(esc) != 0 {
		t.Fatalf("замена токена снимает эскалацию: %+v", esc)
	}
	// Задача продолжается с закреплённого исхода проверок: PR создан, задача
	// уходит на review.
	for range 2 {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.PRURL == "" || task.Status != domain.TaskReview || task.AttemptUsed != before.AttemptUsed {
		t.Fatalf("после замены токена задача продолжает конвейер: %+v", task)
	}
	if as := f.out.lastAssign(t); as.Stage != pb.StageResult_REVIEW {
		t.Fatalf("ожидали назначение REVIEW, got %v", as.Stage)
	}
	if w := eventsOfType(t, f.st, f.task.ID, "task.wait"); len(w) == 0 || w[len(w)-1].Payload["credentials"] != true {
		t.Fatalf("ожидание учётных данных видно в timeline: %+v", w)
	}
}

// Runner сообщает, что хостинг отклонил push: проверка подключения
// подтверждает отзыв, запуск откладывается без вердикта, задача не падает.
// Повторная проверка после восстановления доступа возвращает проект в
// работу, и стадия назначается снова.
func TestRunnerAuthFailureWaitsForCredentials(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.probe = &probeRevoked })
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	before, _ := f.st.GetTask(ctx, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId,
		Stage: pb.StageResult_CODING, Ok: false, Verdict: verdictSCMAuth,
		Detail: "git: exit status 128: fatal: Authentication failed for 'https://github.com/owner/repo.git/'"}); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status == domain.TaskFailed || task.AttemptUsed != before.AttemptUsed {
		t.Fatalf("задача не должна падать и тратить попытку: %+v", task)
	}
	runs, err := f.st.StepRuns(ctx, f.task.ID, task.StepID, task.StepGen)
	if err != nil || len(runs) != 1 || runs[0].Verdict != "" || runs[0].RunnerID != "" {
		t.Fatalf("запуск должен ждать без вердикта: %v %+v", err, runs)
	}
	if c := f.credential(t); c.State != store.CredentialInvalid || c.StateReason != scm.ReasonBadToken {
		t.Fatalf("подключение должно стать нерабочим: %+v", c)
	}
	if evs := f.projectEvents(t, "project.credentials"); len(evs) != 1 || evs[0].Payload["source"] != credSourceRunner {
		t.Fatalf("событие должно назвать источник runner: %+v", evs)
	}
	if esc := f.credEscalations(t); len(esc) != 1 {
		t.Fatalf("эскалация SCM_CREDENTIALS: %+v", esc)
	}
	if atts, _ := f.st.ListAttention(ctx, f.owner.ID); len(atts) != 1 {
		t.Fatalf("эскалаций задачи быть не должно: %+v", atts)
	}
	// Доступ вернули на хостинге без замены токена: повторная проверка.
	f.sc.set(func(a *authSCM) { a.probe = nil })
	defer func(v time.Duration) { credentialRecheckEvery = v }(credentialRecheckEvery)
	credentialRecheckEvery = 0
	f.e.RecheckCredentials(ctx)
	if c := f.credential(t); c.State != store.CredentialOK || c.StateReason != "" {
		t.Fatalf("повторная проверка возвращает подключение в работу: %+v", c)
	}
	if esc := f.credEscalations(t); len(esc) != 0 {
		t.Fatalf("эскалация снимается: %+v", esc)
	}
	if evs := f.projectEvents(t, "project.credentials"); len(evs) != 2 || evs[1].Payload["state"] != "ok" {
		t.Fatalf("событие о восстановлении: %+v", evs)
	}
	// Пауза откладывания истекла — стадия назначается снова.
	if _, err := f.st.Pool.Exec(ctx, `UPDATE task_step_runs SET not_before=now() WHERE task_id=$1`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	sent := f.sentAssigns()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.sentAssigns() != sent+1 || f.out.lastAssign(t).Stage != pb.StageResult_CODING {
		t.Fatal("после восстановления подключения стадия назначается снова")
	}
}

// Проверка подключения не подтверждает отзыв: отказ git разбирается как
// раньше, состояние подключения не меняется.
func TestRunnerAuthFailureNotConfirmed(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId,
		Stage: pb.StageResult_CODING, Ok: false, Verdict: verdictSCMAuth, Detail: "git: The requested URL returned error: 403"}); err != nil {
		t.Fatal(err)
	}
	if c := f.credential(t); c.State != store.CredentialOK {
		t.Fatalf("без подтверждения состояние не меняется: %+v", c)
	}
	if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskFailed {
		t.Fatalf("неуспех стадии code разбирается как раньше, got %s", got)
	}
	if esc := f.credEscalations(t); len(esc) != 0 {
		t.Fatalf("эскалации учётных данных быть не должно: %+v", esc)
	}
}

// 403 на merge при рабочем подключении (защищённая ветка) не делает
// учётные данные нерабочими; 403 при отозванном доступе — делает.
func TestForbiddenMergeChecksConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		probe   *scm.ProbeResult
		invalid bool
	}{
		{"ветка защищена", nil, false},
		{"доступ отозван", &scm.ProbeResult{Reason: scm.ReasonNoAccess, Message: "доступ запрещён"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCredFixture(t)
			enableAutoMerge(t, f.st, f.p.ID, nil)
			f.sc.set(func(a *authSCM) { a.mergeErr, a.probe = err403, tc.probe })
			f.policyFixture.sc = &f.sc.diffSCM
			if err := driveToReviewPassed(t, f.policyFixture); err != nil {
				t.Fatal(err)
			}
			if got := taskStatus(t, f.st, f.task.ID); got != domain.TaskReview {
				t.Fatalf("задача ждёт человека на merge, got %s", got)
			}
			c := f.credential(t)
			if (c.State == store.CredentialInvalid) != tc.invalid {
				t.Fatalf("состояние подключения: %+v", c)
			}
			if tc.invalid && (c.StateReason != scm.ReasonNoAccess || len(f.credEscalations(t)) != 1) {
				t.Fatalf("причина и эскалация: %+v %+v", c, f.credEscalations(t))
			}
			if len(eventsOfType(t, f.st, f.task.ID, "task.merge_failed")) != 1 {
				t.Fatal("отказ merge виден событием")
			}
			if !tc.invalid {
				// Повторный 403 при свежей успешной проверке подключение не
				// проверяет снова.
				if err := f.e.MergeTask(context.Background(), f.task.ID, f.owner.ID); err == nil {
					t.Fatal("ручной merge получает отказ хостинга")
				}
				f.sc.set(func(a *authSCM) {
					if a.probes != 1 {
						t.Fatalf("свежая проверка не повторяется на 403: %d", a.probes)
					}
				})
			}
		})
	}
}

// Успешная операция хостинга возвращает нерабочие учётные данные в работу.
func TestOperationRestoresCredentials(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	if _, err := f.st.MarkCredential(ctx, f.p.ID, f.credential(t).ID, store.CredentialMark{State: store.CredentialInvalid,
		Reason: scm.ReasonBadToken, Message: "токен не принят", Source: credSourceOperation}); err != nil {
		t.Fatal(err)
	}
	a, err := f.e.SCMFor(ctx, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.HeadSHA(ctx, f.p.RepoPath, "main"); err != nil {
		t.Fatal(err)
	}
	if c := f.credential(t); c.State != store.CredentialOK {
		t.Fatalf("успешная операция возвращает подключение в работу: %+v", c)
	}
	if esc := f.credEscalations(t); len(esc) != 0 {
		t.Fatalf("эскалация снимается: %+v", esc)
	}
	// Отказ 401, подтверждённый проверкой, обёрнут в ErrSCMCredentials,
	// прочие ошибки — нет.
	f.sc.set(func(s *authSCM) { s.headErr, s.probe = err401, &probeRevoked })
	if _, err := a.HeadSHA(ctx, f.p.RepoPath, "main"); !errors.Is(err, ErrSCMCredentials) || !errors.Is(err, scm.ErrUnauthorized) {
		t.Fatalf("отказ по авторизации: %v", err)
	}
	f.sc.set(func(s *authSCM) { s.headErr = fmt.Errorf("github branch: 502: bad gateway") })
	if _, err := a.HeadSHA(ctx, f.p.RepoPath, "main"); err == nil || errors.Is(err, ErrSCMCredentials) {
		t.Fatalf("сбой хостинга не про учётные данные: %v", err)
	}
	// Проект без собственных учётных данных не отслеживается.
	p2, err := f.st.CreateProject(ctx, "other", "owner/other", nil, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := f.e.SCMFor(ctx, p2)
	if err != nil {
		t.Fatal(err)
	}
	if _, tracked := a2.(*trackedAdapter); tracked {
		t.Fatal("адаптер проекта на токене установки не оборачивается")
	}
}

// Ревьюер не получает пустой diff: отказ хостинга при чтении diff'а
// откладывает стадию review.
func TestReviewDeferredWhenDiffRefused(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	for _, stage := range []pb.StageResult_Stage{pb.StageResult_CODING, pb.StageResult_TESTING} {
		if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: stage, Ok: true}); err != nil {
			t.Fatal(err)
		}
	}
	f.sc.diffErr = fmt.Errorf("github diff: 401: Bad credentials: %w", scm.ErrUnauthorized)
	f.sc.set(func(a *authSCM) { a.probe = &probeRevoked })
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if as := f.out.lastAssign(t); as.Stage == pb.StageResult_REVIEW {
		t.Fatal("review без diff'а не назначается")
	}
	if c := f.credential(t); c.State != store.CredentialInvalid {
		t.Fatalf("подключение нерабочее: %+v", c)
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.Status != domain.TaskReview || task.WaitReason == "" {
		t.Fatalf("задача ждёт на review с причиной: %+v", task)
	}
}

// 401 на merge при рабочем подключении (GitLab так отвечает токену без
// права merge в ветку) — запрет операции, а не отзыв: проект работает.
func TestUnauthorizedMergeWithWorkingConnection(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	enableAutoMerge(t, f.st, f.p.ID, nil)
	f.sc.set(func(a *authSCM) {
		a.mergeErr = fmt.Errorf("gitlab merge: 401: This user does not have permission to accept this merge request: %w", scm.ErrUnauthorized)
	})
	f.policyFixture.sc = &f.sc.diffSCM
	if err := driveToReviewPassed(t, f.policyFixture); err != nil {
		t.Fatal(err)
	}
	if c := f.credential(t); c.State != store.CredentialOK || len(f.credEscalations(t)) != 0 {
		t.Fatalf("рабочее подключение не становится нерабочим: %+v", c)
	}
	if len(eventsOfType(t, f.st, f.task.ID, "task.merge_failed")) != 1 {
		t.Fatal("отказ merge виден событием")
	}
	if err := f.e.MergeTask(ctx, f.task.ID, f.owner.ID); err == nil || errors.Is(err, ErrSCMCredentials) {
		t.Fatalf("ручной merge получает отказ хостинга: %v", err)
	}
	// 401 проверяется всегда, память о свежей проверке его не гасит.
	f.sc.set(func(a *authSCM) {
		if a.probes != 2 {
			t.Fatalf("401 проверяется при каждом отказе: %d", a.probes)
		}
	})
}

// Операция, собранная на заменённом токене, отказом не портит новые
// учётные данные.
func TestStaleOperationAfterReplace(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	a, err := f.e.SCMFor(ctx, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.ReplaceCredential(ctx, f.p.ID, store.NewRepoConnection{Provider: "github", BaseURL: "https://github.com",
		Token: "fake-test-token-renewed", TokenOwner: "bot"}, f.box); err != nil {
		t.Fatal(err)
	}
	f.sc.set(func(s *authSCM) { s.headErr, s.probe = err401, &probeRevoked })
	if _, err := a.HeadSHA(ctx, f.p.RepoPath, "main"); err == nil {
		t.Fatal("операция старым токеном отклонена")
	}
	if c := f.credential(t); c.State != store.CredentialOK || len(f.credEscalations(t)) != 0 {
		t.Fatalf("новые учётные данные остались рабочими: %+v", c)
	}
	if evs := f.projectEvents(t, "project.credentials"); len(evs) != 0 {
		t.Fatalf("событий о старых учётных данных нет: %+v", evs)
	}
}

// Шаг test с двумя участниками и require: any — второй прерван исходом
// шага. Отказ хостинга при создании PR не оставляет задачу навсегда:
// после восстановления подключения она продолжается.
func TestAwaitCredentialsRequireAny(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	proc := policy.DefaultProcess()
	proc.Steps[1].Participants = []policy.Participant{
		{Agent: &policy.AgentRef{Kind: store.AgentRivet, Model: "sonnet"}},
		{Agent: &policy.AgentRef{Kind: store.AgentRivet, Model: "gpt-5"}},
	}
	proc.Steps[1].Mode, proc.Steps[1].Require = policy.ModeParallel, policy.RequireAny
	setProcess(t, f.policyFixture, proc)
	for _, r := range []domain.Runner{
		{ID: "t-sonnet", Agent: store.AgentRivet, Models: []string{"sonnet"}, Capabilities: []string{"coding"}},
		{ID: "t-gpt", Agent: store.AgentRivet, Models: []string{"gpt-5"}, Capabilities: []string{"coding"}},
	} {
		if err := f.st.UpsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	mark := f.out.count()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	tests := f.out.assignsSince(mark)
	var testers []*pb.Assignment
	for _, as := range tests {
		if as.Stage == pb.StageResult_TESTING {
			testers = append(testers, as)
		}
	}
	if len(testers) != 2 {
		t.Fatalf("две сессии проверок: %+v", tests)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	runs, _ := f.st.StepRuns(ctx, f.task.ID, task.StepID, task.StepGen)
	runnerOf := map[string]string{}
	for _, r := range runs {
		runnerOf[r.SessionID] = r.RunnerID
	}
	if err := f.e.OnStageResult(ctx, runnerOf[testers[0].SessionId], &pb.StageResult{TaskId: f.task.ID, SessionId: testers[0].SessionId, Stage: pb.StageResult_TESTING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.PRURL != "" || task.Status == domain.TaskFailed || task.WaitReason == "" {
		t.Fatalf("задача ждёт подключения: %+v", task)
	}
	f.sc.set(func(a *authSCM) { a.createErr = nil })
	if err := f.st.ReplaceCredential(ctx, f.p.ID, store.NewRepoConnection{Provider: "github", BaseURL: "https://github.com",
		Token: "fake-test-token-renewed", TokenOwner: "bot"}, f.box); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	task, _ = f.st.GetTask(ctx, f.task.ID)
	if task.PRURL == "" || task.Status != domain.TaskReview || task.StepID != "review" {
		t.Fatalf("после восстановления задача продолжает конвейер: %+v", task)
	}
}

// 404 на операцию — тоже возможный отзыв: на приватный репозиторий без
// доступа GitHub отвечает «не найдено». Решает проверка подключения.
func TestNotFoundConfirmedByProbe(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	a, err := f.e.SCMFor(ctx, f.p)
	if err != nil {
		t.Fatal(err)
	}
	notFound := fmt.Errorf("github branch: 404: Not Found: %w", scm.ErrNotFound)
	f.sc.set(func(s *authSCM) { s.headErr = notFound })
	if _, err := a.HeadSHA(ctx, f.p.RepoPath, "main"); err == nil || errors.Is(err, ErrSCMCredentials) {
		t.Fatalf("при рабочем подключении 404 — ошибка операции: %v", err)
	}
	if c := f.credential(t); c.State != store.CredentialOK {
		t.Fatalf("подключение рабочее: %+v", c)
	}
	f.e.forgetProbe(f.credential(t).ID)
	f.sc.set(func(s *authSCM) {
		s.probe = &scm.ProbeResult{Reason: scm.ReasonNotFound, Message: "репозиторий не найден или недоступен по этому токену"}
	})
	if _, err := a.HeadSHA(ctx, f.p.RepoPath, "main"); !errors.Is(err, ErrSCMCredentials) {
		t.Fatalf("репозиторий не виден — нерабочее подключение: %v", err)
	}
	if c := f.credential(t); c.State != store.CredentialInvalid || c.StateReason != scm.ReasonNotFound {
		t.Fatalf("причина «репозиторий не найден»: %+v", c)
	}
}

// Ответы «не защищена» и «каталога нет» хостинги дают и без доступа: они
// нерабочее подключение в работу не возвращают.
func TestAmbiguousAnswersKeepInvalid(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	if _, err := f.st.MarkCredential(ctx, f.p.ID, f.credential(t).ID, store.CredentialMark{State: store.CredentialInvalid,
		Reason: scm.ReasonNoAccess, Message: "доступ запрещён"}); err != nil {
		t.Fatal(err)
	}
	f.sc.set(func(s *authSCM) { s.branchUnprotected = true })
	a, err := f.e.SCMFor(ctx, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := a.BranchProtected(ctx, f.p.RepoPath, "main"); err != nil || ok {
		t.Fatalf("ветка не защищена: %v %v", ok, err)
	}
	if sha, err := a.(scm.TreeHasher).PathSHA(ctx, f.p.RepoPath, "sha", "docs/architecture"); err != nil || sha != "" {
		t.Fatalf("каталога нет: %q %v", sha, err)
	}
	if c := f.credential(t); c.State != store.CredentialInvalid {
		t.Fatalf("неоднозначный ответ не возвращает подключение: %+v", c)
	}
}

// Пока подключение нерабочее, новые стадии не стартуют и на том же
// runner'е, а сессию доработки запустить нельзя.
func TestNoNewStagesOnInvalidCredentials(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	coding := f.out.lastAssign(t)
	if _, err := f.st.MarkCredential(ctx, f.p.ID, f.credential(t).ID, store.CredentialMark{State: store.CredentialInvalid,
		Reason: scm.ReasonBadToken, Message: "токен не принят"}); err != nil {
		t.Fatal(err)
	}
	mark := f.out.count()
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: coding.SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if as := f.out.assignsSince(mark); len(as) != 0 {
		t.Fatalf("проверки не стартуют на том же runner'е: %+v", as)
	}
	if w, _ := f.st.GetRunner(ctx, worker); w.TaskID != "" {
		t.Fatalf("runner освобождён: %+v", w)
	}
	if _, err := f.e.StartUserSession(ctx, f.task.ID, "доработай", f.owner.Login, false); !errors.Is(err, ErrSCMCredentials) {
		t.Fatalf("сессия доработки на нерабочем подключении: %v", err)
	}
}

// Повторная проверка при недоступном хостинге сдвигает очередь проверок.
func TestRecheckUnreachableMovesQueue(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	credID := f.credential(t).ID
	if _, err := f.st.MarkCredential(ctx, f.p.ID, credID, store.CredentialMark{State: store.CredentialInvalid,
		Reason: scm.ReasonBadToken, Message: "токен не принят"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE scm_credentials SET checked_at = now() - interval '2 hours' WHERE id=$1`, credID); err != nil {
		t.Fatal(err)
	}
	f.sc.set(func(s *authSCM) {
		s.probe = &scm.ProbeResult{Reason: scm.ReasonUnreachable, Message: "лимит запросов"}
	})
	defer func(v time.Duration) { credentialRecheckEvery = v }(credentialRecheckEvery)
	credentialRecheckEvery = time.Hour
	f.e.RecheckCredentials(ctx)
	if c := f.credential(t); c.State != store.CredentialInvalid {
		t.Fatalf("недоступный хостинг состояние не меняет: %+v", c)
	}
	if due, err := f.st.CredentialsToRecheck(ctx, time.Hour, 10); err != nil || len(due) != 0 {
		t.Fatalf("попытка отмечена, повтор через интервал: %v %+v", err, due)
	}
}

// driveToPRRefused проводит задачу через code и test; создание PR
// отклоняется отозванным токеном, задача ждёт.
func driveToPRRefused(t *testing.T, f credFixture) {
	t.Helper()
	ctx := context.Background()
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	for _, stage := range []pb.StageResult_Stage{pb.StageResult_CODING, pb.StageResult_TESTING} {
		if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: stage, Ok: true}); err != nil {
			t.Fatal(err)
		}
	}
	if c := f.credential(t); c.State != store.CredentialInvalid {
		t.Fatalf("подключение нерабочее: %+v", c)
	}
	if kind := f.waitKind(t); kind != store.WaitKindCredentials {
		t.Fatalf("задача ждёт восстановления подключения: %q", kind)
	}
}

func (f credFixture) waitKind(t *testing.T) string {
	t.Helper()
	var kind string
	if err := f.st.Pool.QueryRow(context.Background(), `SELECT wait_kind FROM tasks WHERE id=$1`, f.task.ID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	return kind
}

func (f credFixture) restoreToken(t *testing.T) {
	t.Helper()
	if err := f.st.ReplaceCredential(context.Background(), f.p.ID, store.NewRepoConnection{Provider: "github", BaseURL: "https://github.com",
		Token: fmt.Sprintf("fake-test-token-renewed-%d", time.Now().UnixNano()), TokenOwner: "bot"}, f.box); err != nil {
		t.Fatal(err)
	}
}

func (f credFixture) expireResumeLease(t *testing.T) {
	t.Helper()
	if _, err := f.st.Pool.Exec(context.Background(),
		`UPDATE tasks SET wait_claimed_at = now() - interval '1 hour' WHERE id=$1 AND wait_claimed_at IS NOT NULL`, f.task.ID); err != nil {
		t.Fatal(err)
	}
}

// Свежая память о проверке после 403 не гасит 401: подключение
// проверяется, задача ждёт, а не застревает с обычной ошибкой.
func TestUnauthorizedNotSilencedByProbeMemory(t *testing.T) {
	f := newCredFixture(t)
	f.e.rememberProbe(f.credential(t).ID)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	driveToPRRefused(t, f)
	if task, _ := f.st.GetTask(context.Background(), f.task.ID); task.Status == domain.TaskFailed || task.PRURL != "" {
		t.Fatalf("задача ждёт: %+v", task)
	}
}

// Продолжение после восстановления подключения упало на обычной ошибке
// хостинга: задача не теряется, отметка ожидания остаётся, повтор — после
// паузы, и задача доходит до review.
func TestResumeRetriesAfterHostError(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	driveToPRRefused(t, f)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = fmt.Errorf("github create PR: 502: bad gateway"), nil })
	f.restoreToken(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if kind := f.waitKind(t); kind != store.WaitKindCredentials {
		t.Fatalf("после сбоя продолжения задача по-прежнему ждёт: %q", kind)
	}
	creates := 0
	f.sc.set(func(a *authSCM) { creates = a.creates })
	// Повтор не на каждом тике, а после паузы захвата.
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.sc.set(func(a *authSCM) {
		if a.creates != creates {
			t.Fatalf("повтор до истечения паузы: %d → %d", creates, a.creates)
		}
		a.createErr = nil
	})
	f.expireResumeLease(t)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.PRURL == "" || task.Status != domain.TaskReview || task.StepID != "review" || f.waitKind(t) != "" {
		t.Fatalf("после паузы задача продолжает конвейер: %+v", task)
	}
	if n := len(eventsOfType(t, f.st, f.task.ID, "task.step")); n != 2 {
		t.Fatalf("исход шага не применяется повторно: %d событий task.step", n)
	}
}

// Рестарт посреди продолжения: захват остался, но истёк — задачу
// подхватывает следующий проход.
func TestResumeAfterRestartMidway(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	driveToPRRefused(t, f)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = nil, nil })
	f.restoreToken(t)
	// Прошлый процесс захватил продолжение и упал.
	claim, ok, err := f.st.ClaimCredentialsWait(ctx, f.task.ID, credentialResumeLease)
	if err != nil || !ok || claim.Target != "review" || claim.Entry != policy.OutcomeOk {
		t.Fatalf("захват: %+v %v %v", claim, ok, err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.PRURL != "" {
		t.Fatalf("чужой свежий захват не перехватывается: %+v", task)
	}
	f.expireResumeLease(t)
	// Захват прошлого процесса устарел: продлить его нельзя.
	if _, ok, err := f.st.RenewCredentialsClaim(ctx, f.task.ID, claim); err != nil || ok {
		t.Fatalf("устаревший захват не продлевается: %v %v", ok, err)
	}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if task, _ := f.st.GetTask(ctx, f.task.ID); task.PRURL == "" || task.Status != domain.TaskReview {
		t.Fatalf("брошенный захват подхвачен: %+v", task)
	}
}

// Постоянный отказ хостинга при продолжении (PR уже есть): причина
// ожидания показывает настоящую ошибку, а после нескольких неудач задача
// уходит человеку, а не ждёт вечно.
func TestResumePermanentFailureEscalates(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	driveToPRRefused(t, f)
	f.sc.set(func(a *authSCM) {
		a.createErr, a.probe = fmt.Errorf("github create PR: 422: A pull request already exists for owner:agent/task-1"), nil
	})
	f.restoreToken(t)
	for i := 1; i <= credentialResumeAttempts; i++ {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		task, _ := f.st.GetTask(ctx, f.task.ID)
		if i < credentialResumeAttempts {
			if f.waitKind(t) != store.WaitKindCredentials || !strings.Contains(task.WaitReason, "already exists") || task.Status == domain.TaskFailed {
				t.Fatalf("попытка %d: задача ждёт с настоящей причиной: %+v", i, task)
			}
		}
		f.expireResumeLease(t)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status != domain.TaskFailed || f.waitKind(t) != "" {
		t.Fatalf("после %d неудач задача уходит человеку: %+v", credentialResumeAttempts, task)
	}
	atts, _ := f.st.ListAttention(ctx, f.owner.ID)
	var found bool
	for _, a := range atts {
		if a.TaskID == f.task.ID && a.Status != "resolved" && strings.Contains(a.Message, "already exists") {
			found = true
		}
	}
	if !found {
		t.Fatalf("эскалация задачи с текстом хостинга: %+v", atts)
	}
}

// Рестарт во время ожидания стёр отчёт проверок из памяти: при
// продолжении он собирается заново, ревьюер его получает.
func TestResumeRestoresReviewContext(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	worker := mustTaskRunner(t, f.st, f.task.ID)
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId, Stage: pb.StageResult_CODING, Ok: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, worker, &pb.StageResult{TaskId: f.task.ID, SessionId: f.out.lastAssign(t).SessionId,
		Stage: pb.StageResult_TESTING, Ok: true, Detail: "go test ./... — 42 passed"}); err != nil {
		t.Fatal(err)
	}
	f.e.setStageContext(f.task.ID, "") // рестарт rivetd: память пуста
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = nil, nil })
	f.restoreToken(t)
	for range 2 {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	as := f.out.lastAssign(t)
	if as.Stage != pb.StageResult_REVIEW || !strings.Contains(as.ExtraContext, "42 passed") || !strings.Contains(as.ExtraContext, "Результаты автопроверок") {
		t.Fatalf("ревьюер получает отчёт проверок: %v %q", as.Stage, as.ExtraContext)
	}
}

// Токен не расшифровывается (другой ключ после переноса установки):
// попытка проверки всё равно отмечена, проект не занимает очередь.
func TestRecheckUnreadableTokenMovesQueue(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	credID := f.credential(t).ID
	if _, err := f.st.MarkCredential(ctx, f.p.ID, credID, store.CredentialMark{State: store.CredentialInvalid,
		Reason: scm.ReasonBadToken, Message: "токен не принят"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE scm_credentials SET token_enc = '\x00ff'::bytea,
		checked_at = now() - interval '2 hours' WHERE id=$1`, credID); err != nil {
		t.Fatal(err)
	}
	defer func(v time.Duration) { credentialRecheckEvery = v }(credentialRecheckEvery)
	credentialRecheckEvery = time.Hour
	f.e.RecheckCredentials(ctx)
	if due, err := f.st.CredentialsToRecheck(ctx, time.Hour, 10); err != nil || len(due) != 0 {
		t.Fatalf("попытка отмечена и при нечитаемом токене: %v %+v", err, due)
	}
}
