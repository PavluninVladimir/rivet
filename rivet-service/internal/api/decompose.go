package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// plannerUnavailable — модель декомпозиции не настроена (invalid = false)
// или подключение недоступно (invalid = true).
type plannerUnavailable struct {
	invalid      bool
	detail       string
	connectionID string
}

func (e *plannerUnavailable) Error() string {
	if e.invalid {
		return "модель декомпозиции недоступна: " + e.detail
	}
	return "модель для декомпозиции не настроена: задайте ключ в разделе «Управление приложением» или в окружении установки"
}

// errEpicHasTasks — декомпозиция выполняется только на пустой план.
var errEpicHasTasks = errors.New("у Epic уже есть задачи — декомпозиция выполняется на пустой план")

// errDecomposeRunning — декомпозиция этого Epic'а уже идёт (кнопка во
// время фоновой декомпозиции бэклога или двойное нажатие).
var errDecomposeRunning = errors.New("декомпозиция этого Epic'а уже идёт")

// claimDecompose занимает Epic на время декомпозиции в этом процессе.
// Вторая декомпозиция того же Epic'а получает отказ, не дожидаясь модели.
func (s *Server) claimDecompose(epicID string) (release func(), ok bool) {
	s.decompMu.Lock()
	defer s.decompMu.Unlock()
	if s.decompEpics == nil {
		s.decompEpics = map[string]bool{}
	}
	if s.decompEpics[epicID] {
		return nil, false
	}
	s.decompEpics[epicID] = true
	return func() {
		s.decompMu.Lock()
		delete(s.decompEpics, epicID)
		s.decompMu.Unlock()
	}, true
}

// planRejected — модель не ответила или план не прошёл проверку.
type planRejected struct{ err error }

func (e *planRejected) Error() string { return "план не принят: " + e.err.Error() }
func (e *planRejected) Unwrap() error { return e.err }

// decompose — POST /api/v1/epics/{id}/decompose: модель строит план,
// детерминированный код валидирует, план создаётся в статусе planned
// и ждёт просмотра и запуска человеком (спека backend/epic-decomposition).
func (s *Server) decompose(w http.ResponseWriter, r *http.Request) {
	// Слой доступа раньше конфигурации: чужой epic — 404 до любых деталей.
	epic, err := s.St.EpicForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	tasks, err := s.decomposeEpic(r.Context(), epic)
	var unavailable *plannerUnavailable
	var rejected *planRejected
	switch {
	// Модель не настроена или ключ отклонён — машиночитаемый 503, не сбой
	// (спека epic-decomposition, api-contract).
	case errors.As(err, &unavailable) && unavailable.invalid:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": apiError{
			Code: "planner_invalid", Message: unavailable.Error()},
			"connection_id": unavailable.connectionID})
	case errors.As(err, &unavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
			Code: "no_planner", Message: unavailable.Error()}})
	case errors.Is(err, errDecomposeRunning):
		writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "decompose_running", Message: err.Error()}})
	case errors.Is(err, errEpicHasTasks), errors.As(err, &rejected):
		unprocessable(w, err.Error())
	case err != nil:
		writeErr(w, err)
	default:
		writeJSON(w, http.StatusCreated, map[string]any{"tasks": tasks})
	}
}

