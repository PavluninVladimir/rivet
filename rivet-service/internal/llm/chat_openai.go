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
)

// OpenAI-совместимый Chat Completions с function calling: DeepSeek,
// OpenRouter, Ollama, LM Studio и сам OpenAI. Расхождения провайдеров
// закрываются здесь: аргументы строкой или объектом, вызов без id,
// finish_reason «stop» при вызовах инструментов, reasoning_content,
// кешированные токены в двух форматах.

type oaiToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role             string        `json:"role"`
	Content          *string       `json:"content"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
}

func strPtr(s string) *string { return &s }

func openaiMessages(req ChatRequest) []oaiMessage {
	out := make([]oaiMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		out = append(out, oaiMessage{Role: "system", Content: strPtr(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleAssistant:
			om := oaiMessage{Role: "assistant", ReasoningContent: m.Reasoning}
			if m.Content != "" || len(m.ToolCalls) == 0 {
				om.Content = strPtr(m.Content)
			}
			for _, tc := range m.ToolCalls {
				var c oaiToolCall
				c.ID, c.Type = tc.ID, "function"
				c.Function.Name = tc.Name
				// В запросе аргументы — строка с JSON, как требует OpenAI.
				args, _ := json.Marshal(string(normalizeArgs(tc.Arguments)))
				c.Function.Arguments = args
				om.ToolCalls = append(om.ToolCalls, c)
			}
			out = append(out, om)
		case RoleTool:
			content := m.Content
			if m.IsError && !strings.HasPrefix(content, "ошибка") {
				content = "ошибка: " + content
			}
			out = append(out, oaiMessage{Role: "tool", ToolCallID: m.ToolCallID, Content: strPtr(content)})
		default:
			out = append(out, oaiMessage{Role: "user", Content: strPtr(m.Content)})
		}
	}
	return out
}

// maxTokensField — имя предела ответа: OpenAI для reasoning-моделей
// принимает только max_completion_tokens, остальные OpenAI-совместимые
// провайдеры — max_tokens.
func (c Client) maxTokensField() string {
	if strings.Contains(c.base(), "api.openai.com") {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// otherTokensField — запасное имя, если провайдер отверг основное.
func otherTokensField(f string) string {
	if f == "max_tokens" {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// tokensFieldRejected — провайдер отверг параметр предела ответа.
func tokensFieldRejected(err error, field string) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return false
	}
	b := strings.ToLower(apiErr.Body)
	return strings.Contains(b, field) && (strings.Contains(b, "unsupported") || strings.Contains(b, "not supported") || strings.Contains(b, "unrecognized"))
}

func (c Client) openaiChat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	field := c.maxTokensField()
	resp, err := c.openaiChatWith(ctx, req, field)
	if err != nil && tokensFieldRejected(err, field) {
		resp, err = c.openaiChatWith(ctx, req, otherTokensField(field))
	}
	return resp, err
}

func (c Client) openaiChatWith(ctx context.Context, req ChatRequest, tokensField string) (ChatResponse, error) {
	body := map[string]any{
		"model":     req.Model,
		"messages":  openaiMessages(req),
		tokensField: req.MaxTokens,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params,
			}})
		}
		body["tools"] = tools
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}
	resp, err := c.openaiPost(ctx, "/chat/completions", raw)
	if err != nil {
		return ChatResponse{}, err
	}
	return parseOpenAIChat(resp)
}

// openaiPost — POST с разбором кода ответа в типизированные ошибки:
// повторы решает retry по APIError.
func (c Client) openaiPost(ctx context.Context, path string, body []byte) ([]byte, error) {
	if c.base() == "" {
		return nil, errors.New("base URL не задан")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
	if resp.StatusCode == http.StatusOK {
		return raw, nil
	}
	return nil, statusError(resp.StatusCode, resp.Header, raw)
}

// statusError — типизированная ошибка по коду ответа провайдера.
func statusError(status int, h http.Header, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	switch {
	case moderationRefused(status, msg):
		return fmt.Errorf("%w: HTTP %d: %s", ErrRefused, status, msg)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return authError(status)
	case contextWindowExceeded(status, msg):
		return fmt.Errorf("%w: HTTP %d: %s", ErrContextWindow, status, msg)
	case modelRejected(status, msg):
		return fmt.Errorf("%w: HTTP %d: %s", ErrModelUnsupported, status, msg)
	}
	return &APIError{Status: status, Body: msg, RetryAfter: retryAfterOf(h, time.Now())}
}

func parseOpenAIChat(raw []byte) (ChatResponse, error) {
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []oaiToolCall   `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens         int64 `json:"prompt_tokens"`
			CompletionTokens     int64 `json:"completion_tokens"`
			PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
			PromptTokensDetails  struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string      `json:"message"`
			Code    json.Number `json:"code"`
			Type    string      `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ChatResponse{}, fmt.Errorf("невалидный ответ провайдера: %w", err)
	}
	if out.Error != nil && len(out.Choices) == 0 {
		// Часть провайдеров (OpenRouter) отвечает 200 с объектом error при
		// сбое апстрима: по коду 429/5xx это повод повторить запрос.
		msg := out.Error.Message
		if contextWindowExceeded(http.StatusBadRequest, msg) {
			return ChatResponse{}, fmt.Errorf("%w: %s", ErrContextWindow, msg)
		}
		if code, err := out.Error.Code.Int64(); err == nil && (code == http.StatusTooManyRequests || code >= 500) {
			return ChatResponse{}, &APIError{Status: int(code), Body: msg}
		}
		return ChatResponse{}, fmt.Errorf("провайдер вернул ошибку: %s", msg)
	}
	if len(out.Choices) == 0 {
		return ChatResponse{}, errors.New("пустой ответ провайдера")
	}
	ch := out.Choices[0]
	resp := ChatResponse{
		Text:      contentText(ch.Message.Content),
		Model:     out.Model,
		Reasoning: ch.Message.ReasoningContent,
		Usage: ChatUsage{
			InputTokens:       out.Usage.PromptTokens,
			OutputTokens:      out.Usage.CompletionTokens,
			CachedInputTokens: max(out.Usage.PromptCacheHitTokens, out.Usage.PromptTokensDetails.CachedTokens),
		},
	}
	for i, tc := range ch.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		resp.ToolCalls = append(resp.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Arguments: normalizeArgs(tc.Function.Arguments)})
	}
	switch ch.FinishReason {
	case "stop", "end_turn":
		resp.Stop = StopEnd
	case "tool_calls", "function_call":
		resp.Stop = StopToolCalls
	case "length", "max_tokens":
		resp.Stop = StopLength
	case "content_filter":
		resp.Stop = StopRefused
	case "":
		// Некоторые провайдеры не присылают причину при готовом ответе;
		// пустой ответ без причины — прерванная генерация.
		if resp.Text == "" && len(resp.ToolCalls) == 0 {
			return ChatResponse{Usage: resp.Usage}, interruptedError("ответ без причины остановки")
		}
		resp.Stop = StopEnd
	default:
		// Нештатная причина (insufficient_system_resource, aborted, error
		// и прочие): генерация прервана. Переполнение окна — отдельно,
		// остальное — повторяемая ошибка. Вызовы такого ответа не
		// исполняются: клиент возвращает ошибку.
		if contextWindowExceeded(http.StatusBadRequest, ch.FinishReason) {
			return ChatResponse{Usage: resp.Usage}, fmt.Errorf("%w: %s", ErrContextWindow, ch.FinishReason)
		}
		return ChatResponse{Usage: resp.Usage}, interruptedError(ch.FinishReason)
	}
	switch {
	case resp.Stop == StopRefused:
		// Отказ: вызовы инструментов такого ответа не исполняются.
		resp.ToolCalls = nil
	case resp.Stop == StopLength:
		for i := range resp.ToolCalls {
			resp.ToolCalls[i].Truncated = true
		}
	case len(resp.ToolCalls) > 0:
		// Часть провайдеров (Ollama) отвечает «stop» при вызовах.
		resp.Stop = StopToolCalls
	}
	return resp, nil
}

// contentText — текст ответа: строка, null или массив частей.
func contentText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}
