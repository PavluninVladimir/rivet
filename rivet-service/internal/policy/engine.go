package policy

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/open-policy-agent/opa/v1/rego"
)

// Движок политик (change add-policy-engine, спека access-policy
// «Policy-движок с двумя режимами»): решения точек принуждения принимает
// OPA — встроенный в процесс rivetd или внешний endpoint установки.
// Правила стандартной политики живут в rego/*.rego, значения пресетов и
// факты решения приезжают в input.

//go:embed rego/*.rego
var standardPolicy embed.FS

// Точки принуждения: путь запроса к движку (data.rivet.<point>).
const (
	PointMerge    = "merge"
	PointPublish  = "publish"
	PointAssign   = "assign"
	PointMutation = "mutation"
)

// Режимы движка.
const (
	ModeEmbedded = "embedded"
	ModeExternal = "external"
)

// Decision — ответ движка. Reason пуст при разрешении; при отказе несёт
// машиночитаемую причину для события точки принуждения.
type Decision struct {
	Allow  bool
	Reason string
}

// Engine — шов точек принуждения. Ошибка означает «решение не получено»:
// вызывающий обязан трактовать её как запрет для автоматики (fail-closed).
type Engine interface {
	Decide(ctx context.Context, point string, input any) (Decision, error)
	Mode() string
	// Health — состояние движка для состояния установки: пустая ошибка
	// означает «отвечает».
	Health(ctx context.Context) error
}

// Config — настройка движка на установку.
type Config struct {
	Mode    string
	URL     string
	Timeout time.Duration
}

// ErrConfig — режим настроен неверно: установка не должна стартовать.
var ErrConfig = errors.New("настройка движка политик")

// NewEngine собирает движок по конфигурации установки.
func NewEngine(cfg Config) (Engine, error) {
	switch cfg.Mode {
	case "", ModeEmbedded:
		return newEmbedded()
	case ModeExternal:
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("%w: в режиме external нужен адрес OPA (RIVET_POLICY_URL)", ErrConfig)
		}
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		return &externalEngine{
			url:    strings.TrimRight(cfg.URL, "/"),
			client: &http.Client{Timeout: timeout},
		}, nil
	default:
		return nil, fmt.Errorf("%w: неизвестный режим %q (ожидается embedded или external)", ErrConfig, cfg.Mode)
	}
}

// Default — встроенный движок стандартной политики: значение по умолчанию
// там, где движок не задан явно (внутренние потребители и тесты; rivetd
// всегда собирает движок по конфигурации установки). Компиляция статичного
// модуля детерминирована: её отказ — сломанная сборка, а не состояние среды.
var Default = sync.OnceValue(func() Engine {
	e, err := newEmbedded()
	if err != nil {
		panic("стандартная политика не компилируется: " + err.Error())
	}
	return e
})

// ─── встроенный движок ───────────────────────────────────────────────────

type embeddedEngine struct {
	queries map[string]rego.PreparedEvalQuery
}

// newEmbedded компилирует стандартную политику один раз при старте: правила
// статичны, меняются только значения пресетов в input, поэтому кэш
// подготовленных запросов не нужно инвалидировать.
func newEmbedded() (Engine, error) {
	modules := map[string]string{}
	err := fs.WalkDir(standardPolicy, "rego", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := standardPolicy.ReadFile(path)
		if err != nil {
			return err
		}
		modules[path] = string(raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	e := &embeddedEngine{queries: map[string]rego.PreparedEvalQuery{}}
	for _, point := range []string{PointMerge, PointPublish, PointAssign, PointMutation} {
		opts := []func(*rego.Rego){rego.Query(fmt.Sprintf(
			"allow = data.rivet.%s.allow; reason = data.rivet.%s.reason", point, point))}
		for name, src := range modules {
			opts = append(opts, rego.Module(name, src))
		}
		q, err := rego.New(opts...).PrepareForEval(context.Background())
		if err != nil {
			return nil, fmt.Errorf("компиляция политики %s: %w", point, err)
		}
		e.queries[point] = q
	}
	return e, nil
}

func (e *embeddedEngine) Mode() string { return ModeEmbedded }

func (e *embeddedEngine) Health(context.Context) error { return nil }

func (e *embeddedEngine) Decide(ctx context.Context, point string, input any) (Decision, error) {
	q, ok := e.queries[point]
	if !ok {
		return Decision{}, fmt.Errorf("неизвестная точка принуждения %q", point)
	}
	// input проходит через JSON: движок работает с обычными типами, а не с
	// Go-структурами (тот же вид input, что уходит внешнему движку).
	value, err := jsonValue(input)
	if err != nil {
		return Decision{}, err
	}
	rs, err := q.Eval(ctx, rego.EvalInput(value))
	if err != nil {
		return Decision{}, fmt.Errorf("оценка политики %s: %w", point, err)
	}
	if len(rs) == 0 {
		return Decision{}, fmt.Errorf("политика %s не дала решения", point)
	}
	allow, _ := rs[0].Bindings["allow"].(bool)
	reason, _ := rs[0].Bindings["reason"].(string)
	return Decision{Allow: allow, Reason: reason}, nil
}

// jsonValue приводит input к типам, понятным движку.
func jsonValue(input any) (any, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// ─── внешний движок ──────────────────────────────────────────────────────

type externalEngine struct {
	url    string
	client *http.Client
}

func (e *externalEngine) Mode() string { return ModeExternal }

// Health — тот же путь решения, что и у обычного запроса: отдельного
// health-эндпоинта у политики нет, а «движок отвечает» проверяется именно
// оценкой правила.
func (e *externalEngine) Health(ctx context.Context) error {
	_, err := e.Decide(ctx, PointMutation, map[string]any{"action": "health", "actor": map[string]any{"kind": "user"}})
	return err
}

func (e *externalEngine) Decide(ctx context.Context, point string, input any) (Decision, error) {
	body, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		return Decision{}, err
	}
	url := e.url + "/v1/data/rivet/" + point
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("внешний движок политик: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{}, fmt.Errorf("внешний движок политик: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Decision{}, fmt.Errorf("внешний движок политик: ответ %d", resp.StatusCode)
	}
	// Пустой result в OPA означает «правило не сработало»: это не разрешение.
	var out struct {
		Result *struct {
			Allow  bool   `json:"allow"`
			Reason string `json:"reason"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
		return Decision{}, fmt.Errorf("внешний движок политик: неожиданный ответ на %s", point)
	}
	return Decision{Allow: out.Result.Allow, Reason: out.Result.Reason}, nil
}
