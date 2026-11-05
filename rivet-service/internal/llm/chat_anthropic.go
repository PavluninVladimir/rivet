package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Anthropic Messages с tool use (design add-native-agent, решение 3).
//   - Ответ читается потоком и собирается Accumulate: обычный запрос SDK
//     отказывает до отправки при больших max_tokens.
//   - Ответ ассистента хранится в формате API (Raw) и возвращается без
//     изменений: thinking-блоки с подписью обязаны вернуться как пришли.
//   - Результаты инструментов и пользовательский текст, идущие подряд,
//     склеиваются в одно сообщение пользователя: так требует API.
//   - Системный блок и хвост истории помечаются cache_control: 80 ходов
//     не оплачиваются по полной цене входа.

func anthropicTools(specs []ToolSpec) ([]anthropic.ToolUnionParam, error) {
	out := make([]anthropic.ToolUnionParam, 0, len(specs))
	for _, t := range specs {
		var schema struct {
			Properties json.RawMessage `json:"properties"`
			Required   []string        `json:"required"`
		}
		if len(t.Parameters) > 0 {
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return nil, fmt.Errorf("схема инструмента %s: %w", t.Name, err)
			}
		}
		props := any(map[string]any{})
		if len(schema.Properties) > 0 {
			props = schema.Properties
		}
		tp := anthropic.ToolParam{
			Name:        t.Name,
			InputSchema: anthropic.ToolInputSchemaParam{Properties: props, Required: schema.Required},
		}
		if t.Description != "" {
			tp.Description = anthropic.String(t.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tp})
	}
	return out, nil
}

func anthropicMessages(msgs []Message) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam
	var pending []anthropic.ContentBlockParamUnion
	flushUser := func() {
		if len(pending) > 0 {
			out = append(out, anthropic.NewUserMessage(pending...))
			pending = nil
		}
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			flushUser()
			if len(m.Raw) > 0 {
				var p anthropic.MessageParam
				if err := json.Unmarshal(m.Raw, &p); err != nil {
					return nil, fmt.Errorf("ответ ассистента в истории: %w", err)
				}
				out = append(out, p)
				continue
			}
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, json.RawMessage(normalizeArgs(tc.Arguments)), tc.Name))
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewTextBlock("…"))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		case RoleTool:
			// DeepSeek через anthropic-endpoint игнорирует is_error, поэтому
			// ошибку помечаем и текстом, как в OpenAI-ветке.
			content := m.Content
			if m.IsError && !strings.HasPrefix(content, "ошибка") {
				content = "ошибка: " + content
			}
			pending = append(pending, anthropic.NewToolResultBlock(m.ToolCallID, content, m.IsError))
		default:
			pending = append(pending, anthropic.NewTextBlock(m.Content))
		}
	}
	flushUser()
	return out, nil
}

// markCacheTail ставит cache_control на последний блок последнего сообщения
// пользователя: префикс истории до него кешируется провайдером.
func markCacheTail(msgs []anthropic.MessageParam) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	if last.Role != anthropic.MessageParamRoleUser || len(last.Content) == 0 {
		return
	}
	b := &last.Content[len(last.Content)-1]
	cc := anthropic.NewCacheControlEphemeralParam()
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = cc
	}
}

func (c Client) anthropicChat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	tools, err := anthropicTools(req.Tools)
	if err != nil {
		return ChatResponse{}, err
	}
	msgs, err := anthropicMessages(req.Messages)
	if err != nil {
		return ChatResponse{}, err
	}
	markCacheTail(msgs)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  msgs,
		Tools:     tools,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System, CacheControl: anthropic.NewCacheControlEphemeralParam()}}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Temperature)
	}
	if req.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(req.Effort)}
	}
	cl := c.anthropic()
	stream := cl.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var msg anthropic.Message
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return ChatResponse{Usage: anthropicUsage(msg.Usage)}, fmt.Errorf("поток ответа: %w", err)
		}
	}
	// Usage из message_start уже пришёл: оборванная попытка тоже стоит
	// денег и попадает в расход.
	if err := stream.Err(); err != nil {
		return ChatResponse{Usage: anthropicUsage(msg.Usage)}, anthropicChatErr(err)
	}
	return anthropicResponse(msg)
}

