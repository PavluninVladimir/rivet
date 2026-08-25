package orchestrator

import (
	"context"
	"log/slog"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Точки принуждения конвейера (change add-policy-engine, спека
// access-policy «Точки принуждения»): решение принимает движок, метаправила
// и права остаются в коде и стоят до движка. Ошибка движка — запрет для
// автоматики (fail-closed): гейт не пропускает действие, факт попадает в
// event log и в очередь «needs attention».

// decide — решение точки принуждения.
func (e *Engine) decide(ctx context.Context, point string, input any) (policy.Decision, error) {
	return engineOr(e.Policy).Decide(ctx, point, input)
}

// engineOr — заданный движок или встроенный по умолчанию (внутренние
// потребители и тесты; rivetd всегда задаёт движок явно).
func engineOr(e policy.Engine) policy.Engine {
	if e != nil {
		return e
	}
	return policy.Default()
}

// policyEngineDown фиксирует, что решение получить не удалось: событие
// policy.decision с точкой принуждения и причиной плюс одна открытая
// эскалация POLICY_ENGINE на проект.
func (e *Engine) policyEngineDown(ctx context.Context, point, projectID string, eff store.EffectivePolicy, cause error) error {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	if err := policyEngineDown(ctx, e.St, point, projectID, eff, cause); err != nil {
		return err
	}
	e.policyDown[projectID] = true
	return nil
}

// policyEngineUp снимает эскалацию «движок недоступен», когда решение по
// проекту снова получено: иначе она висела бы вечно (закрывать её руками
// нечего — причина исчезла сама). Переходы down/up сериализованы своим
// mutex'ом: иначе параллельная публикация и тик планировщика могли бы
// закрыть эскалацию раньше, чем она записана.
func (e *Engine) policyEngineUp(ctx context.Context, projectID string) error {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	if !e.policyDown[projectID] {
		return nil
	}
	if err := e.St.ResolveProjectEscalation(ctx, projectID, domain.AttPolicyEngine); err != nil {
		return err
	}
	delete(e.policyDown, projectID)
	return nil
}

func policyEngineDown(ctx context.Context, st *store.Store, point, projectID string, eff store.EffectivePolicy, cause error) error {
	slog.Error("движок политик не дал решения", "point", point, "project", projectID, "err", cause)
	payload := eff.Ref()
	payload["point"], payload["allow"] = point, false
	payload["reason"], payload["detail"] = "engine_error", cause.Error()
	if _, err := st.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "policy.decision", ProjectID: projectID,
		Text:    "движок политик не дал решения (" + point + "): автоматика остановлена — " + cause.Error(),
		Payload: payload,
	}); err != nil {
		return err
	}
	return st.EscalateProjectOnce(ctx, projectID, domain.AttPolicyEngine,
		"движок политик недоступен: автоматика проекта остановлена ("+cause.Error()+")")
}

// orEmpty — пустой список вместо nil: движок получает массив, а не null.
func orEmpty(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// budgetInput — вход точки assign: бюджеты и расход установки, проекта и
// Epic. Ноль бюджета означает «без ограничения» (то же, что nil в пресетах).
type budgetInput struct {
	Installation budgetSide `json:"installation"`
	Project      budgetSide `json:"project"`
	Epic         budgetSide `json:"epic"`
}

type budgetSide struct {
	Used   int64 `json:"used"`
	Budget int64 `json:"budget"`
}

func budgetOf(used int64, limit *int64) budgetSide {
	if limit == nil {
		return budgetSide{Used: used}
	}
	return budgetSide{Used: used, Budget: *limit}
}