// decomposeEpic строит план Epic'а агентом-Тимлидом и создаёт задачи.
// Общий код ручной декомпозиции и фоновой декомпозиции после утверждения
// бэклога исследования (add-product-discovery). Если Epic связан с
// исследованием, Тимлид получает его потребности с цитатами.
func (s *Server) decomposeEpic(ctx context.Context, epic domain.Epic) ([]domain.Task, error) {
	pl, st := s.plannerStatus()
	switch {
	case st.State == "invalid":
		return nil, &plannerUnavailable{invalid: true, detail: st.Detail, connectionID: st.ConnectionID}
	case pl == nil:
		return nil, &plannerUnavailable{}
	}
	if epic.Status != domain.EpicPlanned {
		return nil, domain.ErrBadTransition{Entity: "epic", From: string(epic.Status), To: "decompose"}
	}
	release, ok := s.claimDecompose(epic.ID)
	if !ok {
		return nil, errDecomposeRunning
	}
	defer release()
	if existing, err := s.St.ListEpicTasks(ctx, epic.ID); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return nil, errEpicHasTasks
	}
	project, err := s.St.GetProject(ctx, epic.ProjectID)
	if err != nil {
		return nil, err
	}
	needs, err := s.St.EpicNeeds(ctx, epic.ID)
	if err != nil {
		return nil, err
	}

	// План строит агент-Тимлид команды (add-agent-roles): инструкция
	// шаблона и дополнение агента дополняют задание. Без Тимлида в команде —
	// инструкция шаблона; отключённый шаблон — прежнее задание.
	instruction := ""
	rolePayload := map[string]any{"model": st.Model, "source": string(st.Source)}
	var leadAgent *domain.TeamAgent
	if lead, err := s.St.GetRole(ctx, domain.RoleLead); err == nil && lead.Enabled {
		instruction = lead.Instruction
		rolePayload["role"], rolePayload["title"] = lead.ID, lead.Title
		if agent, ok, err := s.St.FirstTeamAgent(ctx, domain.RoleLead); err != nil {
			return nil, err
		} else if ok {
			if extra := strings.TrimSpace(agent.InstructionExtra); extra != "" {
				instruction += "\n" + extra
			}
			rolePayload["agent"] = map[string]any{"id": agent.ID, "name": agent.Name}
			leadAgent = &agent
		}
	} else {
		rolePayload["role_delivered"] = false
	}
	// Старт декомпозиции виден всем зрителям: Тимлид «составляет план».
	startPayload := map[string]any{"epic_id": epic.ID}
	if a, ok := rolePayload["agent"]; ok {
		startPayload["agent"] = a
	}
	// Ход в строке утверждённого бэклога пишется до события: консоль,
	// перечитав версию по событию, видит уже новое состояние.
	s.markDecompose(ctx, epic.ID, "running", "")
	if _, err := s.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.decomposing",
		ProjectID: project.ID, EpicID: epic.ID,
		Text: decomposingText(rolePayload), Payload: startPayload,
	}); err != nil {
		return nil, err
	}
	// Занятость Тимлида видна командам всех проектов (team.agent_busy и
	// team.agent_free): событие освобождения пишется и при обрыве запроса.
	signalCtx := context.WithoutCancel(ctx)
	done := s.startDecomposition(leadAgent, epic, func(busy bool) {
		if _, err := s.St.AppendEvent(signalCtx, store.AgentWorkEvent(leadAgent.ID, leadAgent.Name, busy)); err != nil {
			slog.Error("событие занятости Тимлида", "agent", leadAgent.ID, "busy", busy, "err", err)
		}
	})
	defer done()
	failed := func(reason string) {
		payload := map[string]any{"epic_id": epic.ID, "reason": reason}
		if a, ok := rolePayload["agent"]; ok {
			payload["agent"] = a
		}
		// Событие провала пишется и при обрыве запроса клиентом: иначе
		// консоль осталась бы с «составляет план».
		bg := context.WithoutCancel(ctx)
		s.markDecompose(bg, epic.ID, "failed", reason)
		s.planFailed(bg, epic, reason, payload)
	}
	in := planner.Input{LeadInstruction: instruction, Needs: needs}
	plan, err := s.requestPlan(ctx, pl, epic, project, in)
	// Персональные данные в задачах (email, телефон, имена и описания
	// респондентов): описания задач уходят в ветки и pull request'ы. Один
	// повтор с дефектами, затем план отклоняется. Дословный пересказ
	// требования — только предупреждение в событии.
	review := planner.ReviewPlan(plan, needs)
	if err == nil && len(review.PII) > 0 {
		in.Defects = review.PII
		plan, err = s.requestPlan(ctx, pl, epic, project, in)
		review = planner.ReviewPlan(plan, needs)
		if err == nil && len(review.PII) > 0 {
			err = fmt.Errorf("задачи содержат персональные данные респондентов: %s", strings.Join(review.PII, "; "))
		}
	}
	if err != nil {
		failed(err.Error())
		return nil, &planRejected{err: err}
	}
	if len(review.Warnings) > 0 {
		rolePayload["warnings"] = review.Warnings
	}
	tasks, err := planner.Materialize(ctx, s.St, epic.ID, plan)
	if errors.Is(err, store.ErrPlanExists) {
		// Задачи появились, пока модель отвечала (добавлены вручную): строка
		// бэклога готова, а этот запуск закрывается событием с причиной.
		bg := context.WithoutCancel(ctx)
		s.markDecompose(bg, epic.ID, "done", "")
		const reason = "у Epic'а уже появились задачи, план не создан"
		payload := map[string]any{"epic_id": epic.ID, "reason": reason}
		if a, ok := rolePayload["agent"]; ok {
			payload["agent"] = a
		}
		s.planFailed(bg, epic, reason, payload)
		return nil, errEpicHasTasks
	}
	if err != nil {
		failed(err.Error())
		return nil, err
	}
	s.markDecompose(ctx, epic.ID, "done", "")
	text := decomposedText(rolePayload)
	if n := len(review.Warnings); n > 0 {
		text += fmt.Sprintf(". Предупреждение: в задачах %d дословных фрагментов цитат интервью, проверьте формулировки", n)
	}
	if _, err := s.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.decomposed",
		ProjectID: project.ID, EpicID: epic.ID,
		Text:    text,
		Payload: rolePayload,
	}); err != nil {
		return nil, err
	}
	return tasks, nil
}

