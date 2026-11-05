package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/architecture"
)

// Сценарии агентского цикла Rivet (add-native-agent, design решение 12):
// запрос с tools получает вызовы инструментов, вычисленные по истории
// диалога, поэтому ответы детерминированы и e2e гоняет native-агента без
// модели. Роль определяется по заданию, вариант — по меткам:
//
//	разработчик: list_dir → read_file e2e-result.txt → write_file
//	             e2e-result.txt (+строка «work <номер строки>») → run → finish ok;
//	             [e2e-slow] — после записи run «sleep 6» (сессия держится
//	             открытой, обратный канал доставляет контекст следующим ходом);
//	             [e2e-block] без «Уточнение человека» — ask_human;
//	             [e2e-fail] — finish fail.
//	тестировщик: list_dir → read_file tests/e2e-tests.txt → write_file
//	             tests/e2e-tests.txt → run → finish ok;
//	             [e2e-tester-changes] — changes, пока в изменениях задачи
//	             нет строк тестов (первый проход);
//	             [e2e-tester-code] — пишет src/ (runner откатывает).
//	ревьюер:     list_dir → read_file e2e-result.txt → finish ok;
//	             [e2e-native-changes] — changes, пока в задаче меньше
//	             двух строк «work» (первый проход ревью). Описание задачи
//	             ревьюер не получает (reviewPrompt), метка нужна в заголовке.
//	шаг prompt:  list_dir → finish ok; [e2e-changes] — changes.
//	Архитектор:  задание describe ([rivet:architecture-describe],
//	             add-architecture-c4) — list_dir → write_file описания
//	             демо-CRM в docs/architecture → run "$RIVET_RUNNER_BIN"
//	             architecture views и validate → finish ok.
//
// Номер прохода считается по diff ветки из задания («# Diff PR» у
// ревьюера, «# Изменения задачи относительно …» у тестировщика), а не по
// содержимому общих файлов: в общем репозитории e2e их дописывают и
// другие тесты. Diff PR на стенде отдаёт fake SCM, и это заглушка без
// файлов задачи (scm.Fake.Diff): такой diff ревьюер не считает и берёт
// строки e2e-result.txt ветки задачи (reviewPasses). На первом ходе любой
// роли строка политики из задания повторяется текстом («Учитываю
// политику: …»): доставка политики видна в транскрипте.

