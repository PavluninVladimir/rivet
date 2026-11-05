package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Архитектура проекта (add-architecture-c4, спека backend/project-architecture):
// снимок описания с основной ветки читает control plane через API хостинга
// по SHA, обновление идёт по событиям с одиночным исполнением на проект.
// Задачу описания выполняет агент команды роли architect во внутреннем
// процессе describe → check → review → merge.

// ArchitectureFSLimits — лимиты чтения описания через API хостинга;
// переменная, чтобы тест мог проверить превышение.
var ArchitectureFSLimits = scm.DefaultFSLimits

// Шаги внутреннего процесса задачи описания.
const (
	archStepDescribe = "describe"
	archStepCheck    = "check"
	archStepReview   = "review"
	archStepMerge    = "merge"
)

// architectureProcess — внутренний процесс задачи описания: процесс
// проекта к ней не применяется (пользовательские шаги и проверки проекта
// для документации бессмысленны). Описание пишет Архитектор шагом code:
// замечания проверки и review возвращаются ему как исправление.
func architectureProcess(presets policy.Presets) policy.Resolved {
	attempts := presets.AttemptLimit
	if attempts < 1 {
		attempts = 1
	}
	review := presets.ReviewLimit
	if review < 1 {
		review = 1
	}
	return policy.Resolved{Steps: []policy.ResolvedStep{
		{
			ID: archStepDescribe, Kind: policy.StepCode, Title: "Описание архитектуры",
			// coding: runner ревьюера тоже объявляет architecture, но
			// описание пишется в рабочей копии, как код.
			Capabilities: []string{"coding", "architecture"},
			Participants: []policy.ResolvedParticipant{{ID: "p1", Agent: policy.AgentRef{Role: "architect"}}},
			Mode:         policy.ModeParallel, Require: policy.RequireAll, Attempts: attempts,
			On: policy.Transitions{Ok: archStepCheck, Changes: archStepDescribe, Fail: policy.TargetEscalate},
		},
		{
			ID: archStepCheck, Kind: policy.StepTest, Title: "Проверка архитектуры",
			Capabilities: []string{"architecture"},
			Participants: []policy.ResolvedParticipant{{ID: "p1", Agent: policy.AgentRef{}}},
			Mode:         policy.ModeParallel, Require: policy.RequireAll, Attempts: attempts,
			On: policy.Transitions{Ok: archStepReview, Changes: archStepDescribe, Fail: policy.TargetEscalate},
		},
		{
			ID: archStepReview, Kind: policy.StepReview, Title: "Review описания",
			// Спека: все шаги задачи описания идут runner'ам с поддержкой
			// архитектуры. Runner'ы этой версии объявляют её сами.
			Capabilities: []string{"review", "architecture"},
			Participants: []policy.ResolvedParticipant{{ID: "p1", Agent: policy.AgentRef{Role: "reviewer"}}},
			Mode:         policy.ModeParallel, Require: policy.RequireAll, Attempts: review,
			On: policy.Transitions{Ok: archStepMerge, Changes: archStepDescribe, Fail: policy.TargetEscalate},
		},
		{
			ID: archStepMerge, Kind: policy.StepMerge, Title: "Merge",
			Participants: []policy.ResolvedParticipant{},
			Mode:         policy.ModeParallel, Require: policy.RequireAll, Attempts: 1,
			On: policy.Transitions{Ok: policy.TargetDone},
		},
	}}
}

