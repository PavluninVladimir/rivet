package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Поиск PR не теряется в обёртке проекта с собственным токеном.
type credentialFinder struct {
	*authSCM
	findErr error
}

func (a *credentialFinder) OpenPR(context.Context, string, string, string) (scm.PR, bool, error) {
	return scm.PR{Number: 91, URL: "https://fake.local/owner/repo/pull/91"}, a.findErr == nil, a.findErr
}

func TestCredentialWrapperFindsExistingPR(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	driveToPRRefused(t, f)
	f.restoreToken(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = scm.ErrPRExists, nil })
	f.e.Adapters.Fallback = &credentialFinder{authSCM: f.sc}
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.PRURL != "https://fake.local/owner/repo/pull/91" || task.Status != domain.TaskReview || f.waitKind(t) != "" {
		t.Fatalf("продолжение на существующем PR: %+v", task)
	}
}

func TestCredentialWrapperFindRefusalWaits(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	a := &credentialFinder{authSCM: f.sc, findErr: err401}
	f.sc.set(func(a *authSCM) { a.probe = &probeRevoked })
	cred, err := f.st.ProjectTokenState(ctx, f.p.ID, f.box)
	if err != nil {
		t.Fatal(err)
	}
	tracked := f.e.track(a, f.p, cred).(scm.PRFinder)
	_, _, err = tracked.OpenPR(ctx, "owner/repo", "branch", "main")
	if !errors.Is(err, ErrSCMCredentials) || f.credential(t).State != store.CredentialInvalid {
		t.Fatalf("отказ поиска: %v", err)
	}
}

func TestCredentialResumeTransientDoesNotEscalateAfterFive(t *testing.T) {
	ctx := context.Background()
	f := newCredFixture(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = err401, &probeRevoked })
	driveToPRRefused(t, f)
	f.restoreToken(t)
	f.sc.set(func(a *authSCM) { a.createErr, a.probe = fmt.Errorf("github create PR: 502: bad gateway"), nil })
	for range credentialResumeAttempts {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		f.expireResumeLease(t)
	}
	task, _ := f.st.GetTask(ctx, f.task.ID)
	if task.Status == domain.TaskFailed || f.waitKind(t) != store.WaitKindCredentials {
		t.Fatalf("временный сбой продолжает ожидание: %+v", task)
	}
	evs := eventsOfType(t, f.st, f.task.ID, "task.wait")
	if len(evs) < 2 || evs[len(evs)-1].Payload["error"] == nil {
		t.Fatalf("причина сбоя должна попасть в поток: %+v", evs)
	}
}