type nativeMsg struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCallID       string          `json:"tool_call_id"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (m nativeMsg) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	return ""
}

type nativeAction struct {
	text string
	tool string
	args map[string]any
}

// isNativeRequest — запрос агентского цикла (есть инструменты).
func isNativeRequest(raw []byte) bool {
	var probe struct {
		Tools []json.RawMessage `json:"tools"`
	}
	return json.Unmarshal(raw, &probe) == nil && len(probe.Tools) > 0
}

// isThinkingModel — модель с включённым thinking (по имени): у неё
// reasoning_content обязателен в ответе и в последующих запросах.
func isThinkingModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "think")
}

// deepseekThinkingViolation воспроизводит требование DeepSeek V4 в режиме
// thinking: в запросе с tools у каждого сообщения ассистента с tool_calls
// должен быть reasoning_content, иначе провайдер отвечает 400. Так e2e
// ловит сводку или ход, потерявшие reasoning_content.
func deepseekThinkingViolation(raw []byte) (string, bool) {
	var in struct {
		Model    string      `json:"model"`
		Messages []nativeMsg `json:"messages"`
	}
	if json.Unmarshal(raw, &in) != nil || !isThinkingModel(in.Model) {
		return "", false
	}
	for _, m := range in.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && strings.TrimSpace(m.ReasoningContent) == "" {
			return "The reasoning_content must be fully passed back to the API in all subsequent requests when thinking is enabled", true
		}
	}
	return "", false
}

// nativeResponse — ответ chat completions для агентского цикла.
func nativeResponse(raw []byte) map[string]any {
	var in struct {
		Model    string      `json:"model"`
		Messages []nativeMsg `json:"messages"`
	}
	_ = json.Unmarshal(raw, &in)
	act := nextNativeAction(in.Messages)
	msg := map[string]any{"role": "assistant", "content": act.text}
	// Thinking-модель отдаёт reasoning_content: цикл обязан вернуть его в
	// следующем запросе (это проверяет deepseekThinkingViolation).
	if isThinkingModel(in.Model) {
		msg["reasoning_content"] = fmt.Sprintf("(fake) рассуждение хода %d", countToolTurns(in.Messages))
	}
	finish := "stop"
	if act.tool != "" {
		args, _ := json.Marshal(act.args)
		step := countToolTurns(in.Messages)
		msg["tool_calls"] = []map[string]any{{
			"id": fmt.Sprintf("call_%d", step), "type": "function",
			"function": map[string]any{"name": act.tool, "arguments": string(args)},
		}}
		finish = "tool_calls"
	}
	step := countToolTurns(in.Messages)
	return map[string]any{
		"object": "chat.completion", "model": in.Model,
		"choices": []map[string]any{{"index": 0, "finish_reason": finish, "message": msg}},
		"usage":   map[string]any{"prompt_tokens": 1200 + 300*step, "completion_tokens": 80, "prompt_cache_hit_tokens": 1000},
	}
}

// countToolTurns — сколько ходов с вызовами инструментов уже было.
func countToolTurns(msgs []nativeMsg) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			n++
		}
	}
	return n
}

// taskText — задание: пользовательские сообщения до первого ответа агента.
func taskText(msgs []nativeMsg) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role == "assistant" {
			break
		}
		if m.Role == "user" {
			b.WriteString(m.text())
			b.WriteString("\n")
		}
	}
	return b.String()
}

var numberedLine = regexp.MustCompile(`^\s*\d+\t(.*)$`)

// readResult — содержимое файла из результата read_file с этим путём
// (номера строк сняты); ошибка или пустой файл — пустая строка.
func readResult(msgs []nativeMsg, path string) string {
	ids := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "read_file" && strings.Contains(tc.Function.Arguments, `"`+path+`"`) {
				ids[tc.ID] = true
			}
		}
	}
	for _, m := range msgs {
		if m.Role != "tool" || !ids[m.ToolCallID] {
			continue
		}
		var lines []string
		for _, l := range strings.Split(m.text(), "\n") {
			if mm := numberedLine.FindStringSubmatch(l); mm != nil {
				lines = append(lines, mm[1])
			}
		}
		if len(lines) == 0 {
			return ""
		}
		return strings.Join(lines, "\n") + "\n"
	}
	return ""
}

func lineCount(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimRight(s, "\n"), "\n"))
}

// roleOf — роль по заданию: по фразам, которыми runner начинает задания
// шага prompt, тестировщика и ревьюера.
func roleOf(task string) string {
	switch {
	case strings.Contains(task, "Задание шага процесса"):
		return "prompt"
	case strings.Contains(task, "проверяешь задачу") || strings.Contains(task, "[e2e-role:tester]"):
		return "tester"
	case strings.Contains(task, "независимый ревьюер"):
		return "reviewer"
	}
	return "developer"
}

// taskDiff — diff ветки задачи из задания: «# Diff PR» (ревьюер) или
// «# Изменения задачи относительно <база>» (тестировщик).
func taskDiff(task string) (string, bool) {
	for _, h := range []string{"# Diff PR", "# Изменения задачи относительно "} {
		i := strings.Index(task, h)
		if i < 0 {
			continue
		}
		rest := task[i:]
		start := strings.Index(rest, "```diff\n")
		if start < 0 {
			continue
		}
		body := rest[start+len("```diff\n"):]
		if end := strings.Index(body, "\n```"); end >= 0 {
			body = body[:end]
		}
		return body, true
	}
	return "", false
}

