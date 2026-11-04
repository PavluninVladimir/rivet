package orchestrator

import (
	"context"
	"fmt"
	"sync"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Конфликт PR задачи с базовой веткой при merge (change
// add-merge-conflict-recovery, спека task-pipeline «Конфликт с базовой
// веткой при merge»). Параллельные задачи Epic правят общие файлы, и PR,
// смерженный вторым, конфликтует с базовой веткой. Задача возвращается
// агенту на разрешение конфликта, попытки задачи на это не тратятся.

// taskLocks — блокировки операций по задачам внутри процесса control
// plane. Control plane работает одним экземпляром (контекст стадий тоже
// живёт в памяти), поэтому блокировки базы не нужны: они держали бы
// соединения пула, пока их владельцы ждут соединений для своей работы.
type taskLocks struct {
	mu sync.Mutex
	m  map[string]*taskLock
}

type taskLock struct {
	mu   sync.Mutex
	refs int
}

// lock захватывает блокировку задачи и возвращает её снятие; запись
// удаляется, когда блокировку никто не держит и не ждёт.
func (l *taskLocks) lock(id string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*taskLock{}
	}
	e := l.m[id]
	if e == nil {
		e = &taskLock{}
		l.m[id] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.m, id)
		}
		l.mu.Unlock()
	}
}

// withMergeLock выполняет операцию merge задачи под её блокировкой. id —
// идентификатор из базы (канонический UUID), а не из запроса.
func (e *Engine) withMergeLock(id string, fn func() error) error {
	unlock := e.mergeLocks.lock(id)
	defer unlock()
	return fn()
}

// MergeConflictError — ручной merge отклонён из-за конфликта. Returned:
// задача возвращена агенту на разрешение конфликта; иначе merge просто не
// выполнен (лимит исчерпан, в процессе нет шага code или задачу уже ведёт
// другой обработчик). Разворачивается в scm.ErrMergeConflict.
type MergeConflictError struct {
	Base     string
	Returned bool
}

func (e *MergeConflictError) Error() string {
	if e.Returned {
		return "PR конфликтует с " + e.Base + ": задача возвращена агенту на разрешение конфликта"
	}
	return "PR конфликтует с " + e.Base + ": merge не выполнен"
}

func (e *MergeConflictError) Unwrap() error { return scm.ErrMergeConflict }

// conflictInstruction — поручение агенту, уходит в контекст стадии
// исправления. Merge без коммита: коммит и push делает runner, как после
// любой реализации, и тот же коммит завершает merge.
func conflictInstruction(base string) string {
	return fmt.Sprintf("PR задачи конфликтует с базовой веткой %[1]s: её изменили задачи, смерженные раньше. "+
		"Влей свежую базовую ветку в ветку задачи: git fetch origin && git merge --no-commit --no-ff origin/%[1]s. "+
		"Разреши конфликты, сохранив смысл обеих сторон: и изменения этой задачи, и то, что уже есть в %[1]s. "+
		"Не откатывай изменения, пришедшие из %[1]s. Убедись, что задача по-прежнему выполнена и проверки проекта проходят.",
		base)
}

// mergeConflict — реакция на ErrMergeConflict при merge. В пределах лимита
// шага merge задача уходит на ближайший шаг code с поручением влить
// базовую ветку; сверх лимита (или без шага code в процессе) остаётся на
// merge с одной эскалацией MERGE_CONFLICT. userID непустой — merge
// подтверждал человек: его вмешательство сбрасывает счётчик возвратов
// (спека human-escalation). Вызывается под withMergeLock; task —
// снимок на входе в merge, и если задача с него уже ушла, ничего не
// меняется. Возвращает, ушла ли задача агенту.
func (e *Engine) mergeConflict(ctx context.Context, task domain.Task, p domain.Project, userID string) (bool, error) {
	manual := userID != ""
	proc, hash, err := e.processFor(ctx, task)
	if err != nil {
		return false, err
	}
	mergeID := mergeStepID(proc)
	limit := 1
	if step, ok := proc.Step(mergeID); ok && step.Attempts > 0 {
		limit = step.Attempts
	}
	target := nearestCodeBefore(proc, mergeID)
	noCode := target == policy.TargetEscalate
	if noCode {
		limit = 0
	}
	base := p.DefaultBranch
	if base == "" {
		base = "main"
	}
	escalation := fmt.Sprintf("PR ветки %s конфликтует с %s, лимит возвратов агенту (%d) исчерпан — "+
		"разрешите конфликт на хостинге и подтвердите merge", task.Branch, base, limit)
	if noCode {
		escalation = fmt.Sprintf("PR ветки %s конфликтует с %s, а в процессе проекта нет шага code для исправления — "+
			"разрешите конфликт на хостинге и подтвердите merge", task.Branch, base)
	}
	res, err := e.St.RecordMergeConflict(ctx, store.MergeConflictInput{
		TaskID: task.ID, ProjectID: p.ID, StepID: task.StepID, MergeStepID: mergeID, Gen: task.StepGen, Limit: limit,
		Reset: manual, Escalation: escalation,
	})
	if err != nil || res.Stale {
		return false, err
	}
	actor, actorID := domain.ActorSystem, ""
	if manual {
		actor, actorID = domain.ActorUser, userID
	}
	payload := map[string]any{
		"base": base, "returns": res.Returns, "limit": limit, "manual": manual, "exhausted": res.Exhausted,
	}

	if res.Exhausted {
		text := fmt.Sprintf("PR конфликтует с %s — лимит возвратов исчерпан, нужен человек", base)
		if noCode {
			text = fmt.Sprintf("PR конфликтует с %s — в процессе нет шага реализации, нужен человек", base)
		}
		_, err := e.St.AppendEvent(ctx, store.EventInput{
			ActorKind: actor, ActorID: actorID, Type: "task.merge_conflict",
			ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID, Text: text, Payload: payload,
		})
		return false, err
	}

	detail := conflictInstruction(base)
	payload["detail"] = detail
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: actor, ActorID: actorID, Type: "task.merge_conflict",
		ProjectID: p.ID, EpicID: task.EpicID, TaskID: task.ID,
		Text:    fmt.Sprintf("PR конфликтует с %s — возврат на разрешение конфликта (%d/%d)", base, res.Returns, limit),
		Payload: payload,
	}); err != nil {
		return false, err
	}
	epic, err := e.St.GetEpic(ctx, task.EpicID)
	if err != nil {
		return false, err
	}
	// Человек мог подтвердить merge, пока шёл review: запуски ревьюеров
	// отменяются, как при замечаниях с хостинга.
	e.cancelOpenRuns(ctx, task, 0)
	e.setStageContext(task.ID, detail)
	err = e.gotoTarget(ctx, task, proc, hash, target, policy.OutcomeChanges, "", epic.Status != domain.EpicRunning,
		map[string]any{"detail": detail}, "", &store.EventInput{
			Text:    "исправление: разрешение конфликта с " + base,
			Payload: map[string]any{"reason": "merge_conflict", "base": base},
		})
	if err != nil {
		e.setStageContext(task.ID, "")
		return false, err
	}
	return true, nil
}
