package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// executeStage выполняет назначенную стадию и отправляет StageResult.
func (a *agent) executeStage(ctx context.Context, as *pb.Assignment, emit func(*pb.RunnerMsg)) {
	sctx, cancel := context.WithCancel(ctx)
	sc := &stageCancel{cancel: cancel}
	a.mu.Lock()
	if a.cancel == nil {
		a.cancel = map[string]*stageCancel{}
	}
	a.cancel[as.TaskId] = sc
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		if a.cancel[as.TaskId] == sc {
			delete(a.cancel, as.TaskId)
		}
		a.mu.Unlock()
	}()
	emit = guardResults(sctx, sc, as, emit)

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
	stageCfg := a.cfg.forAssignment(as)
	masker := newSecretMasker(stageCfg.SecretValues, transcript)
	sink := runSink{
		transcript: masker.feed,
		session:    as.SessionId,
		stage:      as.Stage,
		contexts:   a.contexts,
		step: func(ev *pb.AgentEvent) {
			ev.TaskId, ev.SessionId = as.TaskId, as.SessionId
			ev.Text = masker.maskString(ev.Text)
			emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Event{Event: ev}})
		},
	}
	// report — расход запуска агента этой стадии (USAGE: у обёртки, итог
	// запуска у нативного адаптера); нулевые указатели = данных нет.
	var report usageReport
	// Модель сессии — из назначения (участник шага процесса), иначе модель
	// runner'а по умолчанию; адаптер стадии строится под неё.
	model := stageCfg.Model
	stageAdapter := a.adapter
	if as.Model != "" && as.Model != a.cfg.Model || len(stageCfg.ExtraEnv) > 0 || stageCfg.Endpoint != nil {
		stageAdapter = newAdapter(stageCfg)
	}
	// runAgent — запуск адаптера с дожимом маски: хвост транскрипта и
	// итоговый текст (маркеры, вопрос BLOCKED) не уносят секрет назначения.
	// Служебные файлы git снимаются до сессии и сверяются после неё: git
	// runner'а не должен исполнить конфигурацию или хук, оставленные агентом.
	runAgent := func(ws, prompt string) (agentRun, error) {
		snap, err := snapshotGit(ws)
		if err != nil {
			return agentRun{}, err
		}
		run, err := stageAdapter.Run(sctx, ws, prompt, sink)
		masker.flush()
		run.FinalText = masker.maskString(run.FinalText)
		if verr := snap.verify("агент"); verr != nil {
			return run, verr
		}
		return run, err
	}
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
				TokensCached: report.TokensCached, TokensCacheWrite: report.TokensCacheWrite,
				CostUsd: report.CostUSD, CtxPct: report.CtxPct}}})
	}
	// Вердикт шага prompt (ok | changes | fail), пусто у остальных стадий.
	promptVerdict := ""
	var criteria []*pb.CriterionResult
	result := func(ok bool, detail string) {
		emitUsage()
		// Ошибка провайдера или агента в итоге может нести эхо ключа
		// (прокси): итог маскируется, как транскрипт.
		detail = masker.maskString(detail)
		if promptVerdict == verdictModelConnection {
			// Первая строка — ошибка агента: по ней control plane читает код
			// отказа провайдера, длинный текст агента её не вытесняет.
			detail = headTail(detail, 8000)
		} else {
			detail = tail(detail, 8000)
		}
		emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_StageResult{
			StageResult: &pb.StageResult{TaskId: as.TaskId, SessionId: as.SessionId,
				StepId: as.StepId, Participant: as.Participant, Verdict: promptVerdict,
				Stage: as.Stage, Ok: ok, Detail: detail, Criteria: criteria,
				// Эхо доставленной версии: итог стадии привязан к политике,
				// по которой работал агент (спека access-policy).
				PolicyHash: as.GetPolicy().GetHash()}}})
	}

	// gitTampered — сессия изменила служебные файлы git: стадия неуспешна
	// с этой причиной, а не как обычная ошибка агента.
	gitTampered := func(err error) bool {
		var te *gitTamperError
		if errors.As(err, &te) {
			result(false, te.Error())
			return true
		}
		return false
	}

	// Runner выкатки (без агента) стадий задач не исполняет: у него нет
	// агента и песочницы, а проверки проекта на машине выкатки исполнили бы
	// код из ветки задачи рядом с окружением.
	if a.cfg.Adapter == "" {
		if as.Stage == pb.StageResult_TESTING && as.GetRole() != nil {
			promptVerdict = verdictInfraError
		}
		result(false, "runner выкатки без агента не исполняет стадии задач: назначьте стадию runner'у с агентом Rivet (-agent rivet)")
		return
	}

	ws, err := a.workspace(sctx, as, step)
	if err != nil {
		// У тестировщика это сбой инфраструктуры: попытка разработчика не
		// расходуется (add-agent-roles).
		if as.Stage == pb.StageResult_TESTING && as.GetRole() != nil {
			promptVerdict = verdictInfraError
		}
		// Хостинг отклонил clone или fetch по авторизации: control plane
		// проверит учётные данные проекта (fix-credential-state).
		if gitAuthFailed(err.Error()) {
			promptVerdict = verdictSCMAuth
		}
		result(false, "workspace: "+err.Error())
		return
	}

	switch as.Stage {
	case pb.StageResult_CODING, pb.StageResult_FIXING:
		step("агент приступил к реализации")
		run, err := runAgent(ws, stagePrompt(as))
		noteUsage(run)
		if gitTampered(err) {
			return
		}
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
			if isConnectionError(err) {
				promptVerdict = verdictModelConnection
			}
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
			if gitAuthFailed(err.Error()) {
				promptVerdict = verdictSCMAuth
			}
			result(false, "git: "+err.Error())
			return
		}
		result(true, "изменения в ветке "+as.Branch)

	case pb.StageResult_TESTING:
		if role := as.GetRole(); role != nil {
			// Push тестировщику запрещён окружением процесса агента, а не
			// конфигом репозитория: падение runner'а посреди сессии не
			// оставит блок следующим стадиям в этом каталоге.
			testerCfg := stageCfg
			testerCfg.ExtraEnv = append(slices.Clone(stageCfg.ExtraEnv),
				pushBlockEnv(append(os.Environ(), stageCfg.ExtraEnv...))...)
			// У агента Rivet команды получают окружение заново из белого
			// списка: запрет push передаётся им отдельно.
			testerCfg.CommandEnv = pushBlockEnv(nil)
			testerAdapter := newAdapter(testerCfg)
			// Сессия тестировщика под той же защитой .git, что и остальные
			// сессии агента: снимок до, сверка после.
			testerRun := func(ws, prompt string) (agentRun, error) {
				snap, err := snapshotGit(ws)
				if err != nil {
					return agentRun{}, err
				}
				run, err := testerAdapter.Run(sctx, ws, prompt, sink)
				masker.flush()
				run.FinalText = masker.maskString(run.FinalText)
				if verr := snap.verify("тестировщик"); verr != nil {
					return run, verr
				}
				return run, err
			}
			// Вывод проверок после сессии — через тот же маскер.
			checksOut := func(b []byte) { masker.feed(b) }
			a.testerStage(sctx, as, ws, role, step, checksOut, masker.flush, testerRun, noteUsage, emitUsage, emit,
				func(verdict string) { promptVerdict = verdict }, result)
			return
		}
		if len(as.Checks) == 0 {
			step("проверки проекта не настроены — этап пропущен")
			result(true, "нет настроенных проверок")
			return
		}
		// Проверки исполняют код из ветки задачи: он идёт в песочнице агента
		// и тоже не должен оставить конфигурацию или хук для git runner'а.
		snap, err := snapshotGit(ws)
		if err != nil {
			result(false, err.Error())
			return
		}
		ok, report, cerr := a.runChecks(sctx, ws, as.Checks, step, masker.feed)
		masker.flush()
		if gitTampered(snap.verify("код проверки")) {
			return
		}
		if cerr != nil {
			// Проверки не запускались, прерваны или упали из-за окружения
			// песочницы: сбой инфраструктуры, а не замечания разработчику.
			// Шаг повторяется без расхода попытки, повторный сбой эскалирует.
			promptVerdict = verdictInfraError
			result(false, strings.TrimSpace(report+"\n"+cerr.Error()))
			return
		}
		result(ok, report)

	case pb.StageResult_REVIEW:
		step("независимое review изменений")
		run, err := runAgent(ws, reviewPrompt(as))
		noteUsage(run)
		if gitTampered(err) {
			return
		}
		if err != nil {
			if isConnectionError(err) {
				promptVerdict = verdictModelConnection
			}
			result(false, fmt.Sprintf("ревьюер завершился с ошибкой: %v", err))
			return
		}
		criteria = parseCriteria(run.FinalText, len(as.Criteria))
		for _, c := range criteria {
			c.Note = masker.maskString(c.Note)
		}
		approved, verdict := parseVerdict(run.FinalText)
		result(approved, verdict)

	case pb.StageResult_PROMPT:
		// Шаг prompt (спека agent-integration «Стадия PROMPT»): задание из
		// процесса в ветке задачи, изменения коммитятся, вердикт — маркером;
		// без маркера успешный выход агента даёт ok.
		step("задание процесса: " + firstLine(as.StepPrompt))
		run, err := runAgent(ws, promptStepPrompt(as))
		noteUsage(run)
		if gitTampered(err) {
			return
		}
		out := run.FinalText
		if q, blocked := parseBlocked(out); blocked && err == nil {
			emitUsage()
			emit(&pb.RunnerMsg{MsgId: newMsgID(), Kind: &pb.RunnerMsg_Blocked{
				Blocked: &pb.BlockedQuestion{TaskId: as.TaskId, SessionId: as.SessionId, Question: q}}})
			return
		}
		if err != nil {
			if isConnectionError(err) {
				promptVerdict = verdictModelConnection
			}
			result(false, fmt.Sprintf("агент завершился с ошибкой: %v\n%s", err, out))
			return
		}
		gitEnv, gitCleanup, err := a.gitCredentials(as)
		if err != nil {
			result(false, "git credentials: "+err.Error())
			return
		}
		defer gitCleanup()
		if err := gitCommitPush(sctx, ws, as.Branch, gitEnv,
			fmt.Sprintf("task-%d: %s (%s)", as.TaskNum, as.Title, as.StepId)); err != nil {
			if gitAuthFailed(err.Error()) {
				promptVerdict = verdictSCMAuth
			}
			result(false, "git: "+err.Error())
			return
		}
		verdict, detail := parsePromptVerdict(out)
		promptVerdict = verdict
		result(verdict != "fail", detail)

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
	origin := cloneURL(a.cfg.GitBase, as)
	clone := func() error {
		step("клонирование " + as.Repo)
		// clone запускается напрямую, без /bin/sh: URL и путь приходят
		// снаружи, а Go-кавычки shell не экранируют.
		cmd := exec.CommandContext(ctx, "git", "clone", "--", origin, dir)
		cmd.Dir = a.cfg.Workdir
		cmd.Env = gitEnv(env)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("clone: %v: %s", err, out)
		}
		return nil
	}
	if _, err := os.Lstat(dir + "/.git"); err != nil {
		if err := clone(); err != nil {
			return "", err
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
	// Клон общий для стадий и задач репозитория: .git приводится к виду
	// свежего клона до fetch и до снимка (gitguard.go). Клон в неизвестном
	// состоянии удаляется и клонируется заново.
	if err := prepareWorkspaceGit(ctx, dir, origin); err != nil {
		step("рабочая копия клонируется заново: " + err.Error())
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("подготовка рабочей копии: %w", err)
		}
		if err := clone(); err != nil {
			return "", err
		}
		if err := prepareWorkspaceGit(ctx, dir, origin); err != nil {
			return "", fmt.Errorf("подготовка рабочей копии: %w", err)
		}
	}
	// Токен назначения нужен только fetch: остальная подготовка идёт без
	// учётных данных в окружении git. --prune: ветка, удалённая на
	// хостинге, не продолжается по старой ссылке.
	if out, err := runShellEnv(ctx, dir, "git fetch --prune origin", gitEnv(env), nil); err != nil {
		return "", fmt.Errorf("fetch: %v: %s", err, out)
	}
	// Незакоммиченные правки в каталоге — хвост прерванной стадии (остановка
	// или падение runner'а): в работу новой стадии они не попадают.
	// Игнорируемые файлы (зависимости, кэши) остаются: копия тёплая.
	const prep = "git reset -q --hard && git clean -qffd && "
	var script string
	switch as.Stage {
	case pb.StageResult_CODING:
		// Повтор CODING (итог прошлой попытки не дошёл до control plane, а
		// её push дошёл до origin) продолжает ветку задачи с origin: старт
		// от базовой отбросил бы доставленную работу, и push не прошёл бы.
		script = fmt.Sprintf(prep+
			"if git rev-parse -q --verify refs/remotes/origin/%s >/dev/null; then git checkout -B %s origin/%s; else git checkout -B %s origin/%s; fi",
			br, br, br, br, bs)
	default:
		// FIXING, TESTING, REVIEW — ветка обычно уже существует на origin:
		// локальная копия выравнивается на её вершину (reset, а не pull
		// --ff-only || true). Иначе после force push проверялась бы старая
		// локальная вершина, а control plane считал бы проверенной новую
		// (ревью Opus, ограничитель). Сессия доработки из blocked/failed
		// может прийти на другой runner до первого push ветки — тогда ветка
		// локальная или создаётся от базовой (add-user-sessions).
		script = fmt.Sprintf(prep+"if git rev-parse -q --verify refs/remotes/origin/%s >/dev/null; then git checkout -B %s origin/%s; "+
			"else (git checkout %s || git checkout -B %s origin/%s); fi",
			br, br, br, br, br, bs)
	}
	if out, err := runShellEnv(ctx, dir, script, gitEnv(nil), nil); err != nil {
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

// verdictSCMAuth — вердикт стадии: хостинг отклонил git-операцию runner'а
// (clone, fetch, push) по авторизации. Control plane проверяет учётные
// данные проекта и, если они не работают, откладывает запуск без расхода
// попытки (fix-credential-state).
const verdictSCMAuth = "scm_auth"

// gitAuthMarkers — признаки отказа хостинга по авторизации в выводе git
// (HTTPS с askpass): 401 и 403, отозванный или неверный токен, нет права
// на push. Сюда же «репозиторий не найден»: на приватный репозиторий без
// доступа GitHub и GitLab отвечают именно так. Ложный признак безопасен —
// состояние подключения меняет только проверка control plane.
var gitAuthMarkers = []string{
	"authentication failed",
	"could not read username",
	"could not read password",
	"returned error: 401",
	"returned error: 403",
	"returned error: 404",
	"http basic: access denied",
	"invalid username or password",
	"not allowed to push",
	"permission denied for writing",
	"repository not found",
	"could not be found or you don't have permission",
}

// gitAuthFailed — вывод git говорит об отказе хостинга по авторизации.
func gitAuthFailed(out string) bool {
	low := strings.ToLower(out)
	for _, m := range gitAuthMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	// GitHub: «remote: Permission to owner/repo.git denied to bot.»
	if i := strings.Index(low, "permission to "); i >= 0 && strings.Contains(low[i:], " denied to ") {
		return true
	}
	// git: «fatal: repository 'https://…' not found».
	if i := strings.Index(low, "fatal: repository '"); i >= 0 && strings.Contains(low[i:], "' not found") {
		return true
	}
	return false
}

func gitCommitPush(ctx context.Context, dir, branch string, env []string, message string) error {
	if err := validBranch(branch); err != nil {
		return err
	}
	// Identity задаём явно: глобальный git config runner'а не читается
	// (gitEnv). Коммит нужен и при пустом diff, если агент оставил
	// незавершённый merge (MERGE_HEAD): разрешение конфликта с базовой
	// веткой может дать дерево, равное HEAD, но без merge-коммита базовая
	// ветка не станет предком ветки задачи. Хуки клона не исполняются
	// (gitEnv и --no-verify).
	script := fmt.Sprintf(
		`git add -A && { if git rev-parse -q --verify MERGE_HEAD >/dev/null || ! git diff --cached --quiet; then `+
			`git -c user.name=rivet-runner -c user.email=runner@rivet.local commit --no-verify -m %s; fi; } && git push --no-verify -u origin %s`,
		shellQuote(message), shellQuote(branch))
	if out, err := runShellEnv(ctx, dir, script, gitEnv(env), nil); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// runShellEnv — команда runner'а с заданным окружением (git runner'а:
// gitEnv с askpass, секрет не попадает в аргументы). Без env — окружение
// runner'а без секретных переменных. Код из рабочей копии (проверки
// проекта) так не запускается: он идёт в песочнице агента (checks.go).
func runShellEnv(ctx context.Context, dir, script string, env []string, transcript func([]byte)) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = scrubEnv(os.Environ())
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
// policyPrompt — блок политики проекта для промпта стадии: единственный
// источник политики у runner'а — назначение, из рабочей копии он её не
// читает (спека access-policy «Доставка политик runner'ам»). Пустая
// политика блока не даёт: промпт остаётся прежним.
func policyPrompt(p *pb.Policy) string {
	if p == nil || p.Hash == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Политика проекта (версия %s)\n", shortHash(p.Hash))
	if dir := p.PolicyDir; dir != "" {
		fmt.Fprintf(&b, "- Файлы политики в каталоге %s менять нельзя: это делает человек отдельным решением.\n", dir)
	}
	if paths := p.HumanReviewPaths; len(paths) > 0 {
		shown := paths
		if len(shown) > policyPathsInPrompt {
			shown = shown[:policyPathsInPrompt]
		}
		fmt.Fprintf(&b, "- Правки в путях %s требуют ревью человека — без необходимости их не трогай.\n",
			strings.Join(sanitizePaths(shown), ", "))
		if len(paths) > len(shown) {
			fmt.Fprintf(&b, "- …и ещё %d защищённых путей.\n", len(paths)-len(shown))
		}
	}
	b.WriteString("\n")
	return b.String()
}

// policyPathsInPrompt — сколько защищённых путей уходит в промпт целиком:
// длинный список раздул бы контекст стадии.
const policyPathsInPrompt = 50

// sanitizePaths чистит пути перед вставкой в промпт: управляющие символы
// в шаблоне превратили бы путь в отдельную инструкцию агенту. Plane такие
// шаблоны не принимает, но промпт собирает runner — чистим на его границе.
func sanitizePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, clipRunes(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
				return ' '
			}
			return r
		}, p), 300))
	}
	return out
}

