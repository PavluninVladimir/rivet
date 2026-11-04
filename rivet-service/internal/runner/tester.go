package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PavluninVladimir/rivet/internal/policy"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Роли агентов на runner'е (add-agent-roles, спеки agent-roles «Инструкция
// роли в назначении» и process «Шаг test с тестировщиком»): блок роли перед
// заданием стадии и сессия тестировщика на TESTING. Границу роли
// тестировщика держит код: правки вне тестовых путей откатываются, а
// проверки проекта остаются гейтом независимо от вердикта модели.

// testerDiffCap — сколько diff'а задачи уходит тестировщику в задание.
const testerDiffCap = 60000

// roleLine — значение роли одной строкой: управляющие символы в названии или
// имени превратили бы их в отдельную инструкцию.
func roleLine(s string, max int) string {
	return clipRunes(strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)), max)
}

// roleLabel — «Роль Имя» исполнителя для шагов транскрипта.
func roleLabel(r *pb.AgentRole) string {
	label := roleLine(r.GetTitle(), 40)
	if name := roleLine(r.GetName(), 40); name != "" {
		label += " " + name
	}
	return label
}

// rolePrompt — блок роли перед заданием стадии; без роли пусто, и задание
// остаётся прежним.
func rolePrompt(r *pb.AgentRole) string {
	if r == nil || strings.TrimSpace(r.GetTitle()) == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Роль\nТвоя роль в команде Rivet: %s.\n", roleLabel(r))
	if inst := strings.TrimSpace(r.GetInstruction()); inst != "" {
		b.WriteString(inst + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// testerPrompt — задание тестировщику: задача, критерии, diff ветки
// относительно базовой, проверки проекта и границы правок.
func testerPrompt(as *pb.Assignment, diff string) string {
	var b strings.Builder
	b.WriteString(rolePrompt(as.GetRole()))
	fmt.Fprintf(&b, "Ты проверяешь задачу task-%d в ветке %s по критериям приёмки.\n\n", as.TaskNum, as.Branch)
	fmt.Fprintf(&b, "# Задача: %s\n\n%s\n\n", as.Title, as.Description)
	if len(as.Criteria) > 0 {
		b.WriteString("# Acceptance criteria\n")
		for _, c := range as.Criteria {
			b.WriteString("- " + c + "\n")
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(diff) != "" {
		base := as.BaseBranch
		if base == "" {
			base = "main"
		}
		fmt.Fprintf(&b, "# Изменения задачи относительно %s\n```diff\n%s\n```\n\n", base, diff)
	}
	if len(as.Checks) > 0 {
		b.WriteString("# Проверки проекта\n")
		for _, c := range as.Checks {
			fmt.Fprintf(&b, "- %s: `%s`\n", roleLine(c.Name, 80), roleLine(c.Cmd, 300))
		}
		b.WriteString("\n")
	}
	b.WriteString(policyPrompt(as.GetPolicy()))
	paths := as.TestPaths
	if len(paths) == 0 {
		paths = policy.DefaultTestPaths
	}
	fmt.Fprintf(&b, "Напиши или дополни автоматические тесты так, чтобы каждый критерий приёмки был проверен, "+
		"и запусти проверки проекта. Меняй только тестовые файлы (%s): правки вне них будут откатаны. "+
		"Продуктовый код не трогай. Не коммить и не пушь, это сделает оркестратор. "+
		"В конце выведи одну строку с вердиктом: «VERDICT: OK», если все критерии выполнены и тесты проходят, "+
		"или «VERDICT: CHANGES: <какой критерий не выполнен и как это воспроизвести>». "+
		"Если не можешь однозначно понять ожидаемое поведение, выведи «BLOCKED: <конкретный вопрос>» и остановись.\n",
		strings.Join(sanitizePaths(paths), ", "))
	return b.String()
}

// gitLines — вывод git построчно без пустых строк.
func gitLines(ctx context.Context, dir string, args ...string) ([]string, error) {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	// core.quotePath=false: пути с кириллицей приходят как есть, а не
	// восьмеричными escape-последовательностями в кавычках.
	out, err := runShell(ctx, dir, "git -c core.quotePath=false "+strings.Join(quoted, " "), nil)
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, out)
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// changedSince — файлы, изменённые после коммита head: новые коммиты,
// индекс, рабочее дерево и неотслеживаемые файлы. Переименование даёт оба
// пути, удаление — удалённый путь.
func changedSince(ctx context.Context, dir, head string) ([]string, error) {
	diff, err := gitLines(ctx, dir, "diff", "--name-only", "--no-renames", head)
	if err != nil {
		return nil, err
	}
	untracked, err := gitLines(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range append(diff, untracked...) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}

// outsideTests — файлы вне тестовых путей проекта.
func outsideTests(files, patterns []string) []string {
	if len(patterns) == 0 {
		patterns = policy.DefaultTestPaths
	}
	var out []string
	for _, f := range files {
		if !policy.MatchAny(patterns, f) {
			out = append(out, f)
		}
	}
	return out
}

// resetTo откатывает рабочую копию к коммиту: правки сессии не попадают в
// ветку (новые коммиты агента тоже, их ещё не пушили). Подмодули
// приводятся к коммиту только уже инициализированные и с учётными данными
// проекта; их сбой не валит откат: приватный подмодуль без доступа не
// должен ломать тестировщика во всех задачах проекта.
func resetTo(ctx context.Context, dir, head string, env []string) error {
	script := "git reset -q --hard " + shellQuote(head) + " && git clean -qffd"
	if out, err := runShell(ctx, dir, script, nil); err != nil {
		return fmt.Errorf("откат правок тестировщика: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitmodules")); err == nil {
		sub := "git submodule foreach -q --recursive 'git reset -q --hard && git clean -qffd' && git submodule update -q --recursive --force"
		if out, err := runShellEnv(ctx, dir, sub, env, nil); err != nil {
			slog.Warn("тестировщик: подмодули не приведены к коммиту", "dir", dir, "err", err, "out", tail(out, 500))
		}
	}
	return nil
}

// pushBlockedURL — недействительный адрес push сессии тестировщика.
const pushBlockedURL = "rivet-tester-no-push://blocked"

// pushBlockEnv — окружение процесса агента-тестировщика, в котором push в
// origin не проходит: pushurl задаётся через GIT_CONFIG_* (git 2.31+) только
// этому процессу и его потомкам. Конфиг репозитория не меняется, поэтому
// завершение runner'а посреди сессии не оставляет блок следующим стадиям.
// base — окружение процесса: уже заданные GIT_CONFIG_* сохраняются, ключ
// добавляется следующим номером.
func pushBlockEnv(base []string) []string {
	n := 0
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c >= 0 {
				n = c
			}
		}
	}
	i := strconv.Itoa(n)
	return []string{
		"GIT_CONFIG_KEY_" + i + "=remote.origin.pushurl",
		"GIT_CONFIG_VALUE_" + i + "=" + pushBlockedURL,
		"GIT_CONFIG_COUNT=" + strconv.Itoa(n+1),
	}
}

// gitEnvConfig — поддерживает ли git на runner'е конфиг из окружения
// (GIT_CONFIG_COUNT, git 2.31+): без него push тестировщику не запретить.
var gitEnvConfig struct {
	once sync.Once
	err  error
}

func checkGitEnvConfig() error {
	gitEnvConfig.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", "version").Output()
		if err != nil {
			gitEnvConfig.err = fmt.Errorf("git version: %v", err)
			return
		}
		gitEnvConfig.err = gitVersionAtLeast(string(out), 2, 31)
	})
	return gitEnvConfig.err
}

// gitVersionAtLeast — вывод `git version` не старше major.minor.
func gitVersionAtLeast(out string, major, minor int) error {
	f := strings.Fields(out)
	ver := ""
	if len(f) >= 3 {
		ver = f[2]
	}
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) >= 2 {
		ma, err1 := strconv.Atoi(parts[0])
		mi, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil {
			if ma > major || ma == major && mi >= minor {
				return nil
			}
			return fmt.Errorf("на runner'е git %s: тестировщику нужен git %d.%d или новее (запрет push через GIT_CONFIG_COUNT), обновите git", ver, major, minor)
		}
	}
	return fmt.Errorf("не разобрать версию git %q: тестировщику нужен git %d.%d или новее", strings.TrimSpace(out), major, minor)
}

// ownPush — сдвиг ветки на origin сделан из этой рабочей копии: коммит есть
// среди её объектов (без fetch чужой коммит здесь отсутствует) и потомок
// коммита до сессии.
func ownPush(ctx context.Context, dir, origin, remote string) bool {
	if remote == "" {
		return false
	}
	if _, err := runShell(ctx, dir, "git cat-file -e "+shellQuote(remote+"^{commit}"), nil); err != nil {
		return false
	}
	_, err := runShell(ctx, dir, "git merge-base --is-ancestor "+shellQuote(origin)+" "+shellQuote(remote), nil)
	return err == nil
}

// branchDiff — diff ветки задачи относительно базовой (после fetch в
// workspace), с ограничением объёма.
func branchDiff(ctx context.Context, dir, base string) string {
	if base == "" {
		base = "main"
	}
	if validBranch(base) != nil {
		return ""
	}
	out, err := runShell(ctx, dir, "git diff "+shellQuote("origin/"+base)+"...HEAD", nil)
	if err != nil {
		return ""
	}
	if len(out) > testerDiffCap {
		out = out[:testerDiffCap] + "\n…[diff обрезан]\n"
	}
	return out
}

// runChecks прогоняет проверки проекта: отчёт и общий успех.
func runChecks(ctx context.Context, dir string, checks []*pb.Check, step func(string), transcript func([]byte)) (bool, string) {
	if len(checks) == 0 {
		return true, "нет настроенных проверок"
	}
	var report strings.Builder
	ok := true
	for _, c := range checks {
		step("проверка: " + c.Name)
		out, err := runShell(ctx, dir, c.Cmd, transcript)
		fmt.Fprintf(&report, "== %s ==\n%s\n", c.Name, tail(out, 4000))
		if err != nil {
			ok = false
			fmt.Fprintf(&report, "FAIL: %v\n", err)
		}
	}
	return ok, report.String()
}

// testerOutcome — итог шага test с тестировщиком: явный вердикт и отчёт с
// авторами блоков. Красные проверки перекрывают «OK» модели.
func testerOutcome(r *pb.AgentRole, verdict, notes string, checksOK bool, checks string) (ok bool, explicit, detail string) {
	if verdict == "fail" {
		verdict = "changes"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", roleLabel(r))
	switch {
	case verdict == "ok" && strings.TrimSpace(notes) != "":
		b.WriteString("критерии проверены: " + notes + "\n")
	case verdict == "ok":
		b.WriteString("критерии проверены\n")
	default:
		b.WriteString(notes + "\n")
	}
	b.WriteString("\n## Проверки\n" + checks)
	return verdict == "ok" && checksOK, verdict, b.String()
}

// testerStage — стадия TESTING с ролью (add-agent-roles, спека process «Шаг
// test с тестировщиком»). Работа идёт от состояния ветки на origin: хвосты
// прошлых сессий в push не попадают. Всё, что сессия не закоммитила по
// правилам, откатывается на любом выходе (вопрос человеку, отмена, сбой);
// самовольный push тестировщика отменяется на origin. Сбой инфраструктуры
// (git, push, падение агента) — вердикт error, а не замечания разработчику.
func (a *agent) testerStage(sctx context.Context, as *pb.Assignment, ws string, role *pb.AgentRole,
	step func(string), transcript func([]byte),
	runAgent func(ws, prompt string) (agentRun, error), noteUsage func(agentRun), emitUsage func(),
	emit func(*pb.RunnerMsg), setVerdict func(string), result func(bool, string)) {
	step(roleLabel(role) + " пишет тесты по критериям приёмки")
	gitEnv, gitCleanup, err := a.gitCredentials(as)
	if err != nil {
		setVerdict(verdictInfraError)
		result(false, "git credentials: "+err.Error())
		return
	}
	defer gitCleanup()
	origin, err := originBranchSHA(sctx, ws, as.Branch)
	if err != nil {
		setVerdict(verdictInfraError)
		result(false, "тестировщик: ветка на origin: "+err.Error())
		return
	}
	// Откат — до отправки результата: после него в тот же каталог может
	// прийти следующее назначение. Отложенный вызов — страховка на панику.
	committed, settled := false, false
	settle := func() {
		if settled {
			return
		}
		settled = true
		if committed {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(sctx), settleTimeout)
		defer cancel()
		if err := resetTo(cctx, ws, origin, gitEnv); err != nil {
			slog.Warn("тестировщик: откат рабочей копии", "task", as.TaskId, "err", err)
		}
	}
	defer settle()
	finish := func(ok bool, detail string) {
		settle()
		result(ok, detail)
	}
	infra := func(msg string) {
		setVerdict(verdictInfraError)
		finish(false, msg)
	}
	if err := resetTo(sctx, ws, origin, gitEnv); err != nil {
		infra(err.Error())
		return
	}
	// Push агенту запрещён окружением его процесса (runAgent тестировщика).
	if err := checkGitEnvConfig(); err != nil {
		infra(err.Error())
		return
	}
	run, runErr := runAgent(ws, testerPrompt(as, branchDiff(sctx, ws, as.BaseBranch)))
	noteUsage(run)
	out := run.FinalText
	if q, blocked := parseBlocked(out); blocked && runErr == nil {
		settle()
		emitUsage()
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Blocked{
			Blocked: &pb.BlockedQuestion{TaskId: as.TaskId, SessionId: as.SessionId, Question: q}}})
		return
	}
	if runErr != nil {
		infra(fmt.Sprintf("тестировщик завершился с ошибкой: %v", runErr))
		return
	}
	// Ветка на origin после сессии: свой push тестировщика (в обход блока)
	// отменяется; чужой сдвиг (человек, параллельный участник) не трогается,
	// шаг повторится от нового состояния ветки.
	remote, err := remoteBranchSHA(sctx, ws, gitEnv, as.Branch)
	if err != nil {
		infra("тестировщик: ветка на origin после сессии: " + err.Error())
		return
	}
	selfPushed := false
	if remote != origin {
		if !ownPush(sctx, ws, origin, remote) {
			infra("ветка задачи изменилась на origin во время сессии тестировщика: шаг повторится от нового состояния")
			return
		}
		if err := restoreRemote(sctx, ws, gitEnv, as.Branch, remote, origin); err != nil {
			infra("тестировщик сам отправил коммиты в ветку, вернуть её не удалось: " + err.Error())
			return
		}
		selfPushed = true
		step("push тестировщика в ветку отменён")
	}
	verdict, notes := parsePromptVerdict(out)
	changed, err := changedSince(sctx, ws, origin)
	if err != nil {
		infra("тестировщик: " + err.Error())
		return
	}
	outside := outsideTests(changed, as.TestPaths)
	switch {
	case selfPushed || len(outside) > 0:
		// Граница роли держится кодом: правки сессии в ветку не попадают.
		if err := resetTo(sctx, ws, origin, gitEnv); err != nil {
			infra(err.Error())
			return
		}
		verdict = "changes"
		var reasons []string
		if selfPushed {
			reasons = append(reasons, "тестировщик сам отправил коммиты в ветку, они отменены")
		}
		if len(outside) > 0 {
			reasons = append(reasons, "тестировщик изменил файлы вне тестов, его правки откатаны: "+strings.Join(outside, ", "))
		}
		notes = strings.Join(reasons, "\n") + "\n" + strings.TrimSpace(notes)
		step("правки тестировщика вне тестов откатаны")
	case len(changed) > 0:
		step("тесты готовы — фиксация изменений")
		if err := gitCommitPush(sctx, ws, as.Branch, gitEnv,
			fmt.Sprintf("task-%d: тесты (%s)", as.TaskNum, roleLabel(role))); err != nil {
			infra("git: " + err.Error())
			return
		}
		committed = true
	default:
		committed = true
	}
	checksOK, report := runChecks(sctx, ws, as.Checks, step, transcript)
	ok, explicit, detail := testerOutcome(role, verdict, notes, checksOK, report)
	setVerdict(explicit)
	finish(ok, detail)
}

