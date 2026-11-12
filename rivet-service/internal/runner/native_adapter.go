package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/runner/native"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// nativeAdapter — агентский цикл Rivet за интерфейсом adapter (design
// add-native-agent, решение 1): полная глубина, обратный канал контекста,
// внешних программ нет.
//
// Endpoint модели приходит в Assignment.endpoint: адрес, модель, окно,
// предел ответа и предел модели, цены (с ценой кешированного входа),
// лимиты и ключ по режиму секретов. Без него (ручной запуск, control plane
// старой версии) endpoint берётся из окружения runner'а и окружения
// профиля из назначения (ExtraEnv поверх него). Ключи (RIVET_NATIVE_KEY,
// RIVET_PROVIDER_KEY_*) — из секретов runner'а (Config.Secrets): в
// окружении процесса их нет.
//
//	RIVET_NATIVE_API            openai | anthropic (по умолчанию openai)
//	RIVET_NATIVE_BASE_URL       адрес API провайдера
//	RIVET_NATIVE_KEY            ключ; иначе RIVET_PROVIDER_KEY_<CONNECTION>
//	RIVET_NATIVE_CONNECTION     идентификатор подключения для ключа
//	RIVET_NATIVE_CONTEXT_WINDOW окно модели в токенах
//	RIVET_NATIVE_MAX_OUTPUT     предел ответа на ход
//	RIVET_NATIVE_MODEL_MAX_OUTPUT предельный выход модели: потолок
//	                            повтора хода с обрезанным вызовом
//	RIVET_NATIVE_PRICE_IN, _OUT, _CACHED — цены за миллион токенов, USD
//
// Модель — модель стадии (назначение или -model runner'а).
type nativeAdapter struct {
	cfg Config
}

// ErrNoProviderKey — ключ подключения не доставлен и не задан в окружении.
var ErrNoProviderKey = errors.New("нет ключа подключения")

// nativeEnv — значение переменной: окружение профиля поверх секретов и
// окружения runner'а.
func nativeEnv(extra []string, secrets map[string]string, key string) string {
	val := os.Getenv(key)
	if v, ok := secrets[key]; ok {
		val = v
	}
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return strings.TrimSpace(val)
}

// providerKeyEnv — имя переменной ключа подключения в окружении runner'а.
func providerKeyEnv(connection string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(connection) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "RIVET_PROVIDER_KEY_" + b.String()
}

type nativeEndpoint struct {
	client      llm.Client
	window      int
	maxOutput   int
	modelOutput int
	prices      *native.Prices
	// model — модель из назначения (пусто — модель стадии), limits —
	// лимиты профиля rivet (нулевые поля — по умолчанию цикла).
	model  string
	limits native.Limits
}

// endpoint — подключение модели стадии: из Assignment.endpoint (каталог
// подключений control plane), а без него — из окружения runner'а и
// секретов (e2e-стенд, ручной запуск).
func (n *nativeAdapter) endpoint() (nativeEndpoint, error) {
	if e := n.cfg.Endpoint; e != nil && e.GetBaseUrl() != "" {
		return endpointFromAssignment(e, n.cfg.Secrets)
	}
	env := func(k string) string { return nativeEnv(n.cfg.ExtraEnv, n.cfg.Secrets, k) }
	api := llm.API(env("RIVET_NATIVE_API"))
	if api == "" {
		api = llm.APIOpenAI
	}
	if api != llm.APIOpenAI && api != llm.APIAnthropic {
		return nativeEndpoint{}, fmt.Errorf("неизвестный тип API %q: ожидается openai или anthropic", api)
	}
	base := env("RIVET_NATIVE_BASE_URL")
	if base == "" {
		base = llm.DefaultBaseURL(api)
	}
	if base == "" {
		return nativeEndpoint{}, errors.New("не задан адрес API модели (RIVET_NATIVE_BASE_URL)")
	}
	key := env("RIVET_NATIVE_KEY")
	if key == "" {
		if conn := env("RIVET_NATIVE_CONNECTION"); conn != "" {
			key = env(providerKeyEnv(conn))
		}
	}
	// Локальные серверы (Ollama, LM Studio) работают без ключа: требуем его
	// только у адресов не на этой машине.
	if key == "" && !isLocalURL(base) {
		return nativeEndpoint{}, ErrNoProviderKey
	}
	ep := nativeEndpoint{
		client:      llm.Client{API: api, BaseURL: base, Key: key},
		window:      atoiEnv(env("RIVET_NATIVE_CONTEXT_WINDOW")),
		maxOutput:   atoiEnv(env("RIVET_NATIVE_MAX_OUTPUT")),
		modelOutput: atoiEnv(env("RIVET_NATIVE_MODEL_MAX_OUTPUT")),
	}
	in, out := atofEnv(env("RIVET_NATIVE_PRICE_IN")), atofEnv(env("RIVET_NATIVE_PRICE_OUT"))
	if in > 0 || out > 0 {
		ep.prices = &native.Prices{InputPerM: in, OutputPerM: out, CachedInputPerM: atofEnv(env("RIVET_NATIVE_PRICE_CACHED"))}
	}
	return ep, nil
}

