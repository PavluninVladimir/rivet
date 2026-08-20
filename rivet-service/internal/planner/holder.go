package planner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Источник и состояние настроек модели (спека epic-decomposition «Настройка
// модели для декомпозиции», design add-operations-management: горячая
// замена планировщика).

// Source — откуда взят активный планировщик.
type Source string

const (
	SourceDB   Source = "db"
	SourceEnv  Source = "env"
	SourceNone Source = "none"
)

// Status — сведения об активном планировщике для состояния установки.
type Status struct {
	Source   Source
	Provider string
	Model    string
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

// DefaultModel — модель провайдера по умолчанию (api-contract).
func DefaultModel(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-opus-5"
	case "deepseek":
		return "deepseek-v4-flash"
	}
	return ""
}

// Build собирает планировщик провайдера; пустая модель — по умолчанию.
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

// ErrKeyRejected — провайдер явно отклонил ключ (401/403): состояние invalid.
var ErrKeyRejected = errors.New("провайдер отклонил ключ")

// probeHTTP — короткий клиент проверки: список моделей отвечает быстро.
var probeHTTP = &http.Client{Timeout: 15 * time.Second}

// Probe проверяет ключ дешёвым запросом списка моделей (токены не тратятся).
// nil — ключ принят; ErrKeyRejected — отклонён; другая ошибка — до
// провайдера не дошли (сеть), состояние остаётся unchecked (design).
func Probe(ctx context.Context, provider, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch provider {
	case "anthropic":
		client := anthropic.NewClient(option.WithAPIKey(key), option.WithHTTPClient(probeHTTP), option.WithMaxRetries(0))
		_, err := client.Models.List(ctx, anthropic.ModelListParams{})
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w: HTTP %d", ErrKeyRejected, apiErr.StatusCode)
		}
		return err
	case "deepseek":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, deepseekBase+"/models", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := probeHTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		switch resp.StatusCode {
		case http.StatusOK:
			return nil
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: HTTP %d", ErrKeyRejected, resp.StatusCode)
		default:
			return fmt.Errorf("deepseek: HTTP %d", resp.StatusCode)
		}
	}
	return fmt.Errorf("неизвестный провайдер модели %q", provider)
}
