// Package domain — словарь Rivet: сущности и статусные модели.
// Единственный источник правил переходов — спека backend/domain-model.
package domain

import "fmt"

type TaskStatus string

const (
	TaskQueued    TaskStatus = "queued"
	TaskReady     TaskStatus = "ready"
	TaskRunning   TaskStatus = "running"
	TaskTesting   TaskStatus = "testing"
	TaskReview    TaskStatus = "review"
	TaskFixing    TaskStatus = "fixing"
	TaskBlocked   TaskStatus = "blocked"
	TaskFailed    TaskStatus = "failed"
	TaskDone      TaskStatus = "done"
	TaskCancelled TaskStatus = "cancelled"
)

// taskTransitions — матрица переходов из спеки backend/domain-model, дословно.
var taskTransitions = map[TaskStatus][]TaskStatus{
	TaskQueued:    {TaskReady, TaskBlocked, TaskCancelled},
	TaskReady:     {TaskRunning, TaskQueued, TaskCancelled},
	TaskRunning:   {TaskTesting, TaskBlocked, TaskFailed, TaskReady, TaskCancelled},
	TaskTesting:   {TaskReview, TaskFixing, TaskFailed, TaskReady},
	TaskReview:    {TaskDone, TaskFixing, TaskFailed, TaskReady},
	TaskFixing:    {TaskTesting, TaskBlocked, TaskFailed, TaskReady},
	TaskBlocked:   {TaskQueued, TaskReady, TaskCancelled},
	TaskFailed:    {TaskQueued, TaskReady, TaskCancelled},
	TaskDone:      {},
	TaskCancelled: {},
}

func (s TaskStatus) CanTransition(to TaskStatus) bool {
	for _, t := range taskTransitions[s] {
		if t == to {
			return true
		}
	}
	return false
}

func (s TaskStatus) Terminal() bool { return len(taskTransitions[s]) == 0 }

// ErrBadTransition возвращается при попытке недопустимого перехода —
// по спеке переход не выполняется, а в event log пишется ошибка.
type ErrBadTransition struct {
	Entity   string
	From, To string
}

func (e ErrBadTransition) Error() string {
	return fmt.Sprintf("недопустимый переход %s: %s → %s", e.Entity, e.From, e.To)
}

type EpicStatus string

const (
	EpicPlanned  EpicStatus = "planned"
	EpicRunning  EpicStatus = "running"
	EpicPaused   EpicStatus = "paused"
	EpicDone     EpicStatus = "done"
	EpicArchived EpicStatus = "archived"
)

var epicTransitions = map[EpicStatus][]EpicStatus{
	EpicPlanned:  {EpicRunning, EpicArchived},
	EpicRunning:  {EpicPaused, EpicDone},
	EpicPaused:   {EpicRunning, EpicArchived},
	EpicDone:     {EpicArchived},
	EpicArchived: {},
}

func (s EpicStatus) CanTransition(to EpicStatus) bool {
	for _, t := range epicTransitions[s] {
		if t == to {
			return true
		}
	}
	return false
}

type RunnerStatus string

const (
	RunnerIdle      RunnerStatus = "idle"
	RunnerRunning   RunnerStatus = "running"
	RunnerTesting   RunnerStatus = "testing"
	RunnerReview    RunnerStatus = "review"
	RunnerDeploying RunnerStatus = "deploying"
	RunnerOffline   RunnerStatus = "offline"
)

// Busy — runner занят этапом задачи или публикацией.
func (s RunnerStatus) Busy() bool {
	return s == RunnerRunning || s == RunnerTesting || s == RunnerReview || s == RunnerDeploying
}