// architectureGateCheck — проверка «Архитектура» шага test: подкоманда
// rivet-runner, которую runner экспортирует в RIVET_RUNNER_BIN. Для задачи
// описания она обязательна (-force) и не пускает правки вне описания
// (-only-docs): такие задачи идут только runner'ам с capability
// architecture. Для остальных задач она срабатывает, только если ветка
// меняет описание. Runner без подкоманды пропускает ветку без изменений
// описания, а ветку с изменениями проваливает с явной причиной (спека
// «Проверка архитектуры на шаге test»).
//
// origin — адрес репозитория из конфигурации проекта, тот же, что runner
// клонирует: гейт забирает базу с него, а не с remote из .git/config
// рабочей копии (ревью Opus, круг 3). Пусто — адрес неизвестен (fake).
func architectureGateCheck(base string, describe bool, origin string) *pb.Check {
	if base == "" {
		base = "main"
	}
	ref := shellQuote("origin/" + base)
	gate := `architecture gate -base ` + ref
	if origin != "" {
		gate += ` -origin ` + shellQuote(origin)
	}
	if describe {
		return &pb.Check{Name: "Архитектура", Cmd: `"${RIVET_RUNNER_BIN:?нужен rivet-runner с поддержкой проверки архитектуры}" ` +
			gate + ` -force -only-docs`}
	}
	dir := shellQuote(architecture.DefaultDir)
	cmd := `if [ -n "${RIVET_RUNNER_BIN:-}" ]; then "$RIVET_RUNNER_BIN" ` + gate + `; ` +
		`elif ! mb=$(git merge-base ` + ref + ` HEAD); then ` +
		`echo "проверка архитектуры: нет общего предка ветки с "` + ref + ` >&2; exit 1; ` +
		`elif git diff --no-renames --quiet "$mb" -- ` + dir + ` && ` +
		`[ -z "$(git ls-files --others --exclude-standard -- ` + dir + `)" ]; then ` +
		`echo "описание архитектуры в ветке не менялось: проверка не нужна"; ` +
		`else echo "ветка меняет описание архитектуры, а runner не умеет его проверять: обновите rivet-runner" >&2; exit 1; fi`
	return &pb.Check{Name: "Архитектура", Cmd: cmd}
}

// architectureWaitReason — причина ожидания шага, которому нужна
// capability architecture. Подсказка «обновите rivet-runner» верна, только
// если ни один зарегистрированный runner capability не объявляет: runner'ы
// этой версии объявляют её сами, и тогда настоящая причина другая (нет
// ревьюера, модели, свободного агента роли) и остаётся как есть.
func (e *Engine) architectureWaitReason(ctx context.Context, caps []string, fallback string) string {
	for _, c := range caps {
		if c != "architecture" {
			continue
		}
		if found, err := e.St.AnyRunnerWithCapability(ctx, "architecture"); err == nil && !found {
			return "нет runner'а с поддержкой архитектуры (capability architecture): обновите rivet-runner"
		}
		break
	}
	return fallback
}

// archState — одиночное исполнение обновления снимка на проект: повод,
// пришедший во время обновления, помечает проект грязным, и обновление
// повторяется один раз. trigger — повод автоописания, который проверяется
// после записи снимка (connect важнее epic_start).
type archState struct {
	mu      sync.Mutex
	running map[string]bool
	dirty   map[string]bool
	force   map[string]bool
	trigger map[string]string
	wg      sync.WaitGroup
	// sem — общий потолок одновременных обновлений снимков: сотни проектов
	// на одном токене не должны бить в API хостинга разом (secondary rate
	// limit GitHub на параллельные запросы).
	sem chan struct{}
	// reverify — задачи описания с идущей фоновой сверкой файлов
	// смерженного PR: сверка одна на задачу.
	reverify map[string]bool
}

// archConcurrency — одновременных обновлений снимков на установку.
const archConcurrency = 4

// archInflight — фоновые обновления снимков всех движков процесса: тесты
// пакета ждут их до удаления своей базы.
var archInflight sync.WaitGroup

func (e *Engine) arch() *archState {
	e.archOnce.Do(func() {
		e.archSt = &archState{running: map[string]bool{}, dirty: map[string]bool{},
			force: map[string]bool{}, trigger: map[string]string{}, sem: make(chan struct{}, archConcurrency),
			reverify: map[string]bool{}}
	})
	return e.archSt
}

