package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Состояние учётных данных хостинга (change fix-credential-state): переход
// в нерабочее пишет одно событие и одну эскалацию уровня проекта, повтор
// только обновляет причину, возврат и замена токена эскалацию снимают.
func TestMarkCredentialTransitions(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-cred", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProjectWithRepo(ctx, "cred", nil, owner.ID, NewRepoConnection{
		Provider: "github", BaseURL: "https://github.com", RepoPath: "own/proj",
		Token: "fake-test-token-not-real", TokenOwner: "bot"}, box)
	if err != nil {
		t.Fatal(err)
	}
	// Проект на токене установки: учётных данных нет, ничего не меняется.
	bare, err := s.CreateProject(ctx, "bare", "own/bare", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := s.MarkCredential(ctx, bare.ID, "00000000-0000-0000-0000-000000000000", CredentialMark{State: CredentialInvalid, Reason: "bad_token"}); err != nil || changed {
		t.Fatalf("проект без учётных данных: %v %v", changed, err)
	}

	escalations := func() []domain.Attention {
		t.Helper()
		atts, err := s.ListAttention(ctx, owner.ID)
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
	events := func() []domain.Event {
		t.Helper()
		evs, err := s.Events(ctx, EventFilter{ProjectID: p.ID, Type: "project.credentials", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}

	cur, err := s.ProjectCredential(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	credID := cur.ID
	if pc, err := s.ProjectTokenState(ctx, p.ID, box); err != nil || pc.CredID != credID || pc.Token != "fake-test-token-not-real" || pc.State != CredentialOK {
		t.Fatalf("токен и состояние: %v %+v", err, pc)
	}

	// Рабочее → рабочее: ни события, ни эскалации.
	if changed, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: CredentialOK}); err != nil || changed {
		t.Fatalf("ok → ok: %v %v", changed, err)
	}
	if changed, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: CredentialInvalid,
		Reason: "bad_token", Message: "токен не принят", Source: "operation"}); err != nil || !changed {
		t.Fatalf("ok → invalid: %v %v", changed, err)
	}
	// Повтор с другой причиной: состояние то же, причина свежая.
	if changed, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: CredentialInvalid,
		Reason: "no_access", Message: "доступ запрещён", Source: "recheck"}); err != nil || changed {
		t.Fatalf("invalid → invalid: %v %v", changed, err)
	}
	c, err := s.ProjectCredential(ctx, p.ID)
	if err != nil || c.State != CredentialInvalid || c.StateReason != "no_access" || c.StateMessage != "доступ запрещён" || c.CheckedAt == nil {
		t.Fatalf("учётные данные: %v %+v", err, c)
	}
	// Текст эскалации следует за причиной.
	if esc := escalations(); len(esc) != 1 || esc[0].TaskID != "" || esc[0].ProjectID != p.ID || !strings.Contains(esc[0].Message, "доступ запрещён") {
		t.Fatalf("одна эскалация уровня проекта с актуальной причиной: %+v", esc)
	}
	if evs := events(); len(evs) != 1 || evs[0].Payload["reason"] != "bad_token" || evs[0].ActorKind != domain.ActorSystem {
		t.Fatalf("одно событие перехода: %+v", evs)
	}
	ids, err := s.InvalidCredentialProjects(ctx)
	if err != nil || len(ids) != 1 || ids[0] != p.ID {
		t.Fatalf("нерабочие проекты: %v %v", ids, err)
	}
	// Повторная проверка берёт только давно не проверенные.
	if due, err := s.CredentialsToRecheck(ctx, time.Hour, 10); err != nil || len(due) != 0 {
		t.Fatalf("свежая проверка не повторяется: %v %+v", err, due)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE scm_credentials SET checked_at = now() - interval '2 hours' WHERE id=$1`, credID); err != nil {
		t.Fatal(err)
	}
	if due, err := s.CredentialsToRecheck(ctx, time.Hour, 10); err != nil || len(due) != 1 || due[0].ID != p.ID {
		t.Fatalf("к повторной проверке: %v %+v", err, due)
	}
	// Попытка проверки сдвигает очередь при любом исходе.
	if err := s.TouchCredentialRecheck(ctx, credID); err != nil {
		t.Fatal(err)
	}
	if due, err := s.CredentialsToRecheck(ctx, time.Hour, 10); err != nil || len(due) != 0 {
		t.Fatalf("после попытки проверка не повторяется сразу: %v %+v", err, due)
	}

	// Возврат в работу: событие, эскалация снята, причина пустая.
	if changed, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: CredentialOK, Source: "operation"}); err != nil || !changed {
		t.Fatalf("invalid → ok: %v %v", changed, err)
	}
	if c, _ := s.ProjectCredential(ctx, p.ID); c.State != CredentialOK || c.StateReason != "" || c.StateMessage != "" {
		t.Fatalf("рабочее состояние без причины: %+v", c)
	}
	if esc := escalations(); len(esc) != 0 {
		t.Fatalf("эскалация снята: %+v", esc)
	}
	if evs := events(); len(evs) != 2 || evs[1].Payload["state"] != CredentialOK {
		t.Fatalf("событие возврата: %+v", evs)
	}

	// Замена токена снимает эскалацию нерабочих учётных данных.
	if _, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: CredentialInvalid, Reason: "bad_token", Message: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceCredential(ctx, p.ID, NewRepoConnection{Provider: "github", BaseURL: "https://github.com",
		Token: "fake-test-token-renewed", TokenOwner: "bot"}, box); err != nil {
		t.Fatal(err)
	}
	if esc := escalations(); len(esc) != 0 {
		t.Fatalf("замена токена снимает эскалацию: %+v", esc)
	}
	if c, _ := s.ProjectCredential(ctx, p.ID); c.State != CredentialOK {
		t.Fatalf("новые учётные данные рабочие: %+v", c)
	}
	// Отказ операции, шедшей на заменённом токене, новые учётные данные не трогает.
	if changed, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: CredentialInvalid, Reason: "bad_token", Message: "старый"}); err != nil || changed {
		t.Fatalf("наблюдение по старым учётным данным: %v %v", changed, err)
	}
	if c, _ := s.ProjectCredential(ctx, p.ID); c.State != CredentialOK || len(escalations()) != 0 {
		t.Fatalf("новые учётные данные остались рабочими: %+v", c)
	}
	if _, err := s.MarkCredential(ctx, p.ID, credID, CredentialMark{State: "unchecked"}); err == nil {
		t.Fatal("недопустимое состояние отклоняется")
	}
}
