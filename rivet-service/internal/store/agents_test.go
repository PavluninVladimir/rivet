package store

import (
	"context"
	"errors"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// modelPairs — пары «подключение/модель» для сравнения.
func modelPairs(models []domain.AgentModelRef) []string {
	out := []string{}
	for _, m := range models {
		out = append(out, m.ConnectionID+"/"+m.Model)
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Профиль агента Rivet (add-native-agent, спека agents «Каталог агентов» и
// «Привязка моделей к агенту»): единственный профиль с адаптером native,
// модели — видимые модели включённых подключений, модель по умолчанию —
// одна из них, лимиты сохраняются, runner'ы агента Rivet получают модели
// каталога вместо объявленных.
func TestRivetProfileCatalogModels(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)
	list, err := s.ListAgents(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("каталог новой установки: %+v %v", list, err)
	}
	if a := list[0]; a.ID != AgentRivet || a.Adapter != AdapterNative || !a.Preset || !a.Enabled || len(a.Models) != 0 || a.DefaultModel != nil || a.Command != "" {
		t.Fatalf("профиль агента Rivet без подключений: %+v", a)
	}

	// Скрытая, пропавшая модель и модели выключенного подключения в
	// модели агента не входят.
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO model_connections (id, name, kind, api, base_url, models, enabled, updated_by)
		VALUES ('anthropic', 'Anthropic', 'vendor', 'anthropic', 'https://api.anthropic.com',
		        '[{"id":"opus"},{"id":"sonnet"},{"id":"haiku","hidden":true}]', true, $1),
		       ('openai', 'OpenAI', 'vendor', 'openai', 'https://api.openai.com',
		        '[{"id":"gpt-5"},{"id":"gpt-old","missing":true}]', true, $1),
		       ('off', 'Off', 'local', 'openai', 'http://localhost:1/v1', '[{"id":"m"}]', false, $1)`, admin.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"anthropic/opus", "anthropic/sonnet", "openai/gpt-5"}
	a, err := s.GetAgent(ctx, AgentRivet)
	if err != nil || !sameStrings(modelPairs(a.Models), want) {
		t.Fatalf("модели агента Rivet — каталог: %v %v", modelPairs(a.Models), err)
	}
	if a.DefaultModel == nil || a.DefaultModel.ConnectionID != "anthropic" || a.DefaultModel.Model != "opus" {
		t.Fatalf("без своей модели по умолчанию — первая модель каталога: %+v", a.DefaultModel)
	}

	// Runner агента Rivet: модели каталога вместо объявленных.
	r := rivetRunner("r1", "coding")
	r.Models = []string{"declared-1"}
	if err := s.UpsertRunner(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRunner(ctx, "r1"); !got.Catalog || !sameStrings(got.Models, []string{"opus", "sonnet", "gpt-5"}) || got.Model != "opus" ||
		!sameStrings(got.DeclaredModels, []string{"declared-1"}) {
		t.Fatalf("runner агента Rivet: %+v", got)
	}

	// Модель по умолчанию — только модель каталога.
	in := AgentInput{ID: AgentRivet, Name: "Агент Rivet", Adapter: AdapterNative}
	for _, bad := range []domain.AgentModelRef{
		{ConnectionID: "anthropic", Model: "haiku"},
		{ConnectionID: "openai", Model: "gpt-old"},
		{ConnectionID: "off", Model: "m"},
		{ConnectionID: "ghost", Model: "opus"},
	} {
		in.DefaultModel = &bad
		var fe *FieldError
		if _, _, err := s.UpsertAgent(ctx, in, admin.ID, admin.Login); !errors.As(err, &fe) || fe.Field != "default_model" {
			t.Fatalf("модель по умолчанию %+v вне каталога: %v", bad, err)
		}
	}

	// Модель по умолчанию, лимиты и режим секретов сохраняются; свои
	// привязки профиля не хранятся.
	in.DefaultModel = &domain.AgentModelRef{ConnectionID: "openai", Model: "gpt-5"}
	in.Limits = &domain.AgentLimits{MaxTurns: 120, MaxTokens: 2_000_000, MaxMinutes: 45}
	in.Secrets = "always"
	in.Models = []domain.AgentModelRef{{ConnectionID: "anthropic", Model: "opus"}}
	saved, created, err := s.UpsertAgent(ctx, in, admin.ID, admin.Login)
	if err != nil || created || saved.DefaultModel == nil || saved.DefaultModel.Model != "gpt-5" || saved.Limits != *in.Limits ||
		saved.Secrets != "always" || !sameStrings(modelPairs(saved.Models), want) {
		t.Fatalf("сохранение профиля: %+v %v %v", saved, created, err)
	}
	got, _ := s.GetAgent(ctx, AgentRivet)
	if got.DefaultModel == nil || got.DefaultModel.ConnectionID != "openai" || got.Limits != *in.Limits || got.Secrets != "always" || got.UpdatedBy != admin.Login {
		t.Fatalf("профиль после сохранения: %+v", got)
	}
	var stored []byte
	if err := s.Pool.QueryRow(ctx, `SELECT models FROM agents WHERE id=$1`, AgentRivet).Scan(&stored); err != nil || string(stored) != "[]" {
		t.Fatalf("привязки профиля не хранятся: %s %v", stored, err)
	}
	evs, _ := s.Events(ctx, EventFilter{Installation: true, Type: "agent.updated"})
	if len(evs) != 1 || evs[0].ActorID != admin.Login {
		t.Fatalf("событие изменения профиля: %+v", evs)
	}
	if _, binding, ok, err := s.ResolveAgentModel(ctx, AgentRivet, "", nil); err != nil || !ok || binding.ConnectionID != "openai" || binding.Model != "gpt-5" {
		t.Fatalf("действующая модель по умолчанию: %+v %v %v", binding, ok, err)
	}

	// Модель по умолчанию пропала из каталога: действует первая доступная.
	if _, err := s.Pool.Exec(ctx, `UPDATE model_connections SET enabled=false WHERE id='openai'`); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAgent(ctx, AgentRivet)
	if !sameStrings(modelPairs(got.Models), want[:2]) || got.DefaultModel == nil || got.DefaultModel.ConnectionID != "anthropic" || got.DefaultModel.Model != "opus" {
		t.Fatalf("модель по умолчанию вне каталога сбрасывается: %v %+v", modelPairs(got.Models), got.DefaultModel)
	}

	// Пустые лимиты — значения цикла по умолчанию.
	in.DefaultModel, in.Limits = nil, nil
	if saved, _, err := s.UpsertAgent(ctx, in, admin.ID, admin.Login); err != nil || saved.Limits != (domain.AgentLimits{}) {
		t.Fatalf("лимиты по умолчанию: %+v %v", saved.Limits, err)
	}

	// Других адаптеров, команды, окружения и аргументов CLI нет;
	// отрицательные лимиты не принимаются.
	for _, c := range []struct {
		in    AgentInput
		field string
	}{
		{AgentInput{ID: "my-agent", Name: "Mine", Adapter: "wrap", Command: "my-agent"}, "adapter"},
		{AgentInput{ID: "claude-code", Name: "Claude Code", Adapter: "claude-code"}, "adapter"},
		{AgentInput{ID: AgentRivet, Name: "R", Adapter: AdapterNative, Command: "claude"}, "command"},
		{AgentInput{ID: AgentRivet, Name: "R", Adapter: AdapterNative, Env: []domain.EnvVar{{Name: "KEY", Value: "{{key}}"}}}, "command"},
		{AgentInput{ID: AgentRivet, Name: "R", Adapter: AdapterNative, Args: []string{"-m", "{{model}}"}}, "command"},
		{AgentInput{ID: AgentRivet, Name: "R", Adapter: AdapterNative, Limits: &domain.AgentLimits{MaxTurns: -1}}, "limits"},
	} {
		var fe *FieldError
		if _, _, err := s.UpsertAgent(ctx, c.in, admin.ID, admin.Login); !errors.As(err, &fe) || fe.Field != c.field {
			t.Fatalf("%+v: ожидали ошибку поля %s, got %v", c.in, c.field, err)
		}
	}
	if list, _ := s.ListAgents(ctx); len(list) != 1 || list[0].ID != AgentRivet {
		t.Fatalf("каталог остался из одного профиля: %+v", list)
	}
	var fe *FieldError
	if err := s.DeleteAgent(ctx, AgentRivet, admin.Login); !errors.As(err, &fe) || fe.Field != "id" {
		t.Fatalf("встроенный профиль не удаляется: %v", err)
	}
}
