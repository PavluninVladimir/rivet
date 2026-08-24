package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"os"
	"os/exec"
	"strings"
	"time"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// executeStage выполняет назначенную стадию и отправляет StageResult.
func (a *agent) executeStage(ctx context.Context, as *pb.Assignment, emit func(*pb.RunnerMsg)) {
	sctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.cancel[as.TaskId] = cancel
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		delete(a.cancel, as.TaskId)
		a.mu.Unlock()
	}()

	// Новая стадия — заполненность контекста прежнего запуска неактуальна.
	a.ctxPct.Store(ctxUnknown)

	started := time.Now()
	step := func(text string) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Event{
			Event: &pb.AgentEvent{TaskId: as.TaskId, SessionId: as.SessionId, Text: text}}})
	}
	transcript := func(data []byte) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Transcript{
			Transcript: &pb.TranscriptChunk{TaskId: as.TaskId, SessionId: as.SessionId, Data: data}}})
	}
	// sink — потоки запуска агента: транскрипт и структурированные шаги
	// адаптера полной глубины (спека agent-integration «Шаги сессии»).
	sink := runSink{
		transcript: transcript,
		step: func(ev *pb.AgentEvent) {
			ev.TaskId, ev.SessionId = as.TaskId, as.SessionId
			emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Event{Event: ev}})
		},
	}
	// report — расход запуска агента этой стадии (USAGE: у обёртки, итог
	// запуска у нативного адаптера); нулевые указатели = данных нет.
	var report usageReport
	model := a.cfg.Model
	noteUsage := func(run agentRun) {
		report = run.Usage
		if run.Model != "" {
			model = run.Model
		}
		if report.CtxPct != nil {
			a.ctxPct.Store(*report.CtxPct)
		}
	}
	emitUsage := func() {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Usage{
			Usage: &pb.Usage{TaskId: as.TaskId, SessionId: as.SessionId, Model: model,
				DurationS: int32(time.Since(started).Seconds()),
				TokensIn:  report.TokensIn, TokensOut: report.TokensOut,
				CostUsd: report.CostUSD, CtxPct: report.CtxPct}}})
	}
	result := func(ok bool, detail string) {
		emitUsage()
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_StageResult{
			StageResult: &pb.StageResult{TaskId: as.TaskId, SessionId: as.SessionId,
				Stage: as.Stage, Ok: ok, Detail: tail(detail, 8000)}}})
	}

	ws, err := a.workspace(sctx, as, step)
	if err != nil {
		result(false, "workspace: "+err.Error())
		return
	}

	switch as.Stage {
	case pb.StageResult_CODING, pb.StageResult_FIXING:
		step("агент приступил к реализации")
		run, err := a.adapter.Run(sctx, ws, stagePrompt(as), sink)
		noteUsage(run)
		out := run.FinalText
		// Блокировка распознаётся только у успешного запуска: агент,
		// упавший с ошибкой, не эскалируется как «вопрос человеку».
		if q, blocked := parseBlocked(out); blocked && err == nil {
			// Расход заблокировавшегося запуска тоже учитывается: стадия
			// завершится позже, а токены уже потрачены.
			emitUsage()
			emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Blocked{
				Blocked: &pb.BlockedQuestion{TaskId: as.TaskId, SessionId: as.SessionId, Question: q}}})
			return
		}
		if err != nil {
			result(false, fmt.Sprintf("агент завершился с ошибкой: %v\n%s", err, out))
			return
		}
		step("реализация завершена — фиксация изменений")
		// push идёт в тот же репозиторий: без askpass приватный репозиторий
		// запросил бы пароль и подвис (GIT_TERMINAL_PROMPT=0 даёт ошибку).
		gitEnv, gitCleanup, err := a.gitCredentials(as)
		if err != nil {
			result(false, "git credentials: "+err.Error())
			return
		}
		defer gitCleanup()
		if err := gitCommitPush(sctx, ws, as.Branch, gitEnv,
			fmt.Sprintf("task-%d: %s (%s)", as.TaskNum, as.Title, strings.ToLower(as.Stage.String()))); err != nil {
			result(false, "git: "+err.Error())
			return
		}
		result(true, "изменения в ветке "+as.Branch)

	case pb.StageResult_TESTING:
		if len(as.Checks) == 0 {
			step("проверки проекта не настроены — этап пропущен")
			result(true, "нет настроенных проверок")
			return
		}
		var report strings.Builder
		ok := true
		for _, c := range as.Checks {
			step("проверка: " + c.Name)
			out, err := runShell(sctx, ws, c.Cmd, transcript)
			fmt.Fprintf(&report, "== %s ==\n%s\n", c.Name, tail(out, 4000))
			if err != nil {
				ok = false
				fmt.Fprintf(&report, "FAIL: %v\n", err)
			}
		}
		result(ok, report.String())

	case pb.StageResult_REVIEW:
		step("независимое review изменений")
		run, err := a.adapter.Run(sctx, ws, reviewPrompt(as), sink)
		noteUsage(run)
		if err != nil {
			result(false, fmt.Sprintf("ревьюер завершился с ошибкой: %v", err))
			return
		}
		approved, verdict := parseVerdict(run.FinalText)
		result(approved, verdict)

	default:
		result(false, "неизвестная стадия")
	}
}

