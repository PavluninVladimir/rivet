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
	// Память о поломках держится только по живым git-проектам: удалённый
	// проект или проект, вернувшийся к хранилищу, из неё уходит.
	alive := make(map[string]bool, len(projects))
	for _, p := range projects {
		alive[p.ID] = true
	}
	e.mu.Lock()
	for id := range e.policyBroken {
		if !alive[id] {
			delete(e.policyBroken, id)
		}
	}
	e.mu.Unlock()
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
	// Содержимое не менялось — новой версии не нужно, но эскалация
	// «политика сломана» могла остаться от прошлого прохода, если её не
	// удалось снять: снимаем ещё раз.
	if file.FileID != "" && file.FileID == p.PolicyFileID {
		return e.policySourceHealthy(ctx, p)
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
		if _, err := e.St.SetProjectPolicyFileID(ctx, p.ID, file.FileID); err != nil {
			return err
		}
		return e.policySourceHealthy(ctx, p)
	}
	author := "git"
	if file.FileID != "" {
		author = "git:" + shortID(file.FileID)
	}
	// Версия и идентификатор файла — одной транзакцией и только пока
	// источник — git: сбой между ними плодил бы дубликаты версий, а
	// переключение источника во время чтения — версию не из того источника.
	if _, err := e.St.SaveProjectPolicyFromGit(ctx, p.ID, overrides, file.FileID, author); err != nil {
		return err
	}
	return e.policySourceHealthy(ctx, p)
}

// policySourceHealthy — файл политики валиден и применён: эскалация
// «политика сломана» снимается, память о поломке сбрасывается, чтобы
// следующая поломка снова дала событие. Сброс только здесь: проход,
// закончившийся записью поломки, успехом не считается, даже если сама
// запись прошла без ошибки.
func (e *Engine) policySourceHealthy(ctx context.Context, p domain.Project) error {
	if err := e.St.ResolveProjectEscalation(ctx, p.ID, domain.AttPolicySource); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.policyBroken, p.ID)
	e.mu.Unlock()
	return nil
}

// policySourceBroken фиксирует, что файл политики не применить: событие и
// одна открытая эскалация на проект. Версия политики остаётся прежней.
// Событие пишется один раз на причину: файл, который сломан неделю,
// не должен давать запись в ленту каждую минуту.
func (e *Engine) policySourceBroken(ctx context.Context, p domain.Project, ref, detail string) error {
	e.mu.Lock()
	seen := e.policyBroken[p.ID] == detail
	e.mu.Unlock()
	msg := "политика проекта в репозитории не применяется: " + detail
	if seen {
		return e.St.EscalateProjectOnce(ctx, p.ID, domain.AttPolicySource, msg)
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "policy.sync_failed", ProjectID: p.ID,
		Text:    "политика из репозитория не применена: " + detail + " — действует последняя валидная версия",
		Payload: map[string]any{"file": policy.PolicyFile, "ref": ref, "error": detail},
	}); err != nil {
		return err
	}
	// Причина запоминается только после записи события: иначе сбой
	// записи потерял бы событие навсегда — следующий проход счёл бы его
	// уже написанным.
	e.mu.Lock()
	e.policyBroken[p.ID] = detail
	e.mu.Unlock()
	return e.St.EscalateProjectOnce(ctx, p.ID, domain.AttPolicySource, msg)
}
