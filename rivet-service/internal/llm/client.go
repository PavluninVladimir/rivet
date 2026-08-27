// Package llm — клиенты провайдеров моделей по типу API подключения
// (add-model-connections, спека backend/model-connections): Anthropic
// Messages API и OpenAI-совместимый chat completions (вендоры, агрегаторы,
// локальные серверы). Два метода: список моделей (проверка ключа и
// обнаружение) и одно текстовое дополнение (планировщик).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// API — тип API подключения.
type API string

const (
	APIAnthropic API = "anthropic"
	APIOpenAI    API = "openai"
)

// Model — модель из ответа провайдера.
type Model struct {
	ID    string
	Label string
}

// Client — подключение к провайдеру. Пустой Key допустим (локальный сервер
// или авторизация целиком в Headers).
type Client struct {
	API     API
	BaseURL string
	Key     string
	Headers map[string]string
	// HTTP — клиент запросов; nil — по умолчанию с потолком 10 минут
	// (thinking-модели отвечают минутами).
	HTTP *http.Client
}

// ErrUnauthorized — провайдер отклонил ключ (401/403): состояние invalid.
var ErrUnauthorized = errors.New("провайдер отклонил ключ")

var defaultHTTP = &http.Client{Timeout: 10 * time.Minute}

// DefaultBaseURL — адрес API по умолчанию для типа API.
func DefaultBaseURL(api API) string {
	if api == APIAnthropic {
		return "https://api.anthropic.com"
	}
	return ""
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

func (c Client) base() string { return strings.TrimRight(c.BaseURL, "/") }

// ListModels — модели провайдера: у обоих типов API это GET /models.
// Свой потолок 30 с: проверка ключа и обнаружение не должны висеть.
func (c Client) ListModels(ctx context.Context) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch c.API {
	case APIAnthropic:
		return c.anthropicModels(ctx)
	case APIOpenAI:
		return c.openaiModels(ctx)
	}
	return nil, fmt.Errorf("неизвестный тип API %q", c.API)
}

// Complete — один текстовый запрос, один текстовый ответ.
func (c Client) Complete(ctx context.Context, model, prompt string) (string, error) {
	switch c.API {
	case APIAnthropic:
		return c.anthropicComplete(ctx, model, prompt)
	case APIOpenAI:
		return c.openaiComplete(ctx, model, prompt)
	}
	return "", fmt.Errorf("неизвестный тип API %q", c.API)
}

// ─── Anthropic ───────────────────────────────────────────────────────────

func (c Client) anthropic() anthropic.Client {
	opts := []option.RequestOption{option.WithAPIKey(c.Key), option.WithHTTPClient(c.httpClient()), option.WithMaxRetries(0)}
	if b := c.base(); b != "" {
		opts = append(opts, option.WithBaseURL(b))
	}
	for k, v := range c.Headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return anthropic.NewClient(opts...)
}

func anthropicErr(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("%w: HTTP %d", ErrUnauthorized, apiErr.StatusCode)
	}
	return err
}

func (c Client) anthropicModels(ctx context.Context) ([]Model, error) {
	cl := c.anthropic()
	// Список постраничный: обходим все страницы.
	iter := cl.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var out []Model
	for iter.Next() {
		m := iter.Current()
		out = append(out, Model{ID: m.ID, Label: m.DisplayName})
	}
	if err := iter.Err(); err != nil {
		return nil, anthropicErr(err)
	}
	return out, nil
}

func (c Client) anthropicComplete(ctx context.Context, model, prompt string) (string, error) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	cl := c.anthropic()
	resp, err := cl.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 16000,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		return "", anthropicErr(err)
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

// ─── OpenAI-совместимый ───────────────────────────────────────────────────

func (c Client) openaiRequest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if c.base() == "" {
		return nil, errors.New("base URL не задан")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	const maxBody = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("ответ провайдера больше %d МБ", maxBody>>20)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return raw, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnauthorized, resp.StatusCode)
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
}

func (c Client) openaiModels(ctx context.Context) ([]Model, error) {
	raw, err := c.openaiRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("невалидный ответ списка моделей: %w", err)
	}
	models := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID == "" {
			continue
		}
		models = append(models, Model{ID: m.ID, Label: m.Name})
	}
	return models, nil
}

func (c Client) openaiComplete(ctx context.Context, model, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 16000,
	})
	if err != nil {
		return "", err
	}
	raw, err := c.openaiRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("невалидный ответ: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("пустой ответ")
	}
	return out.Choices[0].Message.Content, nil
}
