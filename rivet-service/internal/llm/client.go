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
	"net"
	"net/http"
	"regexp"
	"strconv"
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
	// HTTP — клиент запросов; nil — по умолчанию с потолком 30 минут
	// (thinking-модели и длинные ответы Аналитика идут минутами). Срок
	// конкретного запроса задаёт контекст вызывающего.
	HTTP *http.Client
}

// ErrUnauthorized — провайдер отклонил ключ (401/403): состояние invalid.
var ErrUnauthorized = errors.New("провайдер отклонил ключ")

var defaultHTTP = &http.Client{Timeout: 30 * time.Minute}

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

// Usage — токены запроса по ответу провайдера; nil — провайдер значение
// не сообщил (спека observability: «нет данных», а не ноль).
type Usage struct {
	InputTokens  *int64
	OutputTokens *int64
	// MaxTokens — лимит ответа, с которым прошёл запрос: ниже запрошенного,
	// если провайдер отклонил большой лимит и запрос повторён.
	MaxTokens int
}

// ErrTruncated — ответ оборван лимитом токенов (`stop_reason == "max_tokens"`
// у Anthropic, `finish_reason == "length"` у OpenAI-совместимых). Такой
// текст не отдаётся вызывающему: оборванный JSON не должен доходить до
// разбора, запрос повторяется (design add-product-discovery, решение 6).
var ErrTruncated = errors.New("ответ модели оборван лимитом токенов")

// ErrStreamCut — поток ответа закрыт до конца сообщения без ошибки
// провайдера (обрыв связи, прокси). Ошибка временная: запрос повторяется.
var ErrStreamCut = errors.New("поток ответа модели оборван до конца сообщения")

// DefaultMaxTokens — лимит ответа по умолчанию (планировщик).
const DefaultMaxTokens = 16000

// anthropicStreamFrom — с какого лимита ответ Anthropic запрашивается
// потоком: SDK отказывает в обычном запросе, если ответ может идти дольше
// 10 минут (лимит больше ~21 000 токенов).
const anthropicStreamFrom = 20000

// Complete — один текстовый запрос, один текстовый ответ.
func (c Client) Complete(ctx context.Context, model, prompt string) (string, error) {
	text, _, err := c.CompleteWithUsage(ctx, model, prompt, 0)
	return text, err
}

// CompleteWithUsage — текстовый запрос с токенами ответа. maxTokens —
// лимит ответа, 0 — DefaultMaxTokens. Оборванный по лимиту ответ —
// ошибка ErrTruncated. Общий клиент с инструментами для агентского цикла
// (add-native-agent) строится рядом; при слиянии этот метод станет
// обёрткой над ним.
func (c Client) CompleteWithUsage(ctx context.Context, model, prompt string, maxTokens int) (string, Usage, error) {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	switch c.API {
	case APIAnthropic:
		return c.anthropicComplete(ctx, model, prompt, maxTokens)
	case APIOpenAI:
		return c.openaiComplete(ctx, model, prompt, maxTokens)
	}
	return "", Usage{}, fmt.Errorf("неизвестный тип API %q", c.API)
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

func (c Client) anthropicComplete(ctx context.Context, model, prompt string, maxTokens int) (string, Usage, error) {
	// Провайдер с меньшим потолком ответа отклоняет max_tokens (400):
	// повтор с потолком из текста ошибки или на шаг ниже.
	limit := maxTokens
	for attempt := 0; ; attempt++ {
		text, usage, err := c.anthropicOnce(ctx, model, prompt, limit)
		usage.MaxTokens = limit
		var apiErr *anthropic.Error
		if err != nil && attempt < 2 && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
			if next := lowerLimit(err.Error(), limit); next > 0 {
				limit = next
				continue
			}
		}
		return text, usage, err
	}
}

func (c Client) anthropicOnce(ctx context.Context, model, prompt string, maxTokens int) (string, Usage, error) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	cl := c.anthropic()
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	}
	var resp *anthropic.Message
	if maxTokens >= anthropicStreamFrom {
		// Длинный ответ — потоком, сообщение собирается из событий.
		stream := cl.Messages.NewStreaming(ctx, params)
		msg := anthropic.Message{}
		stopped := false
		var accErr error
		for stream.Next() {
			ev := stream.Current()
			if accErr = msg.Accumulate(ev); accErr != nil {
				break
			}
			if ev.Type == "message_stop" {
				stopped = true
			}
		}
		err := stream.Err()
		_ = stream.Close()
		// Токены message_start известны и при обрыве: расход учитывается.
		var usage Usage
		if msg.ID != "" {
			in, out := msg.Usage.InputTokens, msg.Usage.OutputTokens
			usage = Usage{InputTokens: &in, OutputTokens: &out}
		}
		switch {
		case accErr != nil:
			return "", usage, accErr
		case err != nil:
			return "", usage, anthropicErr(err)
		case !stopped:
			// Поток закрыт без конца сообщения и без ошибки (прокси,
			// балансировщик): это обрыв связи, а не ответ модели.
			return "", usage, ErrStreamCut
		}
		resp = &msg
	} else {
		r, err := cl.Messages.New(ctx, params)
		if err != nil {
			return "", Usage{}, anthropicErr(err)
		}
		resp = r
	}
	in, out := resp.Usage.InputTokens, resp.Usage.OutputTokens
	usage := Usage{InputTokens: &in, OutputTokens: &out}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return "", usage, fmt.Errorf("модель отклонила запрос (refusal)")
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		return "", usage, ErrTruncated
	}
	var text string
	for _, block := range resp.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text += b.Text
		}
	}
	return text, usage, nil
}

