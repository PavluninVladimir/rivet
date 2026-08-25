// Package adapter — публичный контракт адаптера агента Rivet
// (спека backend/agent-integration «Открытый SDK адаптеров»).
//
// Адаптер — обычная программа, которую runner запускает в каталоге рабочей
// копии задачи. Общение построчным JSON: на stdin приходит задание стадии
// и, если runner это объявил, контекст от Rivet; на stdout адаптер пишет
// события стадии — шаги, транскрипт, расход и итог.
//
// Контракт не зависит от языка: пакет нужен тем, кто пишет адаптер на Go,
// остальным достаточно формата строк. Неизвестные виды событий и
// нечитаемые строки runner игнорирует, поэтому адаптер новее runner'а
// стадию не ломает, а новые поля можно добавлять без версии контракта.
//
// Завершение: адаптер пишет событие result и выходит. Ждать EOF на stdin
// для выхода нельзя — runner закрывает вход как раз после итога, и
// адаптер, который ждёт EOF раньше, повиснет вместе со стадией.
//
// Минимальный адаптер на Go:
//
//	in, _ := adapter.ReadPrompt(os.Stdin)
//	out := adapter.NewWriter(os.Stdout)
//	out.Transcript("запускаю агента\n")
//	out.Step(adapter.Step{Kind: adapter.StepTool, Tool: "Bash", Detail: "make build", OK: true})
//	out.Usage(adapter.Usage{TokensIn: adapter.Int64(1200)})
//	out.Result("готово", false)
package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Виды сообщений контракта.
const (
	// На вход адаптеру.
	TypePrompt  = "prompt"
	TypeContext = "context"
	// От адаптера.
	TypeStep       = "step"
	TypeTranscript = "transcript"
	TypeUsage      = "usage"
	TypeResult     = "result"
)

// Виды шагов сессии: вызов инструмента, завершение работы, заметка.
// Пустой вид — простой текстовый шаг.
const (
	StepTool = "tool"
	StepStop = "stop"
	StepNote = "note"
)

// Prompt — задание стадии: текст промпта и её координаты.
type Prompt struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Task    string `json:"task,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Session string `json:"session,omitempty"`
}

// Context — контекст от Rivet работающему агенту (обратный канал):
// предупреждение о пересечении работ и т.п. Приходит, только если runner
// объявил поддержку канала.
type Context struct {
	Type string `json:"type"`
	Kind string `json:"kind,omitempty"`
	Text string `json:"text"`
}

// Step — шаг сессии для живого наблюдения и event log.
type Step struct {
	Type   string   `json:"type"`
	Kind   string   `json:"kind,omitempty"`
	Tool   string   `json:"tool,omitempty"`
	Detail string   `json:"detail,omitempty"`
	Files  []string `json:"files,omitempty"`
	OK     bool     `json:"ok"`
	Text   string   `json:"text,omitempty"`
}

// Transcript — кусок вывода агента.
type Transcript struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Usage — расход запуска. Отсутствующее поле означает «данных нет», а не
// ноль: занизить расход хуже, чем не сообщить его.
type Usage struct {
	Type      string   `json:"type"`
	TokensIn  *int64   `json:"tokens_in,omitempty"`
	TokensOut *int64   `json:"tokens_out,omitempty"`
	CostUSD   *float64 `json:"cost_usd,omitempty"`
	Model     string   `json:"model,omitempty"`
	CtxPct    *int32   `json:"ctx_pct,omitempty"`
}

// Result — итог запуска: текст, по которому стадия разбирает маркеры
// (BLOCKED:, VERDICT:), и признак ошибки запуска. Событие терминальное:
// после него runner закрывает вход адаптера, а адаптеру следует
// завершиться, не дожидаясь EOF на stdin.
type Result struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Error bool   `json:"error"`
}

// Int64, Float64, Int32 — короткая запись необязательных полей расхода.
func Int64(v int64) *int64       { return &v }
func Float64(v float64) *float64 { return &v }
func Int32(v int32) *int32       { return &v }

// Writer пишет события адаптера построчным JSON. Потокобезопасен:
// адаптер может писать шаги и транскрипт из разных горутин.
type Writer struct {
	mu  sync.Mutex
	out io.Writer
}

func NewWriter(out io.Writer) *Writer { return &Writer{out: out} }

func (w *Writer) emit(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = fmt.Fprintf(w.out, "%s\n", raw)
	return err
}

// Step отправляет шаг сессии.
func (w *Writer) Step(s Step) error {
	s.Type = TypeStep
	return w.emit(s)
}

// Transcript отправляет кусок вывода агента.
func (w *Writer) Transcript(text string) error {
	return w.emit(Transcript{Type: TypeTranscript, Text: text})
}

// Usage отправляет расход запуска.
func (w *Writer) Usage(u Usage) error {
	u.Type = TypeUsage
	return w.emit(u)
}

// Result отправляет итог запуска. После него адаптер обычно завершается.
func (w *Writer) Result(text string, isError bool) error {
	return w.emit(Result{Type: TypeResult, Text: text, Error: isError})
}

// Reader читает вход адаптера: задание стадии и контекст от Rivet.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader — чтение входа адаптера. Лимит строки взят с запасом под
// большой промпт; строка длиннее него даёт ошибку чтения, а не обрезается:
// половина промпта хуже явного отказа.
func NewReader(in io.Reader) *Reader {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	return &Reader{sc: sc}
}

// Next возвращает следующую строку входа: задание или контекст.
// Строки неизвестного вида и с невалидным JSON пропускаются; ошибка
// чтения (например, строка длиннее лимита) возвращается вызывающему.
// io.EOF — вход закончился.
func (r *Reader) Next() (prompt *Prompt, ctx *Context, err error) {
	for r.sc.Scan() {
		raw := r.sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		switch head.Type {
		case TypePrompt:
			var p Prompt
			if err := json.Unmarshal(raw, &p); err != nil {
				continue
			}
			return &p, nil, nil
		case TypeContext:
			var c Context
			if err := json.Unmarshal(raw, &c); err != nil {
				continue
			}
			return nil, &c, nil
		}
	}
	if err := r.sc.Err(); err != nil {
		return nil, nil, err
	}
	return nil, nil, io.EOF
}

// ReadPrompt — задание стадии: первая строка входа. Удобно адаптерам,
// которым контекст не нужен.
func ReadPrompt(in io.Reader) (Prompt, error) {
	r := NewReader(in)
	for {
		p, _, err := r.Next()
		if err != nil {
			return Prompt{}, err
		}
		if p != nil {
			return *p, nil
		}
	}
}