// endpointFromAssignment — подключение из назначения. Ключ приходит
// только по режиму секретов; без него — ключ подключения из секретов
// runner'а (RIVET_PROVIDER_KEY_<ID>), для локальных адресов ключ не нужен.
func endpointFromAssignment(e *pb.ModelEndpoint, secrets map[string]string) (nativeEndpoint, error) {
	api := llm.API(e.GetApi())
	if api == "" {
		api = llm.APIOpenAI
	}
	if api != llm.APIOpenAI && api != llm.APIAnthropic {
		return nativeEndpoint{}, fmt.Errorf("неизвестный тип API %q: ожидается openai или anthropic", api)
	}
	key := e.GetKey()
	if key == "" && e.GetConnectionId() != "" {
		key = secrets[providerKeyEnv(e.GetConnectionId())]
	}
	if key == "" && !isLocalURL(e.GetBaseUrl()) {
		return nativeEndpoint{}, ErrNoProviderKey
	}
	ep := nativeEndpoint{
		client:      llm.Client{API: api, BaseURL: e.GetBaseUrl(), Key: key, Headers: e.GetHeaders()},
		window:      int(e.GetContextWindow()),
		maxOutput:   int(e.GetMaxOutputTokens()),
		modelOutput: int(e.GetModelMaxOutput()),
		model:       e.GetModel(),
	}
	if e.GetHasPrices() {
		ep.prices = &native.Prices{InputPerM: e.GetInputPrice(), OutputPerM: e.GetOutputPrice(), CachedInputPerM: e.GetCachedInputPrice()}
	}
	if l := e.GetLimits(); l != nil {
		ep.limits = native.Limits{MaxTurns: int(l.GetMaxTurns()), MaxTokens: l.GetMaxTokens(), MaxDuration: time.Duration(l.GetMaxMinutes()) * time.Minute}
	}
	return ep, nil
}

func isLocalURL(u string) bool { return llm.IsLocalURL(u) }

// endpointError — подключение модели стадии не собрать: нет ключа, адреса
// или неизвестный тип API. Это ошибка настройки подключения, а не работы
// агента: стадия возвращает вердикт verdictModelConnection, и control
// plane эскалирует её своим кодом (fix-agent-without-connection).
type endpointError struct{ err error }

func (e *endpointError) Error() string { return e.err.Error() }
func (e *endpointError) Unwrap() error { return e.err }

// isEndpointError — агент не запустился из-за подключения модели.
func isEndpointError(err error) bool {
	var ee *endpointError
	return errors.As(err, &ee)
}

// isConnectionError — агент не запустился из-за подключения модели или
// провайдер отклонил ключ (401/403): это ошибка настройки подключения, а не
// работы агента. Стадия возвращает вердикт verdictModelConnection.
func isConnectionError(err error) bool {
	return isEndpointError(err) || errors.Is(err, llm.ErrUnauthorized)
}

func atoiEnv(s string) int {
	n, _ := strconv.Atoi(s)
	return max(n, 0)
}

func atofEnv(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return max(f, 0)
}

