package planner

import (
	"context"

	"github.com/PavluninVladimir/rivet/internal/llm"
)

// Completer — провайдер модели: один текстовый запрос, один текстовый ответ.
type Completer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// clientCompleter — Completer поверх клиента подключения (internal/llm).
type clientCompleter struct {
	c     llm.Client
	model string
}

func (c clientCompleter) Complete(ctx context.Context, prompt string) (string, error) {
	return c.c.Complete(ctx, c.model, prompt)
}

// FromClient — планировщик на клиенте подключения и модели.
func FromClient(c llm.Client, model string) *Planner {
	return &Planner{c: clientCompleter{c: c, model: model}}
}

// New — planner на Anthropic API с моделью по умолчанию (запасной источник
// из окружения установки).
func New(apiKey string) *Planner { return NewAnthropic(apiKey, "") }

// NewAnthropic — planner на Anthropic API; пустая модель — по умолчанию.
func NewAnthropic(apiKey, model string) *Planner {
	if model == "" {
		model = DefaultModel("anthropic")
	}
	return FromClient(llm.Client{API: llm.APIAnthropic, BaseURL: llm.DefaultBaseURL(llm.APIAnthropic), Key: apiKey}, model)
}

// NewDeepSeek — planner на DeepSeek API (OpenAI-совместимый).
func NewDeepSeek(apiKey, model string) *Planner {
	if model == "" {
		model = DefaultModel("deepseek")
	}
	return FromClient(llm.Client{API: llm.APIOpenAI, BaseURL: deepseekBase, Key: apiKey}, model)
}

// deepseekBase — адрес DeepSeek API; переменная, чтобы тесты подменяли его.
var deepseekBase = "https://api.deepseek.com"