// RefreshArchitecture ставит обновление снимка проекта в фон. force —
// перечитать даже при неизменном SHA (ручной запрос).
func (e *Engine) RefreshArchitecture(projectID string, force bool) {
	e.refreshInBackground(projectID, force, "")
}

// ConnectArchitecture — первый снимок после подключения репозитория:
// без описания он даёт повод «подключение» автоописанию.
func (e *Engine) ConnectArchitecture(projectID string) {
	e.refreshInBackground(projectID, true, triggerConnect)
}

// ConnectNewRepoArchitecture — первый снимок нового репозитория, который
// создал Rivet: в нём только стартовый коммит хостинга (README), описывать
// нечего, поэтому повод «подключение» пропускается как пустой репозиторий.
func (e *Engine) ConnectNewRepoArchitecture(projectID string) {
	e.refreshInBackground(projectID, true, triggerNewRepo)
}

// Поводы автоописания: «подключение репозитория» и внутренний повод
// подключения нового репозитория, созданного Rivet.
const (
	triggerConnect = "connect"
	triggerNewRepo = "connect_new_repo"
)

func (e *Engine) refreshInBackground(projectID string, force bool, trigger string) {
	a := e.arch()
	a.mu.Lock()
	if force {
		a.force[projectID] = true
	}
	// Поводы подключения важнее запуска Epic.
	if cur := a.trigger[projectID]; trigger != "" && cur != triggerConnect && cur != triggerNewRepo {
		a.trigger[projectID] = trigger
	}
	if a.running[projectID] {
		a.dirty[projectID] = true
		a.mu.Unlock()
		return
	}
	a.running[projectID] = true
	a.wg.Add(1)
	archInflight.Add(1)
	a.mu.Unlock()
	go func() {
		defer archInflight.Done()
		defer a.wg.Done()
		for {
			a.mu.Lock()
			f, trig := a.force[projectID], a.trigger[projectID]
			delete(a.force, projectID)
			delete(a.trigger, projectID)
			delete(a.dirty, projectID)
			a.mu.Unlock()
			a.sem <- struct{}{}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := e.refreshArchitecture(ctx, projectID, f, trig); err != nil {
				slog.Error("обновление снимка архитектуры", "project", projectID, "err", err)
			}
			cancel()
			<-a.sem
			a.mu.Lock()
			if !a.dirty[projectID] {
				delete(a.running, projectID)
				a.mu.Unlock()
				return
			}
			a.mu.Unlock()
		}
	}()
}

// ResumeArchitecture после рестарта сверяет SHA основной ветки у проектов,
// чей снимок уже строился: очередь обновлений живёт в памяти и при
// рестарте теряется. Проекты без снимка не читаются (design, план
// миграции: массового чтения при старте нет). Заодно поднимаются фоновые
// сверки файлов PR задач описания, смерженных на хостинге (ревью Opus,
// ограничитель webhook'ов).
func (e *Engine) ResumeArchitecture(ctx context.Context) {
	e.resumeExternalReverify(ctx)
	ids, err := e.St.ProjectsWithArchitecture(ctx)
	if err != nil {
		slog.Error("сверка снимков архитектуры при старте", "err", err)
		return
	}
	// Разброс старта: обновления встают в очередь постепенно, а не сотней
	// горутин разом (потолок archConcurrency держит и сам запуск).
	for i, id := range ids {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(resumeSpread):
			}
		}
		e.RefreshArchitecture(id, false)
	}
}

// resumeSpread — пауза между проектами при сверке снимков на старте.
var resumeSpread = 200 * time.Millisecond

// WaitArchitecture ждёт фоновых обновлений снимков (тесты, остановка).
func (e *Engine) WaitArchitecture() { e.arch().wg.Wait() }

