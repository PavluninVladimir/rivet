package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Политика проекта из репозитория (change add-policy-git-provider, спека
// access-policy «Хранение политик — провайдеры и модель угроз»): файл
// доверенной ветки читается синхронизацией и превращается в обычную
// версию политики проекта. Точки принуждения продолжают читать
// действующую политику из базы — горячий путь на хостинг не ходит.

// policySyncInterval — как часто перечитывается файл политики: правки в
// защищённой ветке идут через ревью, минута задержки роли не играет.
const policySyncInterval = time.Minute

// syncGitPolicies подтягивает политику проектов с git-провайдером.
func (e *Engine) syncGitPolicies(ctx context.Context) error {
	e.mu.Lock()
	due := e.policySyncedAt.IsZero() || e.Now().Sub(e.policySyncedAt) >= policySyncInterval
	if due {
		e.policySyncedAt = e.Now()
	}
	e.mu.Unlock()
	if !due {
		return nil
	}
	projects, err := e.St.ProjectsWithGitPolicy(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if err := e.syncProjectPolicy(ctx, p); err != nil {
			slog.Error("политика из репозитория", "project", p.ID, "err", err)
		}
	}
	return nil
}

// syncProjectPolicy читает файл политики доверенной ветки и создаёт
// версию, если содержимое изменилось. Битый файл не останавливает
// конвейер: действует последняя валидная версия, а расхождение видно.
func (e *Engine) syncProjectPolicy(ctx context.Context, p domain.Project) error {
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return err
	}
	ref := p.DefaultBranch
	if ref == "" {
		ref = e.BaseBranch
	}
	file, err := adapter.ReadFile(ctx, p.Repo(), ref, policy.PolicyFile)
	if err != nil {
		if errors.Is(err, scm.ErrFileNotFound) {
			return e.policySourceBroken(ctx, p, ref, "файла политики нет в доверенной ветке")
		}
		// Хостинг недоступен — это не поломка политики: пропускаем проход,
		// действует последняя версия.
		return err
	}
	// Содержимое не менялось — новой версии не нужно.
	if file.FileID != "" && file.FileID == p.PolicyFileID {
		return nil
	}
	overrides, err := policy.ParseOverrides([]byte(file.Content))
	if err != nil {
		return e.policySourceBroken(ctx, p, ref, err.Error())
	}
	// Содержимое может совпасть с действующей версией, даже когда
	// идентификатор файла другой (перенос ветки, второй инстанс rivetd,
	// возврат к прежней политике): тогда версия не нужна, достаточно
	// запомнить идентификатор.
	eff, err := e.St.EffectivePolicy(ctx, p.ID)
	if err != nil {
		return err
	}
	if eff.Project != nil && eff.Project.Hash == policy.Hash(overrides) {
		if err := e.St.SetProjectPolicyFileID(ctx, p.ID, file.FileID); err != nil {
			return err
		}
		return e.St.ResolveProjectEscalation(ctx, p.ID, domain.AttPolicySource)
	}
	author := "git"
	if file.FileID != "" {
		author = "git:" + shortID(file.FileID)
	}
	if _, err := e.St.SaveProjectPolicy(ctx, p.ID, overrides, author); err != nil {
		return err
	}
	if err := e.St.SetProjectPolicyFileID(ctx, p.ID, file.FileID); err != nil {
		return err
	}
	// Файл снова читается — эскалация «политика сломана» больше не нужна.
	return e.St.ResolveProjectEscalation(ctx, p.ID, domain.AttPolicySource)
}

// policySourceBroken фиксирует, что файл политики не применить: событие и
// одна открытая эскалация на проект. Версия политики остаётся прежней.
func (e *Engine) policySourceBroken(ctx context.Context, p domain.Project, ref, detail string) error {
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "policy.sync_failed", ProjectID: p.ID,
		Text:    "политика из репозитория не применена: " + detail + " — действует последняя валидная версия",
		Payload: map[string]any{"file": policy.PolicyFile, "ref": ref, "error": detail},
	}); err != nil {
		return err
	}
	return e.St.EscalateProjectOnce(ctx, p.ID, domain.AttPolicySource,
		"политика проекта в репозитории не применяется: "+detail)
}
