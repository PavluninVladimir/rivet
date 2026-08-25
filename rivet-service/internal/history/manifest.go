// Package history — импорт истории разработки проекта (спека domain-model
// «Импорт истории проекта»): манифест выполненных Epic'ов и задач с датами
// и PR, разбор архива OpenSpec в манифест и привязка change'ей к PR.
//
// Сервер знает только манифест: откуда он собран — дело клиента.
package history

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Лимиты манифеста: импорт — разовая операция владельца, а не поток.
const (
	MaxEpics        = 1000
	MaxTasksPerEpic = 200
)

// Manifest — история проекта: источник и Epic'и.
type Manifest struct {
	Source string `json:"source"`
	Epics  []Epic `json:"epics"`
}

// Epic — выполненный Epic из истории. Key — ключ источника, уникальный в
// проекте: по нему повторный импорт обновляет, а не дублирует.
type Epic struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Goal      string    `json:"goal"`
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at"`
	Tasks     []Task    `json:"tasks"`
}

// Task — пункт истории: выполнен или не выполнен на момент архивации,
// с PR репозитория, к которому относился.
type Task struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
	Repo  string `json:"repo,omitempty"`
	PRURL string `json:"pr_url,omitempty"`
}

// ErrInvalid — манифест не проходит валидацию.
var ErrInvalid = errors.New("некорректный манифест истории")

// Validate проверяет манифест: ключи, названия, даты и лимиты.
func (m Manifest) Validate() error {
	if len(m.Epics) == 0 {
		return fmt.Errorf("%w: нет Epic'ов", ErrInvalid)
	}
	if len(m.Epics) > MaxEpics {
		return fmt.Errorf("%w: больше %d Epic'ов", ErrInvalid, MaxEpics)
	}
	seen := map[string]bool{}
	for i, e := range m.Epics {
		if strings.TrimSpace(e.Key) == "" {
			return fmt.Errorf("%w: у Epic'а #%d пустой ключ", ErrInvalid, i+1)
		}
		if seen[e.Key] {
			return fmt.Errorf("%w: ключ %q повторяется", ErrInvalid, e.Key)
		}
		seen[e.Key] = true
		if strings.TrimSpace(e.Title) == "" {
			return fmt.Errorf("%w: у Epic'а %q пустое название", ErrInvalid, e.Key)
		}
		if e.CreatedAt.IsZero() {
			return fmt.Errorf("%w: у Epic'а %q нет даты", ErrInvalid, e.Key)
		}
		if len(e.Tasks) > MaxTasksPerEpic {
			return fmt.Errorf("%w: у Epic'а %q больше %d задач", ErrInvalid, e.Key, MaxTasksPerEpic)
		}
		for j, t := range e.Tasks {
			if strings.TrimSpace(t.Title) == "" {
				return fmt.Errorf("%w: у задачи #%d Epic'а %q пустое название", ErrInvalid, j+1, e.Key)
			}
		}
	}
	return nil
}

// Normalize приводит манифест к каноническому виду: даты завершения без
// значения берутся из даты создания, обрезаются длинные тексты.
func (m Manifest) Normalize() Manifest {
	for i := range m.Epics {
		e := &m.Epics[i]
		e.Title = clip(strings.TrimSpace(e.Title), 200)
		e.Goal = clip(strings.TrimSpace(e.Goal), 4000)
		if e.DoneAt.IsZero() || e.DoneAt.Before(e.CreatedAt) {
			e.DoneAt = e.CreatedAt
		}
		for j := range e.Tasks {
			e.Tasks[j].Title = clip(strings.TrimSpace(e.Tasks[j].Title), 300)
		}
	}
	if m.Source == "" {
		m.Source = "manifest"
	}
	return m
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