// RefreshArchitectureNow читает описание с основной ветки и записывает
// снимок. Сбой чтения пишется статусом error и ошибкой не считается:
// обновление не влияет на задачу, которая его вызвала.
func (e *Engine) RefreshArchitectureNow(ctx context.Context, projectID string, force bool) error {
	return e.refreshArchitecture(ctx, projectID, force, "")
}

// ConnectArchitectureNow — синхронный вариант ConnectArchitecture.
func (e *Engine) ConnectArchitectureNow(ctx context.Context, projectID string) error {
	return e.refreshArchitecture(ctx, projectID, true, triggerConnect)
}

// noCredentialsText — причина, по которой снимок проекта без собственных
// учётных данных не строится: глобальный токен установки не должен
// открывать содержимое чужого репозитория через API (ревью Opus, круг 1).
const noCredentialsText = "у проекта нет собственных учётных данных хостинга: снимок архитектуры не строится, подключите токен проекта"

// forceCooldown — ручное обновление при неизменном SHA не перечитывает
// описание чаще этого интервала (лимиты API хостинга).
const forceCooldown = 30 * time.Second

func (e *Engine) refreshArchitecture(ctx context.Context, projectID string, force bool, trigger string) error {
	p, err := e.St.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if p.RepoPath == "" {
		return nil
	}
	prev, existed, err := e.St.GetProjectArchitecture(ctx, projectID)
	if err != nil {
		return err
	}
	// Проект на глобальном токене (устаревшая форма подключения): описание
	// не читается и повода автоописания нет. Fake-установка (стенды)
	// внешних хостингов не имеет, ей можно.
	if p.CredentialID == "" && !e.Adapters.Force {
		if !existed || prev.Status != "error" || prev.ErrorKind != store.ArchErrNoCredentials {
			if err := e.saveArchitecture(ctx, projectID, prev, existed, store.ArchitectureSave{
				Status: "error", SHA: prev.SHA, Error: noCredentialsText, ErrorKind: store.ArchErrNoCredentials}); err != nil {
				return err
			}
		}
		e.applyTrigger(ctx, projectID, trigger, "error", false)
		return nil
	}
	branch := p.DefaultBranch
	if branch == "" {
		branch = e.BaseBranch
	}
	save := store.ArchitectureSave{}
	emptyRepo := false
	adapter, aerr := e.SCMFor(ctx, p)
	sha := ""
	if aerr == nil {
		sha, aerr = adapter.HeadSHA(ctx, p.RepoPath, branch)
	}
	switch {
	case errors.Is(aerr, scm.ErrBranchNotFound):
		emptyRepo = true
		save = store.ArchitectureSave{Status: architecture.SnapshotAbsent}
	case aerr != nil:
		save = store.ArchitectureSave{Status: "error", SHA: prev.SHA, Error: hostErrorPrefix + aerr.Error(), ErrorKind: store.ArchErrHost}
	case existed && prev.SHA == sha && sameSnapshot(prev, force):
		// SHA не изменился: перечитывать нечего. Повод автоописания при
		// этом не теряется.
		e.applyTrigger(ctx, projectID, trigger, prev.Status, false)
		return nil
	default:
		// Ветка ушла вперёд, а дерево описания то же (merge без правок
		// docs/architecture): один запрос вместо полного чтения.
		tree := ""
		if th, ok := adapter.(scm.TreeHasher); ok {
			if t, err := th.PathSHA(ctx, p.RepoPath, sha, architecture.DefaultDir); err == nil {
				tree = t
			}
		}
		if !force && existed && tree != "" && tree == prev.TreeSHA && prev.ErrorKind != store.ArchErrHost {
			if err := e.St.TouchArchitectureSHA(ctx, projectID, sha); err != nil {
				return err
			}
			e.applyTrigger(ctx, projectID, trigger, prev.Status, false)
			return nil
		}
		save = buildArchitectureSnapshot(ctx, adapter, p, sha)
		save.TreeSHA = tree
	}
	if err := e.saveArchitecture(ctx, projectID, prev, existed, save); err != nil {
		return err
	}
	// Повод автоописания проверяется после записи снимка (спека
	// «Автоописание архитектуры»).
	e.applyTrigger(ctx, projectID, trigger, save.Status, emptyRepo)
	return nil
}