// shortHash — короткий хэш версии политики для читаемого промпта.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func stagePrompt(as *pb.Assignment) string {
	if as.UserPrompt == "" {
		return codingPrompt(as)
	}
	var b strings.Builder
	b.WriteString(rolePrompt(as.GetRole()))
	fmt.Fprintf(&b, "Ты работаешь над задачей task-%d в ветке %s (сессия доработки по запросу человека).\n\n", as.TaskNum, as.Branch)
	b.WriteString(as.UserPrompt)
	b.WriteString("\n\n")
	// Политика идёт после промпта человека: блок ограничений не должен
	// теряться выше по тексту (там же, где системные правила стадии).
	b.WriteString(policyPrompt(as.GetPolicy()))
	b.WriteString("Работай в текущем каталоге. Не коммить и не пушь — это сделает оркестратор. " +
		"Если не можешь однозначно понять ожидаемое поведение — не гадай: выведи строку " +
		"«BLOCKED: <конкретный вопрос>» и остановись.\n")
	return b.String()
}

// promptStepPrompt — промпт шага prompt: задание из процесса, контекст
// задачи, политика, правила маркеров.
func promptStepPrompt(as *pb.Assignment) string {
	var b strings.Builder
	b.WriteString(rolePrompt(as.GetRole()))
	fmt.Fprintf(&b, "Ты работаешь над задачей task-%d в ветке %s.\n\n", as.TaskNum, as.Branch)
	fmt.Fprintf(&b, "# Задание шага процесса\n\n%s\n\n", as.StepPrompt)
	fmt.Fprintf(&b, "# Контекст задачи: %s\n\n%s\n\n", as.Title, as.Description)
	if as.ExtraContext != "" {
		b.WriteString("# Дополнительный контекст\n" + as.ExtraContext + "\n\n")
	}
	b.WriteString(policyPrompt(as.GetPolicy()))
	b.WriteString("Работай в текущем каталоге. Не коммить и не пушь — это сделает оркестратор. " +
		"В конце выведи одну строку с вердиктом: «VERDICT: OK» если задание выполнено или замечаний нет, " +
		"«VERDICT: CHANGES: <что нужно исправить>» если нашёл проблемы для исправления, " +
		"«VERDICT: FAIL: <причина>» если задание выполнить нельзя. " +
		"Если не можешь однозначно понять задание — выведи «BLOCKED: <конкретный вопрос>» и остановись.\n")
	return b.String()
}