func (n *nativeAdapter) Run(ctx context.Context, dir, prompt string, sink runSink) (agentRun, error) {
	ep, err := n.endpoint()
	if err != nil {
		return agentRun{}, &endpointError{err}
	}
	step := func(ev native.Event) {
		if sink.step == nil {
			return
		}
		sink.step(&pb.AgentEvent{Kind: ev.Kind, Tool: ev.Tool, Detail: ev.Detail, Files: ev.Files, Ok: ev.OK, Text: ev.Text})
	}
	ep.client.Retry = llm.RetryPolicy{OnRetry: func(attempt int, wait time.Duration, err error) {
		step(native.Event{Kind: "note", OK: true,
			Text: fmt.Sprintf("повтор запроса к провайдеру (%d) через %s: %s", attempt, wait.Round(time.Second), clipRunes(err.Error(), 200))})
	}}
	queue := sink.contexts.open(sink.session)
	defer sink.contexts.close(sink.session)
	model := n.cfg.Model
	if ep.model != "" {
		model = ep.model
	}
	// Команды агента всегда без сети: -checks-network их не касается.
	ncfg := native.Config{
		Client: ep.client, Model: model, Limits: ep.limits,
		ContextWindow: ep.window, MaxOutputTokens: ep.maxOutput, ModelMaxOutput: ep.modelOutput, Prices: ep.prices,
		Dir: dir,
		Exec: native.ExecConfig{IsolateNetwork: true, RequireSandbox: !n.cfg.Unsandboxed,
			ReadOnlyCaches: n.cfg.sandboxReadOnly(), HidePaths: n.cfg.sandboxHidePaths(), ExtraEnv: n.cfg.CommandEnv},
		OnStep: step,
		OnTranscript: func(b []byte) {
			if sink.transcript != nil {
				sink.transcript(b)
			}
		},
		Context: queue.take,
	}
	res, err := native.Run(ctx, ncfg, prompt)
	run := agentRun{FinalText: res.FinalText, Model: res.Model, Usage: nativeUsage(res.Usage)}
	if run.Model == "" {
		run.Model = model
	}
	if err != nil {
		run.isError = true
		return run, err
	}
	// Вердикт fail у реализации — неуспех запуска: стадия CODING/FIXING не
	// разбирает маркер VERDICT. Review и prompt разбирают его сами.
	if res.Verdict == "fail" && (sink.stage == pb.StageResult_CODING || sink.stage == pb.StageResult_FIXING) {
		run.isError = true
		return run, fmt.Errorf("агент сообщил, что задание выполнить нельзя: %s", clipRunes(res.Summary, 500))
	}
	return run, nil
}

// dependencyCaches — кеши зависимостей runner'а для команд агента: в
// песочнице они смонтированы только на чтение, поэтому проверки работают
// без сети на уже скачанных модулях, а агент не может подменить их.
func dependencyCaches() []native.CacheDir {
	home, _ := os.UserHomeDir()
	pick := func(env string, def ...string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		if home == "" {
			return ""
		}
		return filepath.Join(append([]string{home}, def...)...)
	}
	gomod := os.Getenv("GOMODCACHE")
	if gomod == "" {
		if gp := os.Getenv("GOPATH"); gp != "" {
			gomod = filepath.Join(strings.Split(gp, string(os.PathListSeparator))[0], "pkg", "mod")
		} else {
			gomod = pick("GOMODCACHE", "go", "pkg", "mod")
		}
	}
	return []native.CacheDir{
		{Env: "GOMODCACHE", Dir: gomod},
		{Env: "npm_config_cache", Dir: pick("npm_config_cache", ".npm")},
	}
}

// nativeUsage — расход сессии агента Rivet для pb.Usage: агентский цикл
// знает все поля, поэтому они заполнены всегда (кроме стоимости без цен и
// окна до первого хода).
func nativeUsage(u native.Usage) usageReport {
	in, out := u.InputTokens, u.OutputTokens
	cached, written := u.CachedInputTokens, u.CacheWriteTokens
	return usageReport{TokensIn: &in, TokensOut: &out, TokensCached: &cached, TokensCacheWrite: &written,
		CostUSD: u.CostUSD, CtxPct: u.CtxPct}
}