// hostErrorPrefix — причина статуса error при сбое хостинга.
const hostErrorPrefix = "хостинг недоступен: "

// sameSnapshot — снимок того же SHA перечитывать не нужно. Перечитывается
// только сбой хостинга: остальные ошибки (лимит, неразобранное или не
// записанное описание) на том же SHA повторятся. Принудительное чтение
// (ручной запрос, подключение, замена учётных данных) идёт всегда: паузу
// между ручными запросами решает API (RefreshWait), иначе 202 мог бы
// остаться без события, если другое чтение успело раньше (ревью Opus,
// круг 3).
func sameSnapshot(prev store.ArchitectureRow, force bool) bool {
	if prev.Status == "error" && prev.ErrorKind == store.ArchErrHost {
		return false
	}
	return !force
}

// RefreshWait — сколько ждать до ручного перечитывания того же SHA. Ноль —
// чтение можно запускать (снимка нет, сбой хостинга, пауза прошла или
// SHA ветки изменился). Решение API на POST .../refresh: 202 только когда
// чтение действительно пойдёт (решения по контракту после ревью консоли).
func (e *Engine) RefreshWait(ctx context.Context, projectID string) (time.Duration, error) {
	prev, existed, err := e.St.GetProjectArchitecture(ctx, projectID)
	if err != nil || !existed || prev.ReadAt == nil {
		return 0, err
	}
	if prev.Status == "error" && prev.ErrorKind == store.ArchErrHost {
		return 0, nil
	}
	left := forceCooldown - time.Since(*prev.ReadAt)
	if left <= 0 {
		return 0, nil
	}
	p, err := e.St.GetProject(ctx, projectID)
	if err != nil {
		return 0, err
	}
	adapter, err := e.SCMFor(ctx, p)
	if err != nil {
		return 0, nil
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sha, err := adapter.HeadSHA(hctx, p.RepoPath, firstNonEmpty(p.DefaultBranch, e.BaseBranch))
	if err != nil || sha != prev.SHA {
		return 0, nil
	}
	return left, nil
}

// applyTrigger — повод автоописания после снимка. «Подключение» на пустой
// ветке или к новому репозиторию, созданному Rivet, пропускается: задача
// ставится при запуске первого Epic. Остальные поводы проверяет
// MaybeAutoDescribe: она ставит задачу или пишет пропуск с причиной
// (описание есть, снимок не прочитан, пресет выключен). Обычное обновление
// поводом не считается, поэтому первый снимок проекта, подключённого до
// обновления установки, задачу не ставит: у него остаётся запуск Epic.
func (e *Engine) applyTrigger(ctx context.Context, projectID, trigger, status string, emptyRepo bool) {
	if trigger == "" {
		return
	}
	switch {
	case trigger == triggerNewRepo && status == architecture.SnapshotAbsent:
		e.skipAutoDescribe(ctx, projectID, triggerConnect, "empty_repo")
	case emptyRepo && trigger == triggerConnect:
		e.skipAutoDescribe(ctx, projectID, trigger, "empty_repo")
	default:
		if trigger == triggerNewRepo {
			trigger = triggerConnect
		}
		if _, _, err := e.MaybeAutoDescribe(ctx, projectID, trigger); err != nil {
			slog.Error("автоописание архитектуры", "project", projectID, "err", err)
		}
	}
}

// saveArchitecture записывает снимок и публикует project.architecture_updated.
// Если база не приняла данные (например, нулевой символ в тексте ADR:
// jsonb его не хранит), пишется статус error с причиной, а не прежний
// снимок без изменений.
func (e *Engine) saveArchitecture(ctx context.Context, projectID string, prev store.ArchitectureRow, existed bool, save store.ArchitectureSave) error {
	prevSHA, _, err := e.St.SaveProjectArchitecture(ctx, projectID, save)
	if err != nil && save.Status != "error" {
		slog.Error("запись снимка архитектуры", "project", projectID, "err", err)
		save = store.ArchitectureSave{Status: "error", SHA: save.SHA, Error: "снимок не записан: " + err.Error(),
			ErrorKind: store.ArchErrData, TreeSHA: save.TreeSHA}
		prevSHA, _, err = e.St.SaveProjectArchitecture(ctx, projectID, save)
	}
	if err != nil {
		return err
	}
	var prevAny any
	if existed && prevSHA != "" {
		prevAny = prevSHA
	}
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "project.architecture_updated", ProjectID: projectID,
		Text:    "снимок архитектуры: " + architectureStatusText(save.Status),
		Payload: map[string]any{"project_id": projectID, "sha": save.SHA, "status": save.Status, "previous_sha": prevAny},
	}); err != nil {
		slog.Error("событие снимка архитектуры", "project", projectID, "err", err)
	}
	return nil
}