// reviewPasses — сколько строк «work» сделала задача: по diff PR, если он
// описывает e2e-result.txt, иначе по файлу из ветки задачи. Fake SCM
// стенда вместо diff PR отдаёт заглушку без файлов задачи, и счёт по ней
// давал бы ноль на любом проходе. Основная ветка репозитория стенда не
// двигается (merge у fake без git-режима ничего не делает), поэтому в
// файле ветки только строки этой задачи.
func reviewPasses(task, file string) int {
	if d, ok := taskDiff(task); ok && diffTouches(d, "e2e-result.txt") {
		return addedLines(d, "work ")
	}
	return lineCount(file)
}

// diffTouches — меняет ли diff файл path (строка «+++ b/<path>»).
func diffTouches(diff, path string) bool {
	for _, l := range strings.Split(diff, "\n") {
		if l == "+++ b/"+path {
			return true
		}
	}
	return false
}

// addedLines — сколько строк diff добавлено с этим началом.
func addedLines(diff, prefix string) int {
	n := 0
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") && strings.HasPrefix(l[1:], prefix) {
			n++
		}
	}
	return n
}

// policyEcho — строка политики из задания для первого хода: заголовок
// «# Политика проекта (версия …)» и две строки за ним.
func policyEcho(task string) string {
	lines := strings.Split(task, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "# Политика проекта") {
			end := min(i+3, len(lines))
			return "Учитываю политику: " + strings.Join(lines[i:end], " ") + "\n"
		}
	}
	return ""
}

func finishAct(verdict, summary string) nativeAction {
	return nativeAction{tool: "finish", args: map[string]any{"verdict": verdict, "summary": summary}}
}

// script — действие шага step из списка; за концом списка — final.
func script(step int, acts []nativeAction, final nativeAction) nativeAction {
	if step < len(acts) {
		return acts[step]
	}
	return final
}

func nextNativeAction(msgs []nativeMsg) nativeAction {
	act := nextRoleAction(msgs)
	if countToolTurns(msgs) == 0 {
		act.text = policyEcho(taskText(msgs)) + act.text
	}
	return act
}

