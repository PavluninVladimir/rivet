package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Completer — провайдер модели: один текстовый запрос, один текстовый ответ.
type Completer interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// --- Anthropic ---

type anthropicCompleter struct {
	client anthropic.Client
	model  string
}

func (a anthropicCompleter) Complete(ctx context.Context, prompt string) (string, error) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 16000,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", fmt.Errorf("модель отклонила запрос (refusal)")
	}
	var text string
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text += b.Text
		}
	}
	return text, nil
}

// --- DeepSeek (OpenAI-совместимый chat completions API) ---

type deepseekCompleter struct {
	apiKey string
	model  string
}

// deepseekHTTP — клиент с потолком на весь запрос: thinking-модели отвечают
// минутами, но зависший upstream не должен держать handler бесконечно.
var deepseekHTTP = &http.Client{Timeout: 10 * time.Minute}

func (d deepseekCompleter) Complete(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": d.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": 16000,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		deepseekBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := deepseekHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("deepseek: невалидный ответ: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("deepseek: пустой ответ")
	}
	return out.Choices[0].Message.Content, nil
}

// deepseekBase — адрес DeepSeek API; переменная, чтобы тесты подменяли его.
var deepseekBase = "https://api.deepseek.com"

// New — planner на Anthropic API с моделью по умолчанию.
func New(apiKey string) *Planner { return NewAnthropic(apiKey, "") }

// NewAnthropic — planner на Anthropic API; пустая модель — по умолчанию.
func NewAnthropic(apiKey, model string) *Planner {
	if model == "" {
		model = DefaultModel("anthropic")
	}
	return &Planner{c: anthropicCompleter{client: anthropic.NewClient(option.WithAPIKey(apiKey)), model: model}}
}

// NewDeepSeek — planner на DeepSeek API.
func NewDeepSeek(apiKey, model string) *Planner {
	if model == "" {
		model = DefaultModel("deepseek")
	}
	return &Planner{c: deepseekCompleter{apiKey: apiKey, model: model}}
}
