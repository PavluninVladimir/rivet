// Package llm — клиенты провайдеров моделей по типу API подключения
// (add-model-connections, спека backend/model-connections): Anthropic
// Messages API и OpenAI-совместимый chat completions (вендоры, агрегаторы,
// локальные серверы). Методы: список моделей (проверка ключа и
// обнаружение), одно текстовое дополнение (планировщик) и диалог с
// инструментами для агентского цикла Rivet (chat.go, add-native-agent).
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
	"net/netip"
	"net/url"
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
	// Retry — повторы диалога (Chat) на перегрузку и сбои сети.
	Retry RetryPolicy
}

// ErrUnauthorized — провайдер отклонил ключ (401/403): состояние invalid.
var ErrUnauthorized = errors.New("провайдер отклонил ключ")

// AuthError — отказ провайдера по ключу с кодом ответа: 401 — ключ
// недействителен (отозван, неверен), 403 — у ключа нет прав (модель,
// регион, блокировка сети). errors.Is(err, ErrUnauthorized) верно для
// обоих, текст прежний: «провайдер отклонил ключ: HTTP 401».
type AuthError struct{ Status int }

func (e *AuthError) Error() string { return fmt.Sprintf("%v: HTTP %d", ErrUnauthorized, e.Status) }

// Is — AuthError это ErrUnauthorized.
func (e *AuthError) Is(target error) bool { return target == ErrUnauthorized }

func authError(status int) error { return &AuthError{Status: status} }

// KeyRejected — провайдер отклонил сам ключ (HTTP 401).
func KeyRejected(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

// RejectionStatus — код отказа провайдера по ключу (401 или 403) в итоге
// стадии с вердиктом runner'а connection. Код читается только из первой
// строки: там runner пишет ошибку агента и не режет её, текст агента идёт
// ниже и на разбор не влияет. 0 — отказа по ключу в первой строке нет.
func RejectionStatus(detail string) int {
	first, _, _ := strings.Cut(detail, "\n")
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if strings.Contains(first, (&AuthError{Status: status}).Error()) {
			return status
		}
	}
	return 0
}

var defaultHTTP = &http.Client{Timeout: 30 * time.Minute}

