package runner

import (
	"context"
	"fmt"

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

	started := time.Now()
	step := func(text string) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Event{
			Event: &pb.AgentEvent{TaskId: as.TaskId, Text: text}}})
	}
	transcript := func(data []byte) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Transcript{
			Transcript: &pb.TranscriptChunk{TaskId: as.TaskId, Data: data}}})
	}
	result := func(ok bool, detail string) {
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Usage{
			Usage: &pb.Usage{TaskId: as.TaskId, Model: a.cfg.Model,
				DurationS: int32(time.Since(started).Seconds())}}})
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_StageResult{
			StageResult: &pb.StageResult{TaskId: as.TaskId, Stage: as.Stage, Ok: ok, Detail: tail(detail, 8000)}}})
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
		if q, blocked := parseBlocked(out); blocked {
			emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Blocked{
				Blocked: &pb.BlockedQuestion{TaskId: as.TaskId, Question: q}}})
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
		if out, err := runShell(ctx, a.cfg.Workdir,
			fmt.Sprintf("git clone https://github.com/%s.git %q", as.Repo, dir), nil); err != nil {
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

// runAgent запускает CLI-агента в PTY, промпт — на stdin.
func (a *agent) runAgent(ctx context.Context, dir, prompt string, transcript func([]byte)) (string, error) {
	return runPTY(ctx, dir, a.cfg.AgentCmd, prompt, transcript)
}

func gitCommitPush(ctx context.Context, dir, branch, message string) error {
	script := fmt.Sprintf(
		`git add -A && (git diff --cached --quiet || git commit -m %q) && git push -u origin %q`,
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

// parseVerdict извлекает вердикт ревьюера из вывода агента.
func parseVerdict(out string) (approved bool, detail string) {
	idx := strings.LastIndex(out, "VERDICT:")
	if idx < 0 {
		return false, "ревьюер не вынес вердикт (нет строки VERDICT:) — считаем замечанием"
	}
	line := strings.TrimSpace(out[idx:])
	if nl := strings.IndexByte(line, '\n'); nl > 0 {
		line = line[:nl]
	}
	if strings.HasPrefix(line, "VERDICT: APPROVED") {
		return true, "review пройден"
	}
	return false, strings.TrimSpace(strings.TrimPrefix(line, "VERDICT: CHANGES:"))
}

// parseBlocked ищет сигнал «BLOCKED: <вопрос>» в выводе агента.
func parseBlocked(out string) (question string, blocked bool) {
	idx := strings.LastIndex(out, "BLOCKED:")
	if idx < 0 {
		return "", false
	}
	line := strings.TrimSpace(out[idx+len("BLOCKED:"):])
	if nl := strings.IndexByte(line, '\n'); nl > 0 {
		line = line[:nl]
	}
	if line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