func nextRoleAction(msgs []nativeMsg) nativeAction {
	task := taskText(msgs)
	step := countToolTurns(msgs)
	has := func(label string) bool { return strings.Contains(task, label) }
	listDir := nativeAction{tool: "list_dir", args: map[string]any{"path": "."}}
	read := func(p string) nativeAction { return nativeAction{tool: "read_file", args: map[string]any{"path": p}} }
	run := func(c string) nativeAction { return nativeAction{tool: "run", args: map[string]any{"command": c}} }
	// appendLine — файл после read_file с новой строкой «<слово> <номер>».
	appendLine := func(p, word string) nativeAction {
		prev := readResult(msgs, p)
		return nativeAction{tool: "write_file", args: map[string]any{"path": p, "content": prev + fmt.Sprintf("%s %d\n", word, lineCount(prev)+1)}}
	}

	switch roleOf(task) {
	case "prompt":
		if step == 0 {
			return listDir
		}
		if has("[e2e-changes]") {
			return finishAct("changes", "e2e-агент просит поправить")
		}
		return finishAct("ok", "Задание шага выполнено.")

	case "reviewer":
		final := finishAct("ok", "Просмотрел изменения, замечаний нет.")
		if step >= 2 && has("[e2e-native-changes]") {
			if reviewPasses(task, readResult(msgs, "e2e-result.txt")) < 2 {
				final = finishAct("changes", "e2e-ревьюер просит поправить: нужен второй проход")
			}
		}
		return script(step, []nativeAction{listDir, read("e2e-result.txt")}, final)

	case "tester":
		target := "tests/e2e-tests.txt"
		write := appendLine(target, "test")
		if has("[e2e-tester-code]") {
			write = nativeAction{tool: "write_file", args: map[string]any{"path": "src/e2e-tester.js", "content": "// правка тестировщика вне тестов\n"}}
		}
		final := finishAct("ok", "Тесты по критериям дописаны, проверка прошла.")
		if step >= 4 && has("[e2e-tester-changes]") {
			done := lineCount(readResult(msgs, target)) >= 1
			if d, ok := taskDiff(task); ok {
				done = addedLines(d, "test ") > 0
			}
			if !done {
				final = finishAct("changes", "e2e-тестировщик: критерий не выполнен на первом проходе")
			}
		}
		return script(step, []nativeAction{listDir, read(target), write, run("test -s " + target)}, final)
	}

	// Архитектор: задание describe идёт стадией реализации, роль та же,
	// что у разработчика, задачу узнаём по маркеру.
	if has(architecture.DescribeMarker) {
		return script(step, describeActions(), finishAct("ok", "Готово: описание архитектуры в "+architecture.DefaultDir+"."))
	}
	// Разработчик.
	if has("[e2e-block]") && !has("Уточнение человека") {
		return nativeAction{text: "Не могу однозначно понять требование.", tool: "ask_human",
			args: map[string]any{"question": "вопрос от e2e-агента: подтвердите поведение"}}
	}
	if step == 1 && has("[e2e-fail]") {
		return finishAct("fail", "e2e-агент: задание выполнить нельзя")
	}
	// «Секрет» в тексте: e2e проверяет, что транскрипт его маскирует.
	intro := nativeAction{text: "Реализую задачу. export GH_TOKEN=ghp_e2eFakeSecret0123456789", tool: "list_dir", args: map[string]any{"path": "."}}
	acts := []nativeAction{intro, read("e2e-result.txt"), appendLine("e2e-result.txt", "work")}
	// Цикл сопровождения (add-sdlc-cycle): [e2e-break] ломает проверку
	// наблюдения стенда, задача-баг (её критерий — проверка наблюдения
	// окружения) чинит её. Сбой пишется словом fail: проверка — grep -qx ok.
	switch {
	case has("[e2e-break]"):
		acts = append(acts, nativeAction{tool: "write_file", args: map[string]any{"path": "health.txt", "content": "fail\n"}})
	case has("Проверка наблюдения окружения"):
		acts = append(acts, nativeAction{tool: "write_file", args: map[string]any{"path": "health.txt", "content": "ok\n"}})
	}
	if has("[e2e-slow]") {
		acts = append(acts, run("sleep 6"))
	}
	acts = append(acts, run("test -s e2e-result.txt"))
	return script(step, acts, finishAct("ok", "Готово: результат записан в e2e-result.txt."))
}

// describeFixture — описание демо-CRM для задачи описания архитектуры:
// копия internal/architecture/testdata/crm без представлений, их строит
// подкоманда runner'а, как у настоящего Архитектора. Совпадение копии с
// эталоном проверяет тест.
//
//go:embed describe
var describeFixture embed.FS

// describeActions — шаги Архитектора: осмотр, файлы описания по порядку
// путей, генерация представлений и проверка подкомандой runner'а.
func describeActions() []nativeAction {
	acts := []nativeAction{{text: "Описываю архитектуру проекта.", tool: "list_dir", args: map[string]any{"path": "."}}}
	_ = fs.WalkDir(describeFixture, "describe", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := describeFixture.ReadFile(p)
		if err != nil {
			return err
		}
		acts = append(acts, nativeAction{tool: "write_file", args: map[string]any{
			"path": architecture.DefaultDir + "/" + strings.TrimPrefix(p, "describe/"), "content": string(b)}})
		return nil
	})
	dir := architecture.DefaultDir
	return append(acts, nativeAction{tool: "run", args: map[string]any{"command": `"$RIVET_RUNNER_BIN" architecture views -dir ` + dir +
		` && "$RIVET_RUNNER_BIN" architecture validate -dir ` + dir}})
}