// settleTimeout — откат рабочей копии тестировщика на любом завершении.
// Меньше stopGrace: остановка runner'а успевает дождаться отката.
const settleTimeout = 20 * time.Second

// verdictInfraError — вердикт стадии тестировщика при сбое инфраструктуры.
const verdictInfraError = "error"

// originBranchSHA — коммит ветки задачи на origin после fetch в workspace.
func originBranchSHA(ctx context.Context, dir, branch string) (string, error) {
	if err := validBranch(branch); err != nil {
		return "", err
	}
	lines, err := gitLines(ctx, dir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("нет ветки %s на origin", branch)
	}
	return lines[0], nil
}

// remoteBranchSHA — текущий коммит ветки на origin (ls-remote); пусто —
// ветки нет.
func remoteBranchSHA(ctx context.Context, dir string, env []string, branch string) (string, error) {
	out, err := runShellEnv(ctx, dir, "git ls-remote origin "+shellQuote("refs/heads/"+branch), env, nil)
	if err != nil {
		return "", fmt.Errorf("ls-remote: %v: %s", err, out)
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0], nil
	}
	return "", nil
}

// restoreRemote возвращает ветку на origin к коммиту want, если там всё
// ещё got (force-with-lease: чужой push после тестировщика не затирается).
func restoreRemote(ctx context.Context, dir string, env []string, branch, got, want string) error {
	ref := "refs/heads/" + branch
	script := "git push -q --force-with-lease=" + shellQuote(ref+":"+got) + " origin " + shellQuote(want+":"+ref)
	if out, err := runShellEnv(ctx, dir, script, env, nil); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
