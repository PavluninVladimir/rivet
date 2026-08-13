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
	// report — USAGE:-отчёт запуска агента этой стадии; нулевые указатели =
	// данных нет (спека agent-integration «Отчёт usage через универсальную обёртку»).
	var report usageReport
	noteUsage := func(out string) {
		report = parseUsage(out)
		if report.CtxPct != nil {
			a.ctxPct.Store(*report.CtxPct)
		}
	}
	emitUsage := func() {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Usage{
			Usage: &pb.Usage{TaskId: as.TaskId, SessionId: as.SessionId, Model: a.cfg.Model,
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
		out, err := a.runAgent(sctx, ws, codingPrompt(as), transcript)
		noteUsage(out)
		if q, blocked := parseBlocked(out); blocked {
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
		if err := gitCommitPush(sctx, ws, as.Branch,
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
		out, err := a.runAgent(sctx, ws, reviewPrompt(as), transcript)
		noteUsage(out)
		if err != nil {
			result(false, fmt.Sprintf("ревьюер завершился с ошибкой: %v", err))
			return
		}
		approved, verdict := parseVerdict(out)
		result(approved, verdict)

	default:
		result(false, "неизвестная стадия")
	}
}

// workspace готовит клон репозитория и нужную ветку.
func (a *agent) workspace(ctx context.Context, as *pb.Assignment, step func(string)) (string, error) {
	dir := a.cfg.Workdir + "/repos/" + strings.ReplaceAll(as.Repo, "/", "__")
	if _, err := os.Stat(dir + "/.git"); err != nil {
		step("клонирование " + as.Repo)
		base := a.cfg.GitBase
		if base == "" {
			base = "https://github.com/"
		}
		if out, err := runShell(ctx, a.cfg.Workdir,
			fmt.Sprintf("git clone %s%s.git %q", base, as.Repo, dir), nil); err != nil {
			return "", fmt.Errorf("clone: %v: %s", err, out)
		}
	}
	var script string
	switch as.Stage {
	case pb.StageResult_CODING:
		script = fmt.Sprintf("git fetch origin && git checkout -B %q origin/main", as.Branch)
	default: // FIXING, TESTING, REVIEW — ветка уже существует (локально или на origin)
		script = fmt.Sprintf("git fetch origin && (git checkout %q || git checkout -b %q origin/%q) && (git pull --ff-only origin %q || true)",
			as.Branch, as.Branch, as.Branch, as.Branch)
	}
	if out, err := runShell(ctx, dir, script, nil); err != nil {
		return "", fmt.Errorf("checkout: %v: %s", err, out)
	}
	return dir, nil
}

// runAgent запускает CLI-агента в PTY; промпт кладётся во временный файл,
// путь передаётся команде через RIVET_PROMPT_FILE.
func (a *agent) runAgent(ctx context.Context, dir, prompt string, transcript func([]byte)) (string, error) {
	pf, err := os.CreateTemp(a.cfg.Workdir, "prompt-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(pf.Name())
	if _, err := pf.WriteString(prompt); err != nil {
		return "", err
	}
	_ = pf.Close()
	return runPTY(ctx, dir, a.cfg.AgentCmd, []string{"RIVET_PROMPT_FILE=" + pf.Name()}, transcript)
}

func gitCommitPush(ctx context.Context, dir, branch, message string) error {
	// Identity задаём явно: на CI глобального git config нет.
	script := fmt.Sprintf(
		`git add -A && (git diff --cached --quiet || git -c user.name=rivet-runner -c user.email=runner@rivet.local commit -m %q) && git push -u origin %q`,
		message, branch)
	if out, err := runShell(ctx, dir, script, nil); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func runShell(ctx context.Context, dir, script string, transcript func([]byte)) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if transcript != nil && len(out) > 0 {
		transcript(out)
	}
	return string(out), err
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