// workspace готовит клон репозитория и нужную ветку. Адрес клонирования
// приходит в Assignment (репозиторий живёт у проекта); пустой repo_url —
// старое поведение по RIVET_GIT_BASE для e2e-стенда.
func (a *agent) workspace(ctx context.Context, as *pb.Assignment, step func(string)) (string, error) {
	dir := a.cfg.Workdir + "/repos/" + workspaceKey(as)
	env, cleanup, err := a.gitCredentials(as)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if _, err := os.Stat(dir + "/.git"); err != nil {
		step("клонирование " + as.Repo)
		// clone запускается напрямую, без /bin/sh: URL и путь приходят
		// снаружи, а Go-кавычки shell не экранируют.
		cmd := exec.CommandContext(ctx, "git", "clone", "--", cloneURL(a.cfg.GitBase, as), dir)
		cmd.Dir = a.cfg.Workdir
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("clone: %v: %s", err, out)
		}
	}
	base := as.BaseBranch
	if base == "" {
		base = "main"
	}
	// Имена веток уезжают в /bin/sh: %q — это кавычки Go, а не shell,
	// поэтому значения проверяются и экранируются одинарными кавычками.
	if err := validBranch(as.Branch); err != nil {
		return "", err
	}
	if err := validBranch(base); err != nil {
		return "", err
	}
	br, bs := shellQuote(as.Branch), shellQuote(base)
	var script string
	switch as.Stage {
	case pb.StageResult_CODING:
		script = fmt.Sprintf("git fetch origin && git checkout -B %s origin/%s", br, bs)
	default:
		// FIXING, TESTING, REVIEW — ветка обычно уже существует (локально
		// или на origin). Сессия доработки из blocked/failed может прийти
		// на другой runner до первого push ветки — тогда она создаётся от
		// базовой (add-user-sessions).
		script = fmt.Sprintf("git fetch origin && (git checkout %s || git checkout -b %s origin/%s || git checkout -B %s origin/%s) && (git pull --ff-only origin %s || true)",
			br, br, br, br, bs, br)
	}
	if out, err := runShellEnv(ctx, dir, script, env, nil); err != nil {
		return "", fmt.Errorf("checkout: %v: %s", err, out)
	}
	return dir, nil
}

// validBranch — имя ветки как безопасный аргумент git: без пробелов,
// подстановок и переходов на другую команду.
func validBranch(b string) error {
	if b == "" {
		return fmt.Errorf("пустое имя ветки")
	}
	if strings.HasPrefix(b, "-") {
		return fmt.Errorf("имя ветки %q начинается с дефиса", b)
	}
	for _, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("недопустимый символ %q в имени ветки %q", r, b)
		}
	}
	return nil
}

