package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Диалог с инструментами (change add-native-agent, design решение 3):
// агентский цикл Rivet ведёт с моделью историю сообщений и получает вызовы
// инструментов. Один формат запроса и ответа для обоих типов API:
// OpenAI-совместимый Chat Completions с function calling и Anthropic
// Messages с tool use.

// Роли сообщений диалога.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	// RoleTool — результат вызова инструмента (ToolCallID обязателен).
	RoleTool = "tool"
)

// ToolSpec — описание инструмента для модели: имя, назначение и JSON Schema
// аргументов (объект).
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall — вызов инструмента моделью. Arguments — JSON-объект аргументов
// (провайдеры отдают его строкой или объектом, клиент приводит к объекту,
// а невалидный JSON оставляет как есть: решать вызывающему).
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
	// Truncated — ответ оборван лимитом длины, аргументы могут быть
	// неполными даже при валидном JSON: такой вызов не исполняется.
	Truncated bool
}

// Usable — вызов можно исполнять: не обрезан и аргументы — JSON-объект.
func (c ToolCall) Usable() bool { return !c.Truncated && ValidArgs(c.Arguments) }

// Message — сообщение истории.
type Message struct {
	Role    string
	Content string
	// ToolCalls — вызовы инструментов в ответе ассистента.
	ToolCalls []ToolCall
	// ToolCallID и ToolName — у результата инструмента.
	ToolCallID string
	ToolName   string
	// IsError — результат инструмента описывает ошибку.
	IsError bool
	// Reasoning — рассуждение модели из ответа OpenAI-совместимого
	// провайдера (reasoning_content): часть провайдеров требует вернуть его
	// в следующем запросе того же хода. Отправляется, только если пришло.
	Reasoning string
	// Raw — ответ ассистента в формате провайдера (Anthropic: блоки с
	// thinking и подписью), возвращается в следующем запросе без изменений.
	// Пусто — сообщение строится из Content и ToolCalls.
	Raw json.RawMessage
}

// StopReason — почему модель остановилась.
type StopReason string

const (
	StopEnd       StopReason = "stop"
	StopToolCalls StopReason = "tool_calls"
	StopLength    StopReason = "length"
	// StopRefused — отказ модели или фильтр контента: вызовы инструментов
	// такого ответа отброшены.
	StopRefused StopReason = "refused"
	StopOther   StopReason = "other"
)

// ErrRefused — провайдер отказал по правилам модерации.
var ErrRefused = errors.New("провайдер отказал в запросе по правилам модерации")

// ErrContextWindow — провайдер сообщил, что запрос не помещается в окно
// контекста модели (finish_reason model_context_window_exceeded,
// «prompt is too long», «maximum context length»). Повтор не поможет:
// стадия завершается, цикл сжимает историю или признаёт переполнение.
var ErrContextWindow = errors.New("запрос не помещается в окно контекста модели")

// ChatUsage — расход одного запроса по ответу провайдера.
type ChatUsage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	// CacheWriteTokens — токены, записанные в кеш провайдера (Anthropic
	// cache_creation_input_tokens): стоят дороже обычного входа.
	CacheWriteTokens int64
}

// ChatRequest — запрос диалога.
type ChatRequest struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature *float64
	// Effort — уровень усилия модели Anthropic (low…max); пусто — по
	// умолчанию модели.
	Effort string
}

// ChatResponse — ответ модели.
type ChatResponse struct {
	Text      string
	ToolCalls []ToolCall
	Stop      StopReason
	// Usage — расход попытки, чей ответ возвращён (успешной). По её входу
	// агентский цикл оценивает заполненность окна контекста.
	Usage ChatUsage
	// Retried — расход неудачных попыток этого вызова (оборванный поток,
	// прерванная генерация): провайдер берёт за них деньги, но размер
	// контекста по ним не оценивается. Если вызов завершился ошибкой, весь
	// его расход лежит здесь, а Usage пуст.
	Retried ChatUsage
	// Model — модель из ответа провайдера (пусто, если не отдал).
	Model string
	// Reasoning — рассуждение (reasoning_content), если провайдер его отдал.
	Reasoning string
	// Thinking — текст thinking-блоков Anthropic (для транскрипта).
	Thinking string
	// Raw — ответ в формате провайдера для точного повтора в истории.
	Raw json.RawMessage
}

// Chat — один ход диалога с повторами на перегрузку и сбои сети
// (Client.Retry).
func (c Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if req.Model == "" {
		return ChatResponse{}, fmt.Errorf("не указана модель")
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 8192
	}
	var call func(context.Context, ChatRequest) (ChatResponse, error)
	switch c.API {
	case APIAnthropic:
		call = c.anthropicChat
	case APIOpenAI:
		call = c.openaiChat
	default:
		return ChatResponse{}, fmt.Errorf("неизвестный тип API %q", c.API)
	}
	// Провайдер берёт деньги и за попытку, оборванную посреди потока или
	// прерванную, поэтому usage неудачных попыток не теряется, а копится в
	// Retried (с ошибкой вызова тоже). В Usage остаётся только успешная
	// попытка: сумма попыток завысила бы оценку контекста цикла.
	var resp ChatResponse
	var retried ChatUsage
	err := c.retry(ctx, func() error {
		r, err := call(ctx, req)
		if err != nil {
			retried = retried.add(r.Usage)
			return err
		}
		resp = r
		return nil
	})
	if err != nil {
		return ChatResponse{Retried: retried}, err
	}
	resp.Retried = retried
	return resp, nil
}

// Billed — весь расход вызова к оплате: успешная попытка и неудачные.
func (r ChatResponse) Billed() ChatUsage { return r.Usage.add(r.Retried) }

func (u ChatUsage) add(o ChatUsage) ChatUsage {
	return ChatUsage{
		InputTokens:       u.InputTokens + o.InputTokens,
		OutputTokens:      u.OutputTokens + o.OutputTokens,
		CachedInputTokens: u.CachedInputTokens + o.CachedInputTokens,
		CacheWriteTokens:  u.CacheWriteTokens + o.CacheWriteTokens,
	}
}

// normalizeArgs приводит аргументы вызова к JSON-объекту: строка с JSON
// разворачивается, пустое значение становится «{}». Невалидный JSON
// возвращается как есть: цикл ответит модели ошибкой инструмента.
func normalizeArgs(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return json.RawMessage("{}")
	}
	if strings.HasPrefix(s, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			inner = strings.TrimSpace(inner)
			if inner == "" {
				return json.RawMessage("{}")
			}
			return json.RawMessage(inner)
		}
	}
	return json.RawMessage(s)
}

// ValidArgs — аргументы вызова являются JSON-объектом (обрезанный по длине
// вызов им не является).
func ValidArgs(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil
}
