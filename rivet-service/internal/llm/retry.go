package llm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Повторы запросов к провайдеру (design add-native-agent, решение 3): 429,
// 5xx и сетевые сбои повторяются с экспоненциальной паузой 1, 2, 4, 8, 16 с
// с джиттером, Retry-After в приоритете, не больше пяти повторов.

// RetryPolicy — настройка повторов. Нулевое значение — значения по
// умолчанию; MaxRetries < 0 отключает повторы.
type RetryPolicy struct {
	MaxRetries int
	// Base — первая пауза (по умолчанию 1 с).
	Base time.Duration
	// MaxWait — потолок одной паузы, в том числе из Retry-After
	// (по умолчанию 60 с).
	MaxWait time.Duration
	// Sleep — ожидание (подмена в тестах); nil — таймер с учётом ctx.
	Sleep func(ctx context.Context, d time.Duration) error
	// OnRetry — уведомление о повторе: номер повтора (с 1), пауза, ошибка.
	OnRetry func(attempt int, wait time.Duration, err error)
}

const (
	defaultRetries = 5
	defaultBase    = time.Second
	defaultMaxWait = 60 * time.Second
)

// APIError — ответ провайдера с кодом ошибки.
type APIError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// ErrModelUnsupported — провайдер отверг модель (нет такой модели или она
// не поддерживает запрос): стадия неуспешна с причиной «модель не
// поддерживается».
var ErrModelUnsupported = errors.New("модель не поддерживается")

// retryable — стоит ли повторить запрос: перегрузка, ошибка сервера, сбой
// сети, таймаут HTTP-клиента. Отмену контекста вызывающего проверяет retry
// до этого: истёкший таймаут самого запроса — повод повторить, а не
// остановиться.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrModelUnsupported) || errors.Is(err, ErrRefused) || errors.Is(err, ErrContextWindow) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Обрыв соединения посреди ответа (io.ErrUnexpectedEOF, reset by peer)
	// приходит не как net.Error.
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "unexpected EOF") || strings.HasSuffix(msg, "EOF")
}

// contextWindowExceeded — тело ошибки говорит о переполнении окна
// контекста (разные формулировки у провайдеров).
func contextWindowExceeded(status int, body string) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity && status != http.StatusRequestEntityTooLarge {
		return false
	}
	b := strings.ToLower(body)
	for _, s := range []string{
		"model_context_window_exceeded", "context_length_exceeded", "context window",
		"prompt is too long", "maximum context length", "too many tokens", "reduce the length",
	} {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

// interruptedError — генерация прервана провайдером (нештатная причина
// остановки, оборванный поток): новый запрос безопасен, вызовы ответа не
// исполнялись. Повторяемая ошибка со статусом 503.
func interruptedError(reason string) error {
	return &APIError{Status: http.StatusServiceUnavailable, Body: "генерация прервана: " + reason}
}

// retryAfterOf — пауза из Retry-After ответа провайдера (секунды или
// HTTP-дата); 0 — заголовка нет.
func retryAfterOf(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func (p RetryPolicy) limits() (retries int, base, maxWait time.Duration) {
	retries, base, maxWait = p.MaxRetries, p.Base, p.MaxWait
	if retries == 0 {
		retries = defaultRetries
	}
	if retries < 0 {
		retries = 0
	}
	if base <= 0 {
		base = defaultBase
	}
	if maxWait <= 0 {
		maxWait = defaultMaxWait
	}
	return retries, base, maxWait
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retry выполняет запрос с повторами по политике клиента.
func (c Client) retry(ctx context.Context, do func() error) error {
	retries, base, maxWait := c.Retry.limits()
	sleep := c.Retry.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for attempt := 0; ; attempt++ {
		err := do()
		// Отмена или дедлайн вызывающего — конец без повторов.
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil || attempt >= retries || !retryable(err) {
			if err != nil && attempt > 0 && retryable(err) {
				return fmt.Errorf("провайдер не ответил после %d повторов: %w", attempt, err)
			}
			return err
		}
		wait := base << attempt
		// Джиттер ±20%: одновременные сессии не бьют провайдера разом.
		wait += time.Duration((rand.Float64()*0.4 - 0.2) * float64(wait))
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
			wait = apiErr.RetryAfter
		}
		if wait > maxWait {
			wait = maxWait
		}
		if c.Retry.OnRetry != nil {
			c.Retry.OnRetry(attempt+1, wait, err)
		}
		if serr := sleep(ctx, wait); serr != nil {
			return serr
		}
	}
}

// modelRejected — тело ошибки говорит, что провайдер не знает модель или
// она не поддерживает инструменты (разные формулировки у провайдеров).
// Неподдерживаемый параметр запроса (max_tokens у reasoning-моделей
// OpenAI) — не отказ модели: его обходит клиент.
func modelRejected(status int, body string) bool {
	if status != http.StatusNotFound && status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	b := strings.ToLower(body)
	if strings.Contains(b, "unsupported_parameter") || strings.Contains(b, "unsupported parameter") {
		return false
	}
	for _, s := range []string{"does not support tools", "no endpoints found that support tool use", "not a valid model id", "is not a valid model"} {
		if strings.Contains(b, s) {
			return true
		}
	}
	// Anthropic 404 на неизвестную модель: {"type":"not_found_error",
	// "message":"model: claude-nope"}.
	if strings.Contains(b, "not_found_error") && strings.Contains(b, "model") {
		return true
	}
	if !strings.Contains(b, "model") {
		return false
	}
	for _, s := range []string{"not exist", "not found", "does not exist", "unknown model", "invalid model", "unsupported model", "model not supported", "model_not_found", "no such model"} {
		if strings.Contains(b, s) {
			return true
		}
	}
	return false
}

// moderationRefused — отказ модерации провайдера (OpenRouter 403 и т.п.):
// не ошибка ключа и не повод повторять.
func moderationRefused(status int, body string) bool {
	if status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(body)
	return strings.Contains(b, "moderation") || strings.Contains(b, "flagged")
}