func anthropicResponse(msg anthropic.Message) (ChatResponse, error) {
	out := ChatResponse{Model: string(msg.Model), Usage: anthropicUsage(msg.Usage)}
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			out.Text += b.Text
		case anthropic.ThinkingBlock:
			out.Thinking += b.Thinking
		case anthropic.ToolUseBlock:
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: normalizeArgs(b.Input)})
		}
	}
	// Пустой StopReason — поток оборвался без message_stop (прокси,
	// балансировщик закрыли соединение): это не готовый ответ, а обрыв.
	if msg.StopReason == "" {
		return ChatResponse{Usage: out.Usage}, interruptedError("поток оборван без message_stop")
	}
	switch msg.StopReason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		out.Stop = StopEnd
	case anthropic.StopReasonToolUse:
		out.Stop = StopToolCalls
	case anthropic.StopReasonMaxTokens:
		out.Stop = StopLength
	case anthropic.StopReasonRefusal:
		out.Stop = StopRefused
	default:
		// model_context_window_exceeded и прочие нештатные причины:
		// переполнение окна отдельно, остальное — прерванная генерация.
		// Вызовы такого ответа не исполняются.
		if contextWindowExceeded(http.StatusBadRequest, string(msg.StopReason)) {
			return ChatResponse{Usage: out.Usage}, fmt.Errorf("%w: %s", ErrContextWindow, msg.StopReason)
		}
		return ChatResponse{Usage: out.Usage}, interruptedError(string(msg.StopReason))
	}
	// Пустой ответ (end_turn без блоков, бывает после результатов
	// инструментов): в историю Raw класть нельзя — Anthropic отвергает
	// сообщение ассистента с пустым content. Цикл считает это пустым ходом.
	if len(msg.Content) > 0 {
		raw, err := json.Marshal(msg.ToParam())
		if err != nil {
			return ChatResponse{}, fmt.Errorf("сохранение ответа: %w", err)
		}
		out.Raw = raw
	}
	switch {
	case out.Stop == StopRefused:
		// Отказ модели: её вызовы не исполняются.
		out.ToolCalls = nil
	case out.Stop == StopLength:
		// Входные данные tool_use при обрыве бывают валидным, но неполным
		// объектом: исполнять такой вызов нельзя.
		for i := range out.ToolCalls {
			out.ToolCalls[i].Truncated = true
		}
	case len(out.ToolCalls) > 0:
		out.Stop = StopToolCalls
	}
	return out, nil
}

// anthropicUsage — расход по usage ответа: вход складывается из трёх полей.
func anthropicUsage(u anthropic.Usage) ChatUsage {
	return ChatUsage{
		InputTokens:       u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
	}
}

// anthropicChatErr — типизированная ошибка по ответу SDK: повторы решает
// retry, SDK сам не повторяет (WithMaxRetries(0)).
func anthropicChatErr(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		// Ошибка потока без типа (SDK вернул строку): перегрузку и обрыв
		// посреди ответа стоит повторить.
		m := strings.ToLower(err.Error())
		if strings.Contains(m, "overloaded") || strings.Contains(m, "error while streaming") {
			return &APIError{Status: http.StatusServiceUnavailable, Body: err.Error()}
		}
		return err
	}
	var h http.Header
	if apiErr.Response != nil {
		h = apiErr.Response.Header
	}
	body := apiErr.RawJSON()
	if body == "" {
		body = apiErr.Error()
	}
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	status := apiErr.StatusCode
	// Ошибка посреди SSE-потока приходит со статусом исходного ответа 200
	// (или без статуса). Реальный статус берём по типу ошибки в теле, иначе
	// overloaded_error и api_error не повторялись бы.
	if status == http.StatusOK || status == 0 {
		switch anthropicStreamErrType(body) {
		case "overloaded_error":
			status = 529
		case "rate_limit_error":
			status = http.StatusTooManyRequests
		default:
			status = http.StatusInternalServerError
		}
	}
	switch {
	case moderationRefused(status, body):
		return fmt.Errorf("%w: HTTP %d: %s", ErrRefused, status, body)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: HTTP %d", ErrUnauthorized, status)
	case contextWindowExceeded(status, body):
		return fmt.Errorf("%w: HTTP %d: %s", ErrContextWindow, status, body)
	case modelRejected(status, body):
		return fmt.Errorf("%w: HTTP %d: %s", ErrModelUnsupported, status, body)
	}
	return &APIError{Status: status, Body: body, RetryAfter: retryAfterOf(h, time.Now())}
}

// anthropicStreamErrType — тип ошибки из тела события error SSE-потока
// ({"type":"error","error":{"type":"overloaded_error",...}}).
func anthropicStreamErrType(body string) string {
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	return e.Error.Type
}