// requestPlan — один запрос плана со своим сроком: у ручной кнопки
// контекст HTTP без дедлайна, а Epic занят, пока декомпозиция идёт.
func (s *Server) requestPlan(ctx context.Context, pl *planner.Planner, epic domain.Epic, project domain.Project, in planner.Input) ([]planner.PlannedTask, error) {
	pctx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	plan, err := pl.DecomposeWith(pctx, epic, project, in)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("модель не ответила за %d минут", int(planTimeout.Minutes()))
	}
	return plan, err
}

// planFailed — событие провала декомпозиции Epic'а с причиной.
func (s *Server) planFailed(ctx context.Context, epic domain.Epic, reason string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{"epic_id": epic.ID, "reason": reason}
	}
	if _, err := s.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.plan_failed",
		ProjectID: epic.ProjectID, EpicID: epic.ID,
		Text: "план не составлен: " + reason, Payload: payload,
	}); err != nil {
		slog.Error("событие провала декомпозиции", "epic", epic.ID, "err", err)
	}
}

// backlogDecomposeTimeout — предел декомпозиции одного Epic'а в фоне,
// planTimeout — предел запроса плана у модели (ручная кнопка и фон).
const (
	backlogDecomposeTimeout = 20 * time.Minute
	planTimeout             = 10 * time.Minute
)

// decomposeBacklog декомпозирует Epic'и, созданные утверждением бэклога, по
// одному в порядке keys (топологический, внутри уровня по приоритету). Сбой
// одного Epic'а не останавливает остальные: Epic остаётся planned без
// задач, причина видна событием на нём и в строке бэклога (спека
// epic-decomposition «Декомпозиция Epic'ов из бэклога»).
func (s *Server) decomposeBacklog(runID string, keys []string, epicIDs map[string]string) {
	ctx := context.Background()
	for _, key := range keys {
		s.decomposeBacklogEpic(ctx, runID, key, epicIDs[key])
	}
}

// decomposeBacklogEpic — декомпозиция одного Epic'а бэклога. Паника не
// останавливает остальные Epic'и и не оставляет строку в «декомпозиция…».
func (s *Server) decomposeBacklogEpic(ctx context.Context, runID, key, id string) {
	set := func(status, detail string) {
		if err := s.St.SetRunDecompose(ctx, runID, key, status, detail); err != nil {
			slog.Error("ход декомпозиции бэклога", "run", runID, "key", key, "err", err)
		}
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("паника декомпозиции бэклога", "run", runID, "epic", id, "panic", p, "stack", string(debug.Stack()))
			// План мог успеть создаться до паники: тогда строка готова, и
			// провал на Epic'е не пишется.
			if tasks, err := s.St.ListEpicTasks(ctx, id); err == nil && len(tasks) > 0 {
				set("done", "")
				return
			}
			reason := fmt.Sprintf("внутренняя ошибка декомпозиции: %v", p)
			set("failed", reason)
			if epic, err := s.St.GetEpic(ctx, id); err == nil {
				s.planFailed(ctx, epic, reason, nil)
			}
		}
	}()
	// «Декомпозиция…» строке ставит сам decomposeEpic после захвата Epic'а:
	// если Epic ведёт ручная декомпозиция, строку ведёт она.
	epic, err := s.St.GetEpic(ctx, id)
	if err != nil {
		set("failed", err.Error())
		return
	}
	dctx, cancel := context.WithTimeout(ctx, backlogDecomposeTimeout)
	_, err = s.decomposeEpic(dctx, epic)
	cancel()
	var unavailable *plannerUnavailable
	switch {
	case err == nil:
		// «План готов» записан в транзакции плана, до события.
	case errors.Is(err, errDecomposeRunning):
		// Epic декомпозирует кнопка владельца: строку ведёт она, это
		// пропуск, а не провал.
		slog.Info("декомпозиция Epic'а из бэклога пропущена: уже идёт", "run", runID, "epic", id)
	case errors.Is(err, errEpicHasTasks):
		// План уже построен другой декомпозицией: строка готова.
		set("done", "")
	default:
		// Текст причины может содержать фрагменты интервью: в лог rivetd
		// идут только идентификаторы и длина, текст — в строку и событие.
		slog.Warn("декомпозиция Epic'а из бэклога не удалась", "run", runID, "epic", id, "reason_len", len(err.Error()))
		set("failed", err.Error())
		// Отказ до старта (нет модели) виден и событием на Epic'е:
		// провалы самой модели decomposeEpic уже записал.
		if errors.As(err, &unavailable) {
			s.planFailed(ctx, epic, err.Error(), nil)
		}
	}
}