// limitRes — потолок ответа в тексте отказа провайдера: «valid range of
// max_tokens is [1, 8192]», «max_tokens: 64000 > 32000».
var limitRes = []*regexp.Regexp{
	regexp.MustCompile(`\[\s*1\s*,\s*(\d+)\s*\]`),
	regexp.MustCompile(`>\s*(\d+)`),
}

// lowerLimit — лимит ответа для повтора после отказа провайдера по
// max_tokens: потолок из текста ошибки, иначе шаг вниз 16000 → 8192.
// 0 — повторять бесполезно: отказ не про размер лимита (параметр не
// поддерживается моделью или ошибка о другом).
func lowerLimit(body string, cur int) int {
	low := strings.ToLower(body)
	if !strings.Contains(low, "max_tokens") || strings.Contains(low, "not supported") ||
		strings.Contains(low, "unsupported") || strings.Contains(low, "max_completion_tokens") {
		return 0
	}
	for _, re := range limitRes {
		if m := re.FindStringSubmatch(body); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && n < cur {
				return n
			}
		}
	}
	switch {
	case cur > DefaultMaxTokens:
		return DefaultMaxTokens
	case cur > 8192:
		return 8192
	}
	return 0
}

// IsTransient — временная ошибка провайдера: перегрузка, лимит частоты,
// 5xx, обрыв потока или сети. Такой запрос стоит повторить. Отказ в
// ключе, неверный запрос и истёкший срок контекста временными не считаются.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrTruncated) {
		return false
	}
	if errors.Is(err, ErrStreamCut) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var status *httpStatusError
	if errors.As(err, &status) {
		return status.Status == http.StatusTooManyRequests || status.Status >= 500
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500 {
			return true
		}
		// Ответ с кодом ошибки решает по коду: 400 с «overloaded» в тексте
		// не повторяется. Подстроки — только для ошибки внутри потока
		// (статус 200 потока) и ошибок без статуса.
		if apiErr.StatusCode != 0 && apiErr.StatusCode != http.StatusOK {
			return false
		}
	}
	// Ошибка посреди потока приходит со статусом 200 потока и типом в теле.
	low := strings.ToLower(err.Error())
	for _, s := range []string{"overloaded", "rate_limit", "api_error", "connection reset", "broken pipe", "eof"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr)
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
	return nil, &httpStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
}

// httpStatusError — ответ провайдера с кодом ошибки; тело урезано в тексте.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	msg := e.Body
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
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

func (c Client) openaiComplete(ctx context.Context, model, prompt string, maxTokens int) (string, Usage, error) {
	request := func(limit int) ([]byte, error) {
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": limit,
		})
		if err != nil {
			return nil, err
		}
		return c.openaiRequest(ctx, http.MethodPost, "/chat/completions", body)
	}
	// Провайдер с меньшим потолком ответа отклоняет большой max_tokens
	// (HTTP 400): повтор с потолком из текста ошибки или на шаг ниже.
	limit := maxTokens
	raw, err := request(limit)
	for attempt := 0; err != nil && attempt < 2; attempt++ {
		var bad *httpStatusError
		if !errors.As(err, &bad) || bad.Status != http.StatusBadRequest {
			break
		}
		next := lowerLimit(bad.Body, limit)
		if next == 0 {
			break
		}
		limit = next
		raw, err = request(limit)
	}
	if err != nil {
		return "", Usage{MaxTokens: limit}, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", Usage{}, fmt.Errorf("невалидный ответ: %w", err)
	}
	usage := Usage{MaxTokens: limit}
	if out.Usage != nil {
		usage.InputTokens, usage.OutputTokens = out.Usage.PromptTokens, out.Usage.CompletionTokens
	}
	if len(out.Choices) == 0 {
		return "", usage, fmt.Errorf("пустой ответ")
	}
	// length — лимит ответа, insufficient_system_resource (DeepSeek) —
	// ответ прерван провайдером: оба значат оборванный текст.
	switch out.Choices[0].FinishReason {
	case "length", "insufficient_system_resource":
		return "", usage, ErrTruncated
	}
	return out.Choices[0].Message.Content, usage, nil
}