// parsePromptVerdict — вердикт шага prompt по маркеру; без маркера — ok.
func parsePromptVerdict(out string) (verdict, detail string) {
	rest, found := lastSentinelLine(out, "VERDICT:")
	if !found {
		return "ok", "задание выполнено (маркера вердикта нет)"
	}
	rest = strings.TrimSpace(rest)
	upper := strings.ToUpper(rest)
	for _, m := range []struct{ word, verdict string }{
		{"APPROVED", "ok"}, {"OK", "ok"}, {"CHANGES", "changes"}, {"FAIL", "fail"},
	} {
		// Граница слова: «OKAY» или «APPROVEDLY» маркером не считаются.
		if strings.HasPrefix(upper, m.word) && (len(upper) == len(m.word) || upper[len(m.word)] == ':' || upper[len(m.word)] == ' ') {
			return m.verdict, strings.TrimSpace(strings.TrimLeft(rest[len(m.word):], ": "))
		}
	}
	return "changes", rest
}

// firstLine — первая строка текста для шага транскрипта.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80]) + "…"
	}
	return s
}

func codingPrompt(as *pb.Assignment) string {
	var b strings.Builder
	b.WriteString(rolePrompt(as.GetRole()))
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
	b.WriteString(policyPrompt(as.GetPolicy()))
	b.WriteString("Реализуй задачу в текущем каталоге. Не коммить и не пушь — это сделает оркестратор. " +
		"Если не можешь однозначно понять ожидаемое поведение — не гадай: выведи строку " +
		"«BLOCKED: <конкретный вопрос>» и остановись.\n")
	return b.String()
}

