package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PavluninVladimir/rivet/internal/llm"
)

// Инструменты агента (design, решение 4): ровно восемь, пути от корня
// рабочей копии, выводы ограничены с явной пометкой об обрезке. Файловые
// инструменты — fstools.go, команды — exec_*.go.

const (
	ToolReadFile  = "read_file"
	ToolListDir   = "list_dir"
	ToolSearch    = "search"
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"
	ToolRun       = "run"
	ToolFinish    = "finish"
	ToolAskHuman  = "ask_human"
)

// ToolSpecs — описания инструментов для модели.
func ToolSpecs() []llm.ToolSpec {
	obj := func(props string, required ...string) json.RawMessage {
		// required — всегда массив: null OpenAI отвергает с 400.
		if required == nil {
			required = []string{}
		}
		req, _ := json.Marshal(required)
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":` + string(req) + `}`)
	}
	return []llm.ToolSpec{
		{Name: ToolReadFile, Description: "Прочитать текстовый файл рабочей копии. Возвращает строки с номерами. Большие файлы читай частями через offset и limit.",
			Parameters: obj(`"path":{"type":"string","description":"путь от корня рабочей копии"},"offset":{"type":"integer","description":"номер первой строки, с 1"},"limit":{"type":"integer","description":"сколько строк, до 2000"}`, "path")},
		{Name: ToolListDir, Description: "Список файлов и каталогов. Каталоги помечены «/». Каталог .git не показывается.",
			Parameters: obj(`"path":{"type":"string","description":"каталог от корня, по умолчанию корень"},"depth":{"type":"integer","description":"глубина обхода от 1 до 3"}`)},
		{Name: ToolSearch, Description: "Поиск по содержимому файлов регулярным выражением (синтаксис RE2). Возвращает «путь:строка: текст». Каталоги зависимостей и сборки пропускаются.",
			Parameters: obj(`"pattern":{"type":"string"},"path":{"type":"string","description":"каталог поиска от корня"},"glob":{"type":"string","description":"фильтр имён, например *.js или src/*.go"}`, "pattern")},
		{Name: ToolWriteFile, Description: "Записать файл целиком (создаёт каталоги). Для правки части файла используй edit_file.",
			Parameters: obj(`"path":{"type":"string"},"content":{"type":"string"}`, "path", "content")},
		{Name: ToolEditFile, Description: "Заменить фрагмент файла. Фрагмент old должен встречаться ровно один раз, иначе укажи replace_all.",
			Parameters: obj(`"path":{"type":"string"},"old":{"type":"string","description":"точный заменяемый фрагмент"},"new":{"type":"string"},"replace_all":{"type":"boolean"}`, "path", "old", "new")},
		{Name: ToolRun, Description: "Выполнить команду оболочки в корне рабочей копии (тесты, сборка, линтер). Не коммить и не пушь: это сделает Rivet.",
			Parameters: obj(`"command":{"type":"string"},"timeout_s":{"type":"integer","description":"таймаут в секундах, по умолчанию 120, не больше 600"}`, "command")},
		{Name: ToolFinish, Description: "Завершить работу. verdict: ok — задание выполнено или замечаний нет, changes — нужны исправления (опиши их в summary), fail — задание выполнить нельзя.",
			Parameters: obj(`"verdict":{"type":"string","enum":["ok","changes","fail"]},"summary":{"type":"string","description":"итог работы или замечания"}`, "verdict", "summary")},
		{Name: ToolAskHuman, Description: "Задать вопрос человеку, если без ответа работу продолжить нельзя. Завершает сессию до ответа.",
			Parameters: obj(`"question":{"type":"string"}`, "question")},
	}
}

// toolOutput — результат инструмента для модели и шага сессии.
type toolOutput struct {
	text   string
	isErr  bool
	detail string   // краткий аргумент для шага: путь, команда, шаблон
	files  []string // затронутые файлы (write_file, edit_file)
	// summary — итог для подписи шага (длительность и код команды).
	summary string
}

func errOut(detail string, err error) toolOutput {
	return toolOutput{text: "ошибка: " + err.Error(), isErr: true, detail: detail}
}

func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("аргументы не разбираются как JSON-объект: %v", err)
	}
	return nil
}

// clip обрезает строку по числу символов с многоточием.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// runTool — инструмент run (exec_*.go): командная оболочка с лимитами.
func (s *session) runTool(ctx context.Context, raw json.RawMessage) toolOutput {
	var a struct {
		Command  string `json:"command"`
		TimeoutS int    `json:"timeout_s"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return errOut("", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return errOut("", errors.New("пустая команда"))
	}
	timeout := time.Duration(a.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = s.cfg.Exec.DefaultTimeout
	}
	if timeout > s.cfg.Exec.MaxTimeout {
		timeout = s.cfg.Exec.MaxTimeout
	}
	started := time.Now()
	res := runCommand(ctx, commandSpec{
		dir: s.ws.real, command: a.Command, timeout: timeout,
		home: s.home, path: s.cfg.Exec.Path, isolate: s.cfg.Exec.IsolateNetwork,
		maxOut: s.cfg.Exec.MaxOutput, killGrace: s.cfg.Exec.KillGrace,
		readOnly: s.cfg.Exec.ReadOnlyCaches, hidePaths: s.cfg.Exec.HidePaths, extraEnv: s.cfg.Exec.ExtraEnv,
		requireSandbox: s.cfg.Exec.RequireSandbox,
	})
	elapsed := time.Since(started).Round(100 * time.Millisecond)
	detail := clip(a.Command, 200)
	if res.err != nil {
		out := errOut(detail, res.err)
		out.summary = elapsed.String()
		return out
	}
	var b strings.Builder
	summary := fmt.Sprintf("%s, код %d", elapsed, res.exitCode)
	if res.timedOut {
		fmt.Fprintf(&b, "превышен таймаут %s, процессы команды завершены\n", timeout)
		summary = fmt.Sprintf("таймаут %s", timeout)
	} else {
		fmt.Fprintf(&b, "код выхода: %d\n", res.exitCode)
	}
	fmt.Fprintf(&b, "время: %s\n", elapsed)
	if res.leftovers > 0 {
		fmt.Fprintf(&b, "фоновых процессов, оставшихся после команды и завершённых: %d\n", res.leftovers)
	}
	if res.output != "" {
		b.WriteString(res.output)
	} else {
		b.WriteString("(вывода нет)")
	}
	return toolOutput{text: b.String(), detail: detail, isErr: res.timedOut || res.exitCode != 0, summary: summary}
}