// buildArchitectureSnapshot читает описание по SHA без листинга и строит
// снимок кодом internal/architecture.
func buildArchitectureSnapshot(ctx context.Context, adapter scm.Adapter, p domain.Project, sha string) store.ArchitectureSave {
	fsys := scm.NewRepoFS(ctx, adapter, p.RepoPath, sha, architecture.DefaultDir, ArchitectureFSLimits)
	snap, err := architecture.BuildSnapshot(fsys, architecture.SnapshotOptions{Dir: architecture.DefaultDir})
	if ferr := fsys.Err(); ferr != nil {
		// Сбой чтения через API — хостинг, его стоит повторить; лимит нет.
		kind := store.ArchErrHost
		if errors.Is(ferr, scm.ErrFSLimit) {
			kind = store.ArchErrLimit
		}
		return store.ArchitectureSave{Status: "error", SHA: sha, Error: readFailText(ferr), ErrorKind: kind}
	}
	if err != nil {
		kind := store.ArchErrData
		if errors.Is(err, architecture.ErrLimit) {
			kind = store.ArchErrLimit
		}
		return store.ArchitectureSave{Status: "error", SHA: sha, Error: readFailText(err), ErrorKind: kind}
	}
	// Символические ссылки в каталоге описания прочитаны как отсутствующие
	// файлы: содержимое цели в снимок не попадает, а описание с ними
	// считается некорректным (ревью Opus, круг 3).
	if links := fsys.Symlinks(); len(links) > 0 && snap.Status != architecture.SnapshotAbsent {
		for _, l := range links {
			snap.Defects = append(snap.Defects, architecture.Defect{
				Severity: architecture.SeverityError, Rule: "file.symlink", File: l,
				Message: "символическая ссылка в каталоге описания не читается: замените её файлом",
			})
		}
		snap.Status, snap.Data = architecture.SnapshotInvalid, nil
	}
	defects, _ := json.Marshal(snap.Defects)
	out := store.ArchitectureSave{Status: snap.Status, SHA: sha, Defects: defects}
	if snap.Status == architecture.SnapshotOK && snap.Data != nil {
		raw, err := json.Marshal(snap.Data)
		if err != nil {
			return store.ArchitectureSave{Status: "error", SHA: sha, Error: "снимок не сериализуется: " + err.Error(), ErrorKind: store.ArchErrData}
		}
		out.Data = raw
	}
	return out
}

func readFailText(err error) string {
	switch {
	case errors.Is(err, scm.ErrFSLimit), errors.Is(err, architecture.ErrLimit):
		return "описание слишком большое для чтения через API хостинга: " + err.Error()
	default:
		return "описание не прочитано: " + err.Error()
	}
}

func architectureStatusText(s string) string {
	switch s {
	case "ok":
		return "описание корректно"
	case "invalid":
		return "в описании есть ошибки"
	case "absent":
		return "описания нет"
	default:
		return "описание не прочитано"
	}
}