func reviewPrompt(as *pb.Assignment) string {
	var b strings.Builder
	// С ролью кем быть говорит блок роли (add-agent-roles); без неё —
	// прежняя первая строка.
	if rp := rolePrompt(as.GetRole()); rp != "" {
		b.WriteString(rp)
		fmt.Fprintf(&b, "Review задачи task-%d: %s.\n\n", as.TaskNum, as.Title)
	} else {
		fmt.Fprintf(&b, "Ты — независимый ревьюер задачи task-%d: %s.\n\n", as.TaskNum, as.Title)
	}
	if len(as.Criteria) > 0 {
		b.WriteString("# Acceptance criteria\n")
		for i, c := range as.Criteria {
			fmt.Fprintf(&b, "%d. %s\n", i+1, c)
		}
		b.WriteString("\n")
	}
	if as.ExtraContext != "" {
		fmt.Fprintf(&b, "# Diff PR\n```diff\n%s\n```\n\n", as.ExtraContext)
	}
	b.WriteString(policyPrompt(as.GetPolicy()))
	b.WriteString("Проверь изменения в текущем каталоге (ветка " + as.Branch + ") по критериям и качеству кода. " +
		"Код не меняй. Перед вердиктом отчитайся по каждому критерию отдельной строкой с начала строки: CRITERION <номер>: OK или CRITERION <номер>: FAIL <причина>. " +
		"Закончи вывод РОВНО одной строкой: " +
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
	TokensIn  *int64 `json:"tokens_in"`
	TokensOut *int64 `json:"tokens_out"`
	// TokensCached и TokensCacheWrite — чтение из кеша провайдера и запись
	// в него, оба входят в TokensIn (pb.Usage tokens_cached и
	// tokens_cache_write).
	TokensCached     *int64   `json:"tokens_cached"`
	TokensCacheWrite *int64   `json:"tokens_cache_write"`
	CostUSD          *float64 `json:"cost_usd"`
	CtxPct           *int32   `json:"ctx_pct"`
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
	// Строковые поля protobuf обязаны быть UTF-8: невалидный байт рвёт
	// поток runner'а целиком. Хвост режется по границе руны.
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// headTail — текст не длиннее n байт с сохранённой первой строкой (не
// длиннее четверти предела): режется остальной текст, с начала, как в tail.
// Однострочный текст режется с конца: начало и есть ошибка.
func headTail(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	first, rest, ok := strings.Cut(s, "\n")
	first = headBytes(first, n/4)
	if !ok {
		return headBytes(s, n)
	}
	// tail ставит в начало «…»: место под него тоже из предела.
	return first + "\n" + tail(rest, n-len(first)-1-len("…"))
}

// headBytes — начало s не длиннее n байт по границе руны.
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// guardResults — итог стадии (StageResult, Blocked), отменённой не control
// plane'ом, а остановкой runner'а или обрывом канала, не отправляется и в
// журнал не пишется: иначе прерванная работа проигралась бы как настоящий
// провал. Сессии нет в pending_sessions, и control plane назначит стадию
// заново без расхода попытки. События, транскрипт и расход идут как прежде.
func guardResults(sctx context.Context, sc *stageCancel, as *pb.Assignment, emit func(*pb.RunnerMsg)) func(*pb.RunnerMsg) {
	return func(m *pb.RunnerMsg) {
		switch m.Kind.(type) {
		case *pb.RunnerMsg_StageResult, *pb.RunnerMsg_Blocked:
			if sctx.Err() != nil && !sc.byPlane.Load() {
				slog.Info("стадия прервана остановкой runner'а или обрывом канала: итог не отправляется, стадия будет назначена заново",
					"task", as.TaskId, "session", as.SessionId)
				return
			}
		}
		emit(m)
	}
}
