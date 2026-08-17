// Package planner — декомпозиция Epic на задачи моделью (спека backend/epic-decomposition).
// Модель предлагает план; принимает его детерминированный код: валидация
// ацикличности, полноты критериев и ссылок. Некорректный план не принимается.
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

type Planner struct {
	c Completer
}

// PlannedTask — элемент плана от модели; deps ссылаются на индексы задач плана.
type PlannedTask struct {
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Criteria     []string `json:"criteria"`
	Deps         []int    `json:"deps"`
	Capabilities []string `json:"capabilities"`
	Estimate     int      `json:"estimate"`
}

// Decompose запрашивает у модели план Epic и валидирует его.
func (p *Planner) Decompose(ctx context.Context, epic domain.Epic, project domain.Project) ([]PlannedTask, error) {
	text, err := p.c.Complete(ctx, buildPrompt(epic, project))
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}

	plan, err := parsePlan(text)
	if err != nil {
		return nil, err
	}
	if err := Validate(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func buildPrompt(epic domain.Epic, project domain.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, `Ты — планировщик разработки. Декомпозируй Epic на небольшие задачи для автономных coding-агентов.

# Проект
Репозиторий: %s

# Epic: %s

%s

Верни ТОЛЬКО JSON-массив задач без пояснений и без markdown-ограждений. Каждая задача:
{"title": "...", "description": "подробное самодостаточное описание для агента, который не видит остальной план",
 "criteria": ["проверяемый критерий приёмки", ...], "deps": [индексы задач-зависимостей в этом массиве],
 "capabilities": ["coding"], "estimate": 1-5}

Правила: задачи небольшие (одна сессия агента); зависимости образуют ациклический граф;
у каждой задачи минимум один проверяемый acceptance criterion; независимые задачи не связывай
зависимостями — они пойдут параллельно.`, project.Repo(), epic.Title, epic.Goal)
	return b.String()
}

// parsePlan достаёт JSON-массив из текста ответа (терпимо к ограждениям).
func parsePlan(text string) ([]PlannedTask, error) {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("в ответе модели нет JSON-массива")
	}
	var plan []PlannedTask
	if err := json.Unmarshal([]byte(text[start:end+1]), &plan); err != nil {
		return nil, fmt.Errorf("невалидный JSON плана: %w", err)
	}
	return plan, nil
}

// Validate — детерминированная проверка плана (спека: цикл, дубли,
// задача без критериев не принимаются).
func Validate(plan []PlannedTask) error {
	if len(plan) == 0 {
		return fmt.Errorf("план пуст")
	}
	seen := map[string]bool{}
	deps := map[string][]string{}
	for i, t := range plan {
		if strings.TrimSpace(t.Title) == "" {
			return fmt.Errorf("задача %d без названия", i)
		}
		if seen[t.Title] {
			return fmt.Errorf("дубль задачи %q", t.Title)
		}
		seen[t.Title] = true
		if len(t.Criteria) == 0 {
			return fmt.Errorf("задача %q без acceptance criteria", t.Title)
		}
		key := fmt.Sprint(i)
		seenDep := map[int]bool{}
		for _, d := range t.Deps {
			if d < 0 || d >= len(plan) {
				return fmt.Errorf("задача %q ссылается на несуществующую зависимость %d", t.Title, d)
			}
			if d == i {
				return fmt.Errorf("задача %q зависит от себя", t.Title)
			}
			// Дубль зависимости упал бы на первичном ключе task_deps при материализации.
			if seenDep[d] {
				return fmt.Errorf("задача %q дублирует зависимость %d", t.Title, d)
			}
			seenDep[d] = true
			deps[key] = append(deps[key], fmt.Sprint(d))
		}
		if _, ok := deps[key]; !ok {
			deps[key] = nil
		}
	}
	return store.ValidateDAG(deps)
}

// Materialize создаёт задачи плана в хранилище в топологическом порядке.
func Materialize(ctx context.Context, st *store.Store, epicID string, plan []PlannedTask) ([]domain.Task, error) {
	order, err := topoOrder(plan)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(plan))
	out := make([]domain.Task, 0, len(plan))
	for _, idx := range order {
		pt := plan[idx]
		depIDs := make([]string, 0, len(pt.Deps))
		for _, d := range pt.Deps {
			depIDs = append(depIDs, ids[d])
		}
		crit := make([]domain.Criterion, 0, len(pt.Criteria))
		for _, c := range pt.Criteria {
			crit = append(crit, domain.Criterion{Text: c})
		}
		task, err := st.CreateTask(ctx, epicID, store.NewTask{
			Title: pt.Title, Description: pt.Description, Criteria: crit,
			Deps: depIDs, Capabilities: pt.Capabilities, Estimate: pt.Estimate,
		})
		if err != nil {
			return nil, err
		}
		ids[idx] = task.ID
		out = append(out, task)
	}
	return out, nil
}

func topoOrder(plan []PlannedTask) ([]int, error) {
	indeg := make([]int, len(plan))
	adj := make([][]int, len(plan))
	for i, t := range plan {
		for _, d := range t.Deps {
			adj[d] = append(adj[d], i)
			indeg[i]++
		}
	}
	var queue, order []int
	for i, n := range indeg {
		if n == 0 {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		order = append(order, i)
		for _, next := range adj[i] {
			if indeg[next]--; indeg[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(order) != len(plan) {
		return nil, fmt.Errorf("план содержит цикл")
	}
	return order, nil
}