// DescribeArchitecture ставит задачу описания архитектуры: системный Epic
// «Архитектура проекта» с одной задачей вида architecture. Вторая
// незавершённая задача отклоняется store.ErrArchitectureTaskActive.
func (e *Engine) DescribeArchitecture(ctx context.Context, projectID, trigger, userLogin string) (epicID, taskID string, err error) {
	return e.describeArchitecture(ctx, projectID, trigger, userLogin, false)
}

// describeArchitecture — постановка задачи; markAuto ставит отметку
// автоописания в той же транзакции.
func (e *Engine) describeArchitecture(ctx context.Context, projectID, trigger, userLogin string, markAuto bool) (epicID, taskID string, err error) {
	p, err := e.St.GetProject(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	if p.RepoPath == "" {
		return "", "", ErrRepoNotConnected
	}
	// Проект на глобальном токене установки: Архитектор прочитал бы чужой
	// репозиторий и выложил его устройство в транскрипт и PR (ревью Opus,
	// круг 2). Fake-установке можно.
	if p.CredentialID == "" && !e.Adapters.Force {
		return "", "", ErrNoCredentials
	}
	eff, err := e.St.EffectivePolicy(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	proc := architectureProcess(eff.Presets)
	criteria := make([]domain.Criterion, 0, len(architecture.DescribeCriteria()))
	for _, c := range architecture.DescribeCriteria() {
		criteria = append(criteria, domain.Criterion{Text: c})
	}
	epicID, taskID, err = e.St.CreateArchitectureTask(ctx, store.ArchitectureTaskInput{
		ProjectID: projectID,
		Title:     "Архитектура проекта",
		Goal:      "Описать текущую архитектуру проекта в docs/architecture: C4, HLD, данные, сценарии и решения.",
		TaskTitle: "Описать архитектуру проекта",
		Description: architecture.DescribePrompt() + "\n\nСистема: " + p.Name +
			". Основная ветка: " + firstNonEmpty(p.DefaultBranch, e.BaseBranch) + ".",
		Criteria: criteria, Process: &proc, ProcessHash: policy.Hash(proc),
		Trigger: trigger, UserLogin: userLogin, MarkAuto: markAuto,
		// Ручная постановка заменяет упавшую задачу, автоописание нет:
		// повтор после неудачи делается только вручную (спека).
		ReplaceFailed: !markAuto,
	})
	if err != nil {
		return "", "", err
	}
	if err := e.St.RecomputeEpic(ctx, epicID); err != nil {
		return epicID, taskID, err
	}
	return epicID, taskID, nil
}

// ErrRepoNotConnected — у проекта нет подключённого репозитория.
var ErrRepoNotConnected = errors.New("у проекта нет подключённого репозитория")

// ErrNoCredentials — у проекта нет собственных учётных данных хостинга.
var ErrNoCredentials = errors.New("у проекта нет собственных учётных данных хостинга")

// MaybeAutoDescribe — автоописание по поводу connect или epic_start:
// пресет включён, отметки ещё нет, описания нет, задачи описания нет.
// Срабатывает один раз на проект (projects.architecture_auto_at).
func (e *Engine) MaybeAutoDescribe(ctx context.Context, projectID, trigger string) (epicID, taskID string, err error) {
	eff, err := e.St.EffectivePolicy(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	if !eff.Presets.AutoDescribe() {
		e.skipAutoDescribe(ctx, projectID, trigger, "preset_off")
		return "", "", nil
	}
	done, err := e.St.ArchitectureAutoDone(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	if done {
		e.skipAutoDescribe(ctx, projectID, trigger, "already_ran")
		return "", "", nil
	}
	row, existed, err := e.St.GetProjectArchitecture(ctx, projectID)
	if err != nil {
		return "", "", err
	}
	switch {
	case !existed:
		// Снимка ещё нет (проект подключён до обновления установки): без
		// него неизвестно, есть ли описание. Снимок строится в фоне, повод
		// проверяется после его записи. Запуск Epic этого не ждёт.
		e.refreshInBackground(projectID, false, trigger)
		return "", "", nil
	case row.Status == "ok" || row.Status == "invalid":
		e.skipAutoDescribe(ctx, projectID, trigger, "has_description")
		return "", "", nil
	case row.Status == "error" && row.ErrorKind == store.ArchErrNoCredentials:
		e.skipAutoDescribe(ctx, projectID, trigger, "no_credentials")
		return "", "", nil
	case row.Status == "error":
		// Чтение не удалось: задачу вслепую не ставим, повод повторится.
		e.skipAutoDescribe(ctx, projectID, trigger, "snapshot_error")
		return "", "", nil
	}
	// Отметка ставится в одной транзакции с задачей: при активной задаче
	// или сбое её нет, повод повторится.
	epicID, taskID, err = e.describeArchitecture(ctx, projectID, trigger, "", true)
	var active *store.ErrArchitectureTaskActive
	switch {
	case errors.As(err, &active):
		e.skipAutoDescribe(ctx, projectID, trigger, "task_active")
		return "", "", nil
	case errors.Is(err, store.ErrArchitectureAutoDone):
		e.skipAutoDescribe(ctx, projectID, trigger, "already_ran")
		return "", "", nil
	case err != nil:
		return "", "", err
	}
	return epicID, taskID, nil
}

func (e *Engine) skipAutoDescribe(ctx context.Context, projectID, trigger, reason string) {
	if _, err := e.St.AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, ActorID: "architecture", Type: "architecture.describe_skipped", ProjectID: projectID,
		Text:    "автоописание архитектуры пропущено: " + skipReasonText(reason),
		Payload: map[string]any{"project_id": projectID, "trigger": trigger, "reason": reason},
	}); err != nil {
		slog.Error("событие пропуска автоописания", "project", projectID, "err", err)
	}
}

func skipReasonText(reason string) string {
	switch reason {
	case "preset_off":
		return "пресет выключен"
	case "already_ran":
		return "уже срабатывало"
	case "has_description":
		return "описание уже есть"
	case "task_active":
		return "задача описания уже идёт"
	case "empty_repo":
		return "репозиторий пуст"
	case "snapshot_error":
		return "снимок архитектуры не прочитан"
	case "no_credentials":
		return "у проекта нет собственных учётных данных хостинга"
	case "explore_stages":
		return "в процессе Epic есть исследование"
	}
	return reason
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// architectureChecks — проверки шага test для задачи: задача описания
// проверяет только описание, у остальных к проверкам проекта добавляется
// проверка «Архитектура», если у проекта есть описание.
func (e *Engine) architectureChecks(ctx context.Context, task domain.Task, p domain.Project, checks []*pb.Check) []*pb.Check {
	base := firstNonEmpty(p.DefaultBranch, e.BaseBranch)
	// Адрес клонирования тот же, что уходит runner'у в назначении
	// (RepoUrl + ".git"); у fake-провайдера адреса нет.
	origin := ""
	if web := p.WebURL(); web != "" && p.Provider != string(scm.ProviderFake) {
		origin = web + ".git"
	}
	if task.Kind == domain.KindArchitecture {
		return []*pb.Check{architectureGateCheck(base, true, origin)}
	}
	status, err := e.St.ArchitectureStatus(ctx, p.ID)
	if err != nil {
		slog.Error("статус снимка архитектуры", "project", p.ID, "err", err)
		return checks
	}
	// Спека: проверку не добавляют только проектам со снимком absent.
	// Снимка ещё нет — описание может быть, гейт сам пропустит ветку без
	// изменений описания.
	if status == architecture.SnapshotAbsent {
		return checks
	}
	return append(checks, architectureGateCheck(base, false, origin))
}
