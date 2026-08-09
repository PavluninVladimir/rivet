package domain

import "testing"

// Матрица из спеки backend/domain-model: полнота и корректность.

func TestTaskMatrixCompleteness(t *testing.T) {
	all := []TaskStatus{TaskQueued, TaskReady, TaskRunning, TaskTesting, TaskReview,
		TaskFixing, TaskBlocked, TaskFailed, TaskDone, TaskCancelled}

	for _, s := range all {
		if _, ok := taskTransitions[s]; !ok {
			t.Errorf("статус %s отсутствует в матрице", s)
		}
	}
	// Каждый нетерминальный статус имеет выход.
	for s, outs := range taskTransitions {
		if s == TaskDone || s == TaskCancelled {
			if len(outs) != 0 {
				t.Errorf("%s должен быть терминальным", s)
			}
			continue
		}
		if len(outs) == 0 {
			t.Errorf("%s — тупик", s)
		}
	}
	// Каждый статус, кроме queued (стартового), достижим.
	reachable := map[TaskStatus]bool{TaskQueued: true}
	for _, outs := range taskTransitions {
		for _, to := range outs {
			reachable[to] = true
		}
	}
	for _, s := range all {
		if !reachable[s] {
			t.Errorf("%s недостижим", s)
		}
	}
}

func TestTaskTransitions(t *testing.T) {
	allowed := [][2]TaskStatus{
		{TaskQueued, TaskReady}, {TaskQueued, TaskBlocked}, {TaskQueued, TaskCancelled},
		{TaskReady, TaskRunning}, {TaskReady, TaskQueued},
		{TaskRunning, TaskTesting}, {TaskRunning, TaskBlocked}, {TaskRunning, TaskFailed},
		{TaskTesting, TaskReview}, {TaskTesting, TaskFixing}, {TaskTesting, TaskFailed},
		{TaskReview, TaskDone}, {TaskReview, TaskFixing}, {TaskReview, TaskFailed},
		{TaskFixing, TaskTesting}, {TaskFixing, TaskBlocked},
		{TaskBlocked, TaskReady}, {TaskFailed, TaskReady}, {TaskFailed, TaskCancelled},
		// runner offline — перезапуск попытки
		{TaskRunning, TaskReady}, {TaskTesting, TaskReady}, {TaskReview, TaskReady}, {TaskFixing, TaskReady},
	}
	for _, p := range allowed {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("переход %s → %s должен быть разрешён", p[0], p[1])
		}
	}
	denied := [][2]TaskStatus{
		{TaskDone, TaskRunning}, {TaskCancelled, TaskReady}, {TaskQueued, TaskRunning},
		{TaskRunning, TaskReview}, {TaskReview, TaskRunning}, {TaskTesting, TaskDone},
		{TaskReady, TaskDone}, {TaskBlocked, TaskDone}, {TaskFixing, TaskDone},
	}
	for _, p := range denied {
		if p[0].CanTransition(p[1]) {
			t.Errorf("переход %s → %s должен быть запрещён", p[0], p[1])
		}
	}
}

func TestEpicTransitions(t *testing.T) {
	if !EpicPlanned.CanTransition(EpicRunning) || !EpicRunning.CanTransition(EpicPaused) ||
		!EpicPaused.CanTransition(EpicRunning) || !EpicRunning.CanTransition(EpicDone) ||
		!EpicDone.CanTransition(EpicArchived) {
		t.Error("базовый жизненный цикл Epic нарушен")
	}
	if EpicArchived.CanTransition(EpicRunning) || EpicDone.CanTransition(EpicRunning) {
		t.Error("возврат из done/archived запрещён")
	}
}

func TestRunnerCapabilities(t *testing.T) {
	r := Runner{Capabilities: []string{"coding", "review"}}
	if !r.HasCapabilities([]string{"coding"}) || !r.HasCapabilities(nil) {
		t.Error("подбор по подмножеству должен проходить")
	}
	if r.HasCapabilities([]string{"frontend"}) {
		t.Error("отсутствующая capability должна отклонять runner")
	}
}