// markDecompose — ход декомпозиции в строке утверждённого бэклога, если
// Epic создан утверждением (ручная кнопка и фоновая декомпозиция).
func (s *Server) markDecompose(ctx context.Context, epicID, status, detail string) {
	if err := s.St.MarkEpicDecompose(ctx, epicID, status, detail); err != nil {
		slog.Error("ход декомпозиции бэклога", "epic", epicID, "err", err)
	}
}

// decomposedText — текст события декомпозиции с автором плана.
func decomposedText(p map[string]any) string {
	t, _ := p["title"].(string)
	if t == "" {
		return "план построен и ожидает проверки человеком"
	}
	if a, ok := p["agent"].(map[string]any); ok {
		if name, _ := a["name"].(string); name != "" {
			t += " " + name
		}
	}
	return "план составил " + t + ": ожидает проверки человеком"
}

// decomposingText — текст события старта декомпозиции.
func decomposingText(p map[string]any) string {
	t, _ := p["title"].(string)
	if t == "" {
		return "декомпозиция Epic начата"
	}
	if a, ok := p["agent"].(map[string]any); ok {
		if name, _ := a["name"].(string); name != "" {
			t += " " + name
		}
	}
	return t + " составляет план"
}

// decomposition — идущие декомпозиции агента-Тимлида по одному Epic:
// число параллельных запусков и порядок последнего старта.
type decomposition struct {
	EpicID    string
	ProjectID string
	n         int
	seq       int64
}

// startDecomposition отмечает агента-Тимлида занятым планом Epic до
// возврата done. Параллельные декомпозиции считаются по Epic: done снимает
// только свою. signal(true) вызывается, когда агент начал первую
// декомпозицию, signal(false), когда закончилась последняя. Сигналы идут
// по одному в порядке переходов, а чтение команды (decompositionOf) их не
// ждёт.
func (s *Server) startDecomposition(agent *domain.TeamAgent, epic domain.Epic, signal func(busy bool)) (done func()) {
	if agent == nil {
		return func() {}
	}
	s.decompSignalMu.Lock()
	defer s.decompSignalMu.Unlock()
	s.decompMu.Lock()
	if s.decomposing == nil {
		s.decomposing = map[string]map[string]*decomposition{}
	}
	byEpic := s.decomposing[agent.ID]
	first := byEpic == nil
	if first {
		byEpic = map[string]*decomposition{}
		s.decomposing[agent.ID] = byEpic
	}
	d := byEpic[epic.ID]
	if d == nil {
		d = &decomposition{EpicID: epic.ID, ProjectID: epic.ProjectID}
		byEpic[epic.ID] = d
	}
	s.decompSeq++
	d.n, d.seq = d.n+1, s.decompSeq
	s.decompMu.Unlock()
	if first {
		signal(true)
	}
	return func() {
		s.decompSignalMu.Lock()
		defer s.decompSignalMu.Unlock()
		s.decompMu.Lock()
		if d.n--; d.n <= 0 {
			delete(byEpic, epic.ID)
		}
		last := len(byEpic) == 0
		if last {
			delete(s.decomposing, agent.ID)
		}
		s.decompMu.Unlock()
		if last {
			signal(false)
		}
	}
}

// decompositionOf — последняя начатая из идущих декомпозиций агента.
func (s *Server) decompositionOf(agentID string) (decomposition, bool) {
	s.decompMu.Lock()
	defer s.decompMu.Unlock()
	var best *decomposition
	for _, d := range s.decomposing[agentID] {
		if best == nil || d.seq > best.seq {
			best = d
		}
	}
	if best == nil {
		return decomposition{}, false
	}
	return *best, true
}