// shellQuote — значение одним аргументом /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// workspaceKey — каталог клона по полной идентичности репозитория, а не
// по одному пути: один и тот же owner/name встречается на разных
// инстансах, и общий каталог отправил бы push не туда.
func workspaceKey(as *pb.Assignment) string {
	id := as.Repo
	if as.RepoUrl != "" {
		id = strings.TrimPrefix(strings.TrimPrefix(as.RepoUrl, "https://"), "http://")
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// cloneURL — адрес клонирования: из Assignment (репозиторий проекта) или
// по RIVET_GIT_BASE, если репозиторий не передан (e2e-стенд).
func cloneURL(gitBase string, as *pb.Assignment) string {
	if as.RepoUrl != "" {
		return as.RepoUrl + ".git"
	}
	if gitBase == "" {
		gitBase = "https://github.com/"
	}
	return gitBase + as.Repo + ".git"
}

// gitCredentials готовит askpass-хелпер: токен уходит в git через
// переменные окружения, а не в аргументы команд — иначе он попал бы
// в транскрипт стадии (design, решение 8).
func (a *agent) gitCredentials(as *pb.Assignment) ([]string, func(), error) {
	if as.GitToken == "" {
		return nil, func() {}, nil
	}
	f, err := os.CreateTemp(a.cfg.Workdir, "askpass-*.sh")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	// Хелпер отвечает на запрос логина и пароля: логин — служебный,
	// пароль — токен из окружения (в файле секрета нет).
	script := "#!/bin/sh\n" +
		`case "$1" in *Username*) echo "$RIVET_GIT_USER";; *) echo "$RIVET_GIT_TOKEN";; esac` + "\n"
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		cleanup()
		return nil, func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return []string{
		"GIT_ASKPASS=" + f.Name(),
		"GIT_TERMINAL_PROMPT=0",
		"RIVET_GIT_USER=rivet",
		"RIVET_GIT_TOKEN=" + as.GitToken,
	}, cleanup, nil
}

func gitCommitPush(ctx context.Context, dir, branch string, env []string, message string) error {
	if err := validBranch(branch); err != nil {
		return err
	}
	// Identity задаём явно: на CI глобального git config нет.
	script := fmt.Sprintf(
		`git add -A && (git diff --cached --quiet || git -c user.name=rivet-runner -c user.email=runner@rivet.local commit -m %s) && git push -u origin %s`,
		shellQuote(message), shellQuote(branch))
	if out, err := runShellEnv(ctx, dir, script, env, nil); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func runShell(ctx context.Context, dir, script string, transcript func([]byte)) (string, error) {
	return runShellEnv(ctx, dir, script, nil, transcript)
}

// runShellEnv — runShell с дополнительными переменными окружения
// (askpass-хелпер git: секрет не попадает в аргументы команды).
func runShellEnv(ctx context.Context, dir, script string, env []string, transcript func([]byte)) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if transcript != nil && len(out) > 0 {
		transcript(out)
	}
	return string(out), err
}

// stagePrompt — промпт стадии реализации: промпт пользователя (сессия
// доработки, спека agent-integration «Сессия из интерфейса Rivet») с
// системным хвостом либо сгенерированный промпт задачи.
func stagePrompt(as *pb.Assignment) string {
	if as.UserPrompt == "" {
		return codingPrompt(as)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Ты работаешь над задачей task-%d в ветке %s (сессия доработки по запросу человека).\n\n", as.TaskNum, as.Branch)
	b.WriteString(as.UserPrompt)
	b.WriteString("\n\nРаботай в текущем каталоге. Не коммить и не пушь — это сделает оркестратор. " +
		"Если не можешь однозначно понять ожидаемое поведение — не гадай: выведи строку " +
		"«BLOCKED: <конкретный вопрос>» и остановись.\n")
	return b.String()
}

func codingPrompt(as *pb.Assignment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ты работаешь над задачей task-%d в ветке %s.\n\n", as.TaskNum, as.Branch)
	fmt.Fprintf(&b, "# Задача: %s\n\n%s\n\n", as.Title, as.Description)
	if len(as.Criteria) > 0 {
		b.WriteString("# Acceptance criteria\n")
		for _, c := range as.Criteria {
			b.WriteString("- " + c + "\n")
		}
		b.WriteString("\n")
	}
	if as.ExtraContext != "" {
		fmt.Fprintf(&b, "# Дополнительный контекст\n%s\n\n", as.ExtraContext)
	}
	b.WriteString("Реализуй задачу в текущем каталоге. Не коммить и не пушь — это сделает оркестратор. " +
		"Если не можешь однозначно понять ожидаемое поведение — не гадай: выведи строку " +
		"«BLOCKED: <конкретный вопрос>» и остановись.\n")
	return b.String()
}

func reviewPrompt(as *pb.Assignment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ты — независимый ревьюер задачи task-%d: %s.\n\n", as.TaskNum, as.Title)
	if len(as.Criteria) > 0 {
		b.WriteString("# Acceptance criteria\n")
		for _, c := range as.Criteria {
			b.WriteString("- " + c + "\n")
		}
		b.WriteString("\n")
	}
	if as.ExtraContext != "" {
		fmt.Fprintf(&b, "# Diff PR\n```diff\n%s\n```\n\n", as.ExtraContext)
	}
	b.WriteString("Проверь изменения в текущем каталоге (ветка " + as.Branch + ") по критериям и качеству кода. " +
		"Код не меняй. Закончи вывод РОВНО одной строкой: " +
		"«VERDICT: APPROVED» или «VERDICT: CHANGES: <список замечаний одной строкой>».\n")
	return b.String()
}

// lastSentinelLine ищет последнюю строку вывода, начинающуюся с маркера
// (строго с начала строки — упоминание маркера в середине текста не считается).
func lastSentinelLine(out, sentinel string) (rest string, found bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimRight(lines[i], "\r"))
		if strings.HasPrefix(line, sentinel) {
			return strings.TrimSpace(strings.TrimPrefix(line, sentinel)), true
		}
	}
	return "", false
}

// parseVerdict извлекает вердикт ревьюера из вывода агента.
func parseVerdict(out string) (approved bool, detail string) {
	rest, found := lastSentinelLine(out, "VERDICT:")
	if !found {
		return false, "ревьюер не вынес вердикт (нет строки VERDICT:) — считаем замечанием"
	}
	if strings.HasPrefix(rest, "APPROVED") {
		return true, "review пройден"
	}
	return false, strings.TrimSpace(strings.TrimPrefix(rest, "CHANGES:"))
}

// usageReport — машиночитаемый отчёт агента о расходе (маркер USAGE:).
// Все поля опциональны: nil = агент не сообщил значение, не ноль.
type usageReport struct {
	TokensIn  *int64   `json:"tokens_in"`
	TokensOut *int64   `json:"tokens_out"`
	CostUSD   *float64 `json:"cost_usd"`
	CtxPct    *int32   `json:"ctx_pct"`
}

// parseUsage разбирает последнюю строку «USAGE: {json}» вывода агента.
// Нет маркера или битый JSON — пустой отчёт (запись уйдёт без токенов).
func parseUsage(out string) usageReport {
	rest, found := lastSentinelLine(out, "USAGE:")
	if !found {
		return usageReport{}
	}
	var r usageReport
	if err := json.Unmarshal([]byte(rest), &r); err != nil {
		slog.Warn("битый USAGE:-отчёт агента — игнорируется", "err", err)
		return usageReport{}
	}
	if r.CtxPct != nil && (*r.CtxPct < 0 || *r.CtxPct > 100) {
		slog.Warn("ctx_pct вне диапазона 0–100 — игнорируется", "ctx_pct", *r.CtxPct)
		r.CtxPct = nil
	}
	return r
}

// parseBlocked ищет строку «BLOCKED: <вопрос>» в выводе агента.
func parseBlocked(out string) (question string, blocked bool) {
	rest, found := lastSentinelLine(out, "BLOCKED:")
	if !found || rest == "" {
		return "", false
	}
	return rest, true
}

func tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
