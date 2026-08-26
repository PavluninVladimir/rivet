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

// taskTransitions — матрица переходов из спеки backend/domain-model. Между
// активными статусами (running, testing, review, fixing) порядок задаёт
// процесс проекта (спека backend/process), поэтому они переходят друг в
// друга свободно; из ready задача входит на любой шаг (возобновление после
// потери runner'а).
var taskTransitions = map[TaskStatus][]TaskStatus{
	TaskQueued:    {TaskReady, TaskBlocked, TaskCancelled},
	TaskReady:     {TaskRunning, TaskTesting, TaskReview, TaskFixing, TaskQueued, TaskCancelled},
	TaskRunning:   {TaskTesting, TaskReview, TaskFixing, TaskBlocked, TaskFailed, TaskReady, TaskCancelled},
	TaskTesting:   {TaskReview, TaskFixing, TaskRunning, TaskBlocked, TaskFailed, TaskReady},
	TaskReview:    {TaskDone, TaskFixing, TaskTesting, TaskRunning, TaskBlocked, TaskFailed, TaskReady},
	TaskFixing:    {TaskTesting, TaskReview, TaskRunning, TaskBlocked, TaskFailed, TaskReady},
	TaskBlocked:   {TaskQueued, TaskReady, TaskFixing, TaskCancelled},
	TaskFailed:    {TaskQueued, TaskReady, TaskFixing, TaskCancelled},
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
