package orchestrator

import (
	"context"
	"fmt"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/redact"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Реакции конвейера на события хостинга (change add-scm-events, спека
// scm-integration «События хостинга в конвейере»): решение, принятое на
// стороне GitHub/GitLab, доводится до задачи теми же путями, что и решения
// внутри Rivet — возврат в fixing расходует попытку и упирается в лимит.

// hostingTextCap — предел внешнего текста в событии и контексте стадии:
// тело review приходит снаружи и уезжает в промпт агента.
const hostingTextCap = 4000

// ExternalReacted — событие изменило ход конвейера (ответ webhook'у).
type ExternalReacted bool

// OnExternalChecks — итог внешних проверок хостинга на ветке задачи.
// Конвейер трогается только когда задача ждёт review: в остальных статусах
// это информационное событие (своя стадия тестов всё равно прогонит
// проверки, а вторая реакция расходовала бы попытку дважды).
func (e *Engine) OnExternalChecks(ctx context.Context, task domain.Task, ok bool, name, url string) (ExternalReacted, error) {
	projectID, epicID, err := e.St.TaskRefs(ctx, task.ID)
	if err != nil {
		return false, err
	}
	name = redact.String(clipRunes(name, 200))
	text := "внешние проверки хостинга прошли"
	if !ok {
		text = "внешние проверки хостинга провалились"
	}
	if name != "" {
		text += " (" + name + ")"
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "task.checks_external",
		ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Text:    text,
		Payload: map[string]any{"ok": ok, "name": name, "url": url},
	}); err != nil {
		return false, err
	}
	if ok || task.Status != domain.TaskReview {
		return false, nil
	}
	detail := text
	if url != "" {
		detail += ": " + url
	}
	e.mu.Lock()
	e.stageContext[task.ID] = "Внешние проверки хостинга провалились:\n" + detail
	e.mu.Unlock()
	// Тот же путь, что и провал своих проверок: попытка расходуется,
	// при исчерпании лимита задача проваливается с эскалацией.
	if _, err := e.St.ConsumeAttempt(ctx, task.ID, domain.AttTestFailed, detail, false, 0, ""); err != nil {
		return false, err
	}
	return true, nil
}

// OnExternalReview — review человека на хостинге. Запрос изменений — тот
// же источник замечаний, что и вердикт агента-ревьюера (спека
// task-pipeline «Независимое review»); одобрение и комментарий конвейер
// не трогают.
func (e *Engine) OnExternalReview(ctx context.Context, task domain.Task, state, author, body, url string) (ExternalReacted, error) {
	projectID, epicID, err := e.St.TaskRefs(ctx, task.ID)
	if err != nil {
		return false, err
	}
	author = redact.String(clipRunes(author, 100))
	body = redact.String(clipRunes(body, hostingTextCap))
	text := map[string]string{
		"approved":          "review человека на хостинге: одобрено",
		"changes_requested": "review человека на хостинге: запрошены изменения",
	}[state]
	if text == "" {
		text = "комментарий ревьюера на хостинге"
	}
	if author != "" {
		text += " (" + author + ")"
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorUser, ActorID: author, Type: "task.review_external",
		ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Text:    text,
		Payload: map[string]any{"state": state, "author": author, "url": url, "body": body},
	}); err != nil {
		return false, err
	}
	if state != "changes_requested" || task.Status != domain.TaskReview {
		return false, nil
	}
	eff, err := e.St.EffectivePolicy(ctx, projectID)
	if err != nil {
		return false, err
	}
	detail := "Замечания ревьюера " + author
	if body != "" {
		detail += ":\n" + body
	}
	e.mu.Lock()
	e.stageContext[task.ID] = detail
	e.mu.Unlock()
	if _, err := e.St.ConsumeAttempt(ctx, task.ID, domain.AttReviewLimit,
		detail, false, eff.Presets.ReviewLimit, ""); err != nil {
		return false, err
	}
	return true, nil
}

// OnPRClosed — PR задачи закрыт без merge. Задачу это не завершает и не
// отменяет: PR закрывают и по ошибке, и ради пересоздания, а отмена
// уронила бы зависимые задачи каскадом. Решение — за человеком.
func (e *Engine) OnPRClosed(ctx context.Context, task domain.Task, actor, url string) error {
	projectID, epicID, err := e.St.TaskRefs(ctx, task.ID)
	if err != nil {
		return err
	}
	actor = redact.String(clipRunes(actor, 100))
	msg := fmt.Sprintf("PR задачи task-%d закрыт на хостинге без merge", task.Num)
	if actor != "" {
		msg += " (" + actor + ")"
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorUser, ActorID: actor, Type: "task.pr_closed",
		ProjectID: projectID, EpicID: epicID, TaskID: task.ID,
		Text:    msg,
		Payload: map[string]any{"pr": url, "actor": actor},
	}); err != nil {
		return err
	}
	// Хостинг повторяет доставку, а PR закрывают и переоткрывают: вторая
	// одинаковая карточка в очереди человеку ничего не добавляет.
	open, err := e.St.HasOpenAttention(ctx, task.ID, domain.AttPRClosed)
	if err != nil || open {
		return err
	}
	return e.St.Escalate(ctx, projectID, task.ID, domain.AttPRClosed, msg+" — решите, повторить задачу или отменить")
}

// clipRunes — обрезка внешнего текста по рунам.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