// IsLocalURL — адрес сервера модели на этой же машине (Ollama, LM Studio):
// такому серверу ключ не нужен. Правило общее у runner'а, который без
// ключа отказывается идти на нелокальный адрес, и у control plane, который
// не назначает стадию на такое подключение. Хост разбирается целиком, без
// поиска подстрок и без DNS: localhost (и *.localhost) или loopback-адрес
// (127.0.0.0/8, ::1, в том числе IPv4 в IPv6). Для HTTP(S) без userinfo
// также допустим точный Docker-host alias host.docker.internal.
func IsLocalURL(u string) bool {
	parsed, err := url.Parse(strings.TrimSpace(u))
	if err != nil {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return false
	}
	if host == "host.docker.internal" {
		return parsed.User == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

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
	// Кешированные токены входят в InputTokens. nil — провайдер не
	// сообщил разбивку. CacheWriteTokens имеет отдельную схему цены.
	CachedInputTokens *int64
	CacheWriteTokens  *int64
	// CostIncomplete — есть неучтённая попытка или неполная разбивка:
	// известные токены сохраняются, но итоговую стоимость считать нельзя.
	CostIncomplete bool
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
	// Без автоподстановки из окружения: SDK иначе взял бы ANTHROPIC_AUTH_TOKEN,
	// профиль или WIF runner'а и отправил бы их на чужой base URL. Все опции
	// задаются явно.
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(c.Key), option.WithHTTPClient(c.httpClient()), option.WithMaxRetries(0)}
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
		return authError(apiErr.StatusCode)
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
	retried := false
	for attempt := 0; ; attempt++ {
		text, usage, err := c.anthropicOnce(ctx, model, prompt, limit)
		usage.MaxTokens = limit
		var apiErr *anthropic.Error
		if err != nil && attempt < 2 && errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
			if next := lowerLimit(err.Error(), limit); next > 0 {
				limit = next
				retried = true
				continue
			}
		}
		usage.CostIncomplete = usage.CostIncomplete || retried
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
	var usage Usage
	if maxTokens >= anthropicStreamFrom {
		// Длинный ответ — потоком, сообщение собирается из событий.
		stream := cl.Messages.NewStreaming(ctx, params)
		msg := anthropic.Message{}
		stopped := false
		var accErr error
		var reported anthropic.Usage
		var finalOutput *int64
		for stream.Next() {
			ev := stream.Current()
			if ev.Type == "message_start" {
				reported = ev.Message.Usage
			}
			if ev.Type == "message_delta" {
				if ev.Usage.JSON.OutputTokens.Valid() {
					finalOutput = reportedToken(ev.Usage.OutputTokens, true)
				}
				if ev.Usage.JSON.InputTokens.Valid() {
					reported.InputTokens, reported.JSON.InputTokens = ev.Usage.InputTokens, ev.Usage.JSON.InputTokens
				}
				if ev.Usage.JSON.CacheReadInputTokens.Valid() {
					reported.CacheReadInputTokens, reported.JSON.CacheReadInputTokens = ev.Usage.CacheReadInputTokens, ev.Usage.JSON.CacheReadInputTokens
				}
				if ev.Usage.JSON.CacheCreationInputTokens.Valid() {
					reported.CacheCreationInputTokens, reported.JSON.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens, ev.Usage.JSON.CacheCreationInputTokens
				}
			}
			if accErr = msg.Accumulate(ev); accErr != nil {
				break
			}
			if ev.Type == "message_stop" {
				stopped = true
			}
		}
		err := stream.Err()
		_ = stream.Close()
		// Вход message_start известен и при обрыве. Начальный output не
		// является полным расходом ответа: нужен сообщённый финальный delta.
		usage = anthropicCompleteUsage(reported)
		usage.OutputTokens = finalOutput
		usage.CostIncomplete = usage.CostIncomplete || !stopped || accErr != nil || err != nil
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
		usage = anthropicCompleteUsage(resp.Usage)
	}
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

// reportedToken сохраняет различие между отсутствующим значением и явным0.
// Отрицательный счётчик не является допустимым расходом провайдера.
func reportedToken(n int64, present bool) *int64 {
	if !present || n < 0 {
		return nil
	}
	return &n
}

func anthropicCompleteUsage(raw anthropic.Usage) Usage {
	u := Usage{
		InputTokens:       reportedToken(raw.InputTokens, raw.JSON.InputTokens.Valid()),
		OutputTokens:      reportedToken(raw.OutputTokens, raw.JSON.OutputTokens.Valid()),
		CachedInputTokens: reportedToken(raw.CacheReadInputTokens, raw.JSON.CacheReadInputTokens.Valid()),
		CacheWriteTokens:  reportedToken(raw.CacheCreationInputTokens, raw.JSON.CacheCreationInputTokens.Valid()),
	}
	// Anthropic input_tokens исключает чтение/запись кеша. Без разбивки
	// сохраняем известный fresh-вход, но не объявляем его полным расходом.
	if u.InputTokens == nil || u.CachedInputTokens == nil || u.CacheWriteTokens == nil {
		u.CostIncomplete = true
		return u
	}
	const maxInt64 = int64(1<<63 - 1)
	total := *u.InputTokens
	for _, n := range []int64{*u.CachedInputTokens, *u.CacheWriteTokens} {
		if n > maxInt64-total {
			u.CostIncomplete = true
			return u
		}
		total += n
	}
	u.InputTokens = &total
	return u
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
		return nil, authError(resp.StatusCode)
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
	retried := false
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
		retried = true
		raw, err = request(limit)
	}
	if err != nil {
		return "", Usage{MaxTokens: limit, CostIncomplete: retried}, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens         *int64 `json:"prompt_tokens"`
			CompletionTokens     *int64 `json:"completion_tokens"`
			PromptCacheHitTokens *int64 `json:"prompt_cache_hit_tokens"`
			PromptTokensDetails  *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", Usage{}, fmt.Errorf("невалидный ответ: %w", err)
	}
	usage := Usage{MaxTokens: limit, CostIncomplete: retried}
	if out.Usage != nil {
		if n := out.Usage.PromptTokens; n != nil {
			usage.InputTokens = reportedToken(*n, true)
		}
		if n := out.Usage.CompletionTokens; n != nil {
			usage.OutputTokens = reportedToken(*n, true)
		}
		cache := out.Usage.PromptCacheHitTokens
		if details := out.Usage.PromptTokensDetails; details != nil && details.CachedTokens != nil {
			if cache != nil && *cache != *details.CachedTokens {
				usage.CostIncomplete = true
			}
			cache = details.CachedTokens
		}
		if cache != nil {
			usage.CachedInputTokens = reportedToken(*cache, true)
			usage.CostIncomplete = usage.CostIncomplete || usage.CachedInputTokens == nil
		}
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
