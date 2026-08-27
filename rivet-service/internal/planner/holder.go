package planner

import (
	"fmt"
	"sync"
)

// Источник и состояние модели декомпозиции (спека epic-decomposition
// «Настройка модели для декомпозиции», add-model-connections: модель из
// каталога подключений, окружение установки как запасной источник).

// Source — откуда взят активный планировщик.
type Source string

const (
	SourceCatalog Source = "catalog"
	SourceEnv     Source = "env"
	SourceNone    Source = "none"
)

// Status — сведения об активном планировщике для состояния установки.
type Status struct {
	Source Source
	// ConnectionID — подключение из каталога; у env — имя провайдера.
	ConnectionID string
	Model        string
	// State — результат проверки ключа (ok/invalid/unchecked); для env — "unchecked".
	State  string
	Detail string
}

// Holder — атомарно заменяемый планировщик: API читает его на каждую
// декомпозицию, администратор меняет без перезапуска.
type Holder struct {
	mu sync.RWMutex
	p  *Planner
	st Status
}

func (h *Holder) Set(p *Planner, st Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p, h.st = p, st
}

// Get — текущий планировщик (nil, если модель не настроена) и его статус.
func (h *Holder) Get() (*Planner, Status) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.p == nil && h.st.Source == "" {
		return nil, Status{Source: SourceNone, State: "unchecked"}
	}
	return h.p, h.st
}

// DefaultModel — модель провайдера окружения по умолчанию.
func DefaultModel(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-opus-5"
	case "deepseek":
		return "deepseek-v4-flash"
	}
	return ""
}

// Build собирает планировщик провайдера из окружения; пустая модель — по умолчанию.
func Build(provider, key, model string) (*Planner, error) {
	if model == "" {
		model = DefaultModel(provider)
	}
	switch provider {
	case "anthropic":
		return NewAnthropic(key, model), nil
	case "deepseek":
		return NewDeepSeek(key, model), nil
	}
	return nil, fmt.Errorf("неизвестный провайдер модели %q", provider)
}
