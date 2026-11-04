package orchestrator

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// depsExclusions возвращает Epic'и с незавершёнными зависимостями
// (add-product-discovery): их задачи не назначаются. running — запущенные
// Epic'и. Ожидание объявляется событием epic.waiting_deps, когда запущенный
// Epic начинает ждать (в том числе сразу после запуска или возобновления),
// и снимается событием epic.deps_ready, когда запущенный Epic, о
// котором ожидание было объявлено, больше не ждёт. Пауза не снимает
// объявленного ожидания: Epic, чьи зависимости завершились на паузе,
// получает deps_ready при возобновлении. Первое состояние после старта
// снимается без событий.
func (e *Engine) depsExclusions(ctx context.Context, running []string) ([]string, error) {
	waiting, err := e.St.WaitingDeps(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(waiting))
	for id := range waiting {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	running = append([]string(nil), running...)
	sort.Strings(running)
	type change struct {
		id   string
		typ  string
		refs []domain.EpicRef
	}
	var changes []change
	e.mu.Lock()
	if e.depsWaiting == nil {
		e.depsWaiting = map[string]bool{}
	}
	init := e.depsInit
	e.depsInit = true
	for _, id := range running {
		w := len(waiting[id]) > 0
		switch {
		case w && !e.depsWaiting[id]:
			e.depsWaiting[id] = true
			if init {
				changes = append(changes, change{id: id, typ: "epic.waiting_deps", refs: waiting[id]})
			}
		case !w && e.depsWaiting[id]:
			delete(e.depsWaiting, id)
			if init {
				changes = append(changes, change{id: id, typ: "epic.deps_ready"})
			}
		}
	}
	e.mu.Unlock()
	for _, c := range changes {
		e.depsEvent(ctx, c.id, c.typ, c.refs)
	}
	return ids, nil
}

func (e *Engine) depsEvent(ctx context.Context, epicID, typ string, refs []domain.EpicRef) {
	ep, err := e.St.GetEpic(ctx, epicID)
	if err != nil {
		slog.Error("событие зависимостей Epic'а", "epic", epicID, "err", err)
		return
	}
	waitingFor := make([]string, 0, len(refs))
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		waitingFor = append(waitingFor, r.ID)
		name := "«" + r.Title + "»"
		if r.Status == string(domain.EpicArchived) {
			name += " (в архиве, снимите зависимость)"
		}
		names = append(names, name)
	}
	var text string
	switch typ {
	case "epic.waiting_deps":
		text = "ждёт Epic " + strings.Join(names, ", ")
	default:
		// Ожидание снято: зависимости выполнены или их сняли правкой.
		text = "зависимости выполнены, задачи Epic'а планируются"
		list := []domain.Epic{ep}
		if err := e.St.EnrichEpics(ctx, list); err == nil && len(list[0].DependsOn) == 0 {
			text = "зависимости сняты, задачи Epic'а планируются"
		}
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorScheduler, Type: typ, ProjectID: ep.ProjectID, EpicID: ep.ID,
		Text: text, Payload: map[string]any{"epic_id": ep.ID, "waiting_for": waitingFor},
	}); err != nil {
		slog.Error("событие зависимостей Epic'а", "epic", epicID, "err", err)
	}
}
