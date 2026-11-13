// Package analyst — фоновый анализ интервью Аналитиком (change
// add-product-discovery, спека backend/product-discovery): конспекты
// интервью, синтез, проверка ответов кодом пакета discovery, ход, usage и
// события версии исследования. Запросы выполняет control plane, runner не
// нужен.
package analyst

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/PavluninVladimir/rivet/internal/discovery"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Пределы анализа (design, решение 2). Синтез по пяти интервью — десятки
// тысяч токенов ответа, на thinking-модели это минуты: запросу 15 минут,
// анализу целиком 45.
const (
	DefaultParallel       = 3
	DefaultRequestTimeout = 15 * time.Minute
	DefaultTotalTimeout   = 45 * time.Minute
	// MaterialTimeout — добавка к таймауту анализа на каждый материал при
	// параллельности DefaultParallel: с сотнями отзывов первый анализ
	// конспектирует сотни материалов, и 45 минут ему мало.
	MaterialTimeout = 20 * time.Second
	// Лимиты ответа: конспект одного интервью и синтез по всем.
	SummaryMaxTokens   = 32000
	SynthesisMaxTokens = 64000
)

// Completer — модель Аналитика: текст ответа и токены. maxTokens — лимит
// ответа модели.
type Completer interface {
	CompleteWithUsage(ctx context.Context, model, prompt string, maxTokens int) (string, llm.Usage, error)
}

// Model — модель анализа. Цены — микродоллары за миллион токенов из
// каталога подключений, nil — стоимость не считается.
type Model struct {
	Client       Completer
	Model        string
	ConnectionID string
	InputPrice   *int64
	OutputPrice  *int64
}

// Job — один анализ: версия в статусе running, Аналитик, модель, интервью
// с текстом и контекст продукта (README и архитектура). Epic'и проекта
// читаются перед синтезом.
type Job struct {
	Run        store.DiscoveryRun
	AgentID    string
	Analyst    discovery.Analyst
	Model      Model
	Interviews []store.DiscoveryInterview
	Context    discovery.ProductContext
}

// Runner выполняет анализы. Нулевые поля — значения по умолчанию.
type Runner struct {
	St             *store.Store
	Parallel       int
	RequestTimeout time.Duration
	TotalTimeout   time.Duration
	// RetryDelay — пауза перед повтором после временной ошибки модели.
	RetryDelay time.Duration
}

// failure — анализ не завершён: причина и дефекты для версии.
type failure struct {
	reason  string
	defects []string
}

func (f *failure) Error() string { return f.reason }

// NoFindingsReason — причина сбоя версии, когда в конспектах нет ни одной
// подтверждённой находки (например, в проекте только короткие отзывы).
const NoFindingsReason = "в материалах нет подтверждённых находок: добавьте интервью или более подробные отзывы"

func anyFindings(ivs []discovery.InterviewInput) bool {
	for _, iv := range ivs {
		if len(iv.Summary.Findings) > 0 {
			return true
		}
	}
	return false
}

// AnalysisTimeout — таймаут анализа n материалов: 45 минут и по
// MaterialTimeout на материал, поделённые на параллельность конспектов.
// Кэш конспектов делает повторный анализ быстрее, таймаут считается с
// запасом на худший случай.
func AnalysisTimeout(n, parallel int) time.Duration {
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	return DefaultTotalTimeout + time.Duration(n)*MaterialTimeout/time.Duration(parallel)
}

// Run выполняет анализ до конца и переводит версию в ready или failed.
// Паника внутри анализа тоже завершает версию failed: running не залипает.
func (r *Runner) Run(ctx context.Context, job Job) {
	total := r.TotalTimeout
	if total <= 0 {
		total = AnalysisTimeout(len(job.Interviews), r.Parallel)
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	a := &analysis{r: r, job: job, started: time.Now()}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("паника анализа", "run", job.Run.ID, "panic", p, "stack", string(debug.Stack()))
			a.finishFailed(&failure{reason: fmt.Sprintf("внутренняя ошибка анализа: %v", p)})
		}
	}()
	err := a.run(ctx)
	if err == nil {
		return
	}
	var f *failure
	if !errors.As(err, &f) {
		f = &failure{reason: err.Error()}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		f.reason = fmt.Sprintf("анализ не уложился в %d минут", int(total.Minutes()))
	}
	a.finishFailed(f)
}

type analysis struct {
	r       *Runner
	job     Job
	started time.Time

	mu       sync.Mutex
	dropped  []discovery.Dropped
	warnings []string
	done     int
	// progressMu упорядочивает запись хода: «3 из 5» не приходит раньше
	// «2 из 5».
	progressMu sync.Mutex
}

func (a *analysis) st() *store.Store { return a.r.St }

func (a *analysis) run(ctx context.Context) error {
	ivs := a.job.Interviews
	total := len(ivs)
	inputs := make([]discovery.InterviewInput, total)
	parallel := a.r.Parallel
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallel)
	for i, iv := range ivs {
		g.Go(func() (err error) {
			// errgroup не переносит панику горутины в Wait: без recover здесь
			// паника на ответе модели уронила бы rivetd целиком.
			defer func() {
				if p := recover(); p != nil {
					slog.Error("паника конспекта", "run", a.job.Run.ID, "interview", iv.ID, "panic", p, "stack", string(debug.Stack()))
					err = &failure{reason: fmt.Sprintf("внутренняя ошибка конспекта «%s»: %v", iv.Title, p)}
				}
			}()
			sum, err := a.summary(gctx, iv)
			if err != nil {
				return err
			}
			inputs[i] = discovery.InterviewInput{ID: iv.ID, Kind: iv.Kind, Title: iv.Title, RespondentRole: iv.RespondentRole, Text: iv.Text, Summary: sum}
			a.progressMu.Lock()
			defer a.progressMu.Unlock()
			a.mu.Lock()
			a.done++
			done := a.done
			a.mu.Unlock()
			a.progress(ctx, store.StageSummaries, done, total)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	// Без единой подтверждённой находки синтезу не из чего строить
	// потребности: версия завершается сразу, без запросов к модели.
	if !anyFindings(inputs) {
		return &failure{reason: NoFindingsReason}
	}

	a.progress(ctx, store.StageSynthesis, total, total)

	existing, err := a.st().ExistingEpicsForDiscovery(ctx, a.job.Run.ProjectID)
	if err != nil {
		return err
	}
	pc := a.job.Context
	pc.Epics = existing
	var defects []string
	for attempt := 0; attempt < 2; attempt++ {
		prompt, trim := discovery.SynthesisPromptTrim(discovery.SynthesisTask{
			Analyst: a.job.Analyst, Interviews: inputs, Context: pc, Defects: defects,
		})
		var trimmed []string
		if trim.Removed > 0 {
			trimmed = append(trimmed, "вход синтеза сокращён до порога задания: "+trim.Text())
		}
		// Отзыв в наборе версии ещё не значит, что Аналитик его читал:
		// версия называет, сколько отзывов вошло в синтез.
		if t := trim.FeedbackText(); t != "" {
			trimmed = append(trimmed, t)
		}
		// Сводная цифра не на критическом пути: сбой записи только в лог.
		if err := a.st().SetRunFeedbackCoverage(ctx, a.job.Run.ID, trim.FeedbackTotal, trim.FeedbackSynthesized, trim.FeedbackWithoutFindings); err != nil {
			slog.Warn("отзывы во входе синтеза не записаны", "run", a.job.Run.ID, "err", err)
		}
		raw, err := a.complete(ctx, prompt, SynthesisMaxTokens)
		if errors.Is(err, llm.ErrTruncated) {
			defects = []string{truncatedSynthesis}
			if attempt == 0 {
				continue
			}
			return &failure{reason: "синтез: ответ модели оборван лимитом токенов", defects: defects}
		}
		if err != nil {
			return &failure{reason: "синтез: модель не ответила: " + err.Error()}
		}
		check := discovery.CheckSynthesis(inputs, existing, raw)
		if check.Retry && attempt == 0 {
			defects = check.Defects
			continue
		}
		if !check.Usable {
			a.addDropped(check.Dropped)
			return &failure{reason: "синтез не прошёл проверку", defects: check.Defects}
		}
		a.addDropped(check.Dropped)
		a.mu.Lock()
		warnings := append(append([]string{}, a.warnings...), check.Defects...)
		warnings = append(warnings, trimmed...)
		dropped := append([]discovery.Dropped{}, a.dropped...)
		a.mu.Unlock()
		a.recordDuration(context.WithoutCancel(ctx))
		var ok bool
		err = retryWrite(func() error {
			var werr error
			ok, werr = a.st().FinishRunReady(context.WithoutCancel(ctx), a.job.Run.ID, check.Result, dropped, warnings)
			return werr
		})
		if err != nil {
			return err
		}
		if ok {
			a.event(context.WithoutCancel(ctx), "discovery.analysis_ready",
				fmt.Sprintf("%s закончил анализ: персон %d, потребностей %d, Epic'ов %d", a.who(),
					len(check.Result.Personas), len(check.Result.Needs), len(check.Result.Epics))+": "+check.Result.Outcome, map[string]any{"outcome": check.Result.Outcome})
		}
		return nil
	}
	return &failure{reason: "синтез не прошёл проверку", defects: defects}
}

// Дефекты повтора после обрыва: ответ должен стать короче.
const (
	truncatedSummary   = "прошлый ответ оборван лимитом токенов: ответь короче, только JSON без пояснений, не больше 3 цитат на находку, формулировки находок в одно предложение"
	truncatedSynthesis = "прошлый ответ оборван лимитом токенов: ответь короче, только JSON без пояснений, не больше 3 цитат на потребность, описания персон и обоснования Epic'ов в одно-два предложения"
)

// refPlaceholder — подпись интервью в кэшируемом конспекте. Кэш общий для
// установки: название интервью другого проекта в него не попадает, а
// подставляется при чтении.
const refPlaceholder = "«⟨интервью⟩»"

func withRef(ds []discovery.Dropped, ref string) []discovery.Dropped {
	out := make([]discovery.Dropped, len(ds))
	for i, d := range ds {
		d.Ref = strings.ReplaceAll(d.Ref, refPlaceholder, ref)
		out[i] = d
	}
	return out
}

func defectsWithRef(ds []string, ref string) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = strings.ReplaceAll(d, refPlaceholder, ref)
	}
	return out
}

// retryWrite повторяет запись итога версии при сбое базы: версия не
// должна остаться в running из-за одной неудачной записи.
func retryWrite(fn func() error) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return err
}

// summary — конспект материала (интервью или отзыва): из кэша или по
// частям с проверкой и одним повтором на часть.
func (a *analysis) summary(ctx context.Context, iv store.DiscoveryInterview) (discovery.Summary, error) {
	instr := a.job.Analyst.SummaryInstructionSHA(iv.Kind)
	title := "«" + iv.Title + "»"
	if sum, dropped, ok, err := a.st().CachedSummary(ctx, iv.SHA256, a.job.Model.Model, instr); err != nil {
		return discovery.Summary{}, err
	} else if ok {
		a.addDropped(withRef(dropped, title))
		return withSource(iv, sum), nil
	}
	chunks := discovery.SplitInterview(iv.Text, discovery.SummaryChunkChars, discovery.ChunkOverlapLines)
	parts := make([]discovery.Summary, 0, len(chunks))
	var dropped []discovery.Dropped
	cacheable := true
	for i, ch := range chunks {
		part, d, ok, err := a.summaryPart(ctx, iv, ch, i+1, len(chunks))
		if err != nil {
			return discovery.Summary{}, err
		}
		cacheable = cacheable && ok
		parts = append(parts, part)
		dropped = append(dropped, d...)
	}
	sum, merged := discovery.MergeSummaries(refPlaceholder, parts)
	dropped = append(dropped, merged...)
	if iv.Kind == store.MaterialFeedback {
		// Конспект отзыва кэшируется без описания респондента: кэш общий
		// для установки, а одинаковый текст бывает у разных людей.
		sum.Respondent = discovery.Respondent{}
	}
	// Неразобранный ответ модели в кэш не идёт: следующий анализ спросит
	// модель снова.
	if cacheable {
		if err := a.st().PutSummary(ctx, iv.SHA256, a.job.Model.Model, instr, sum, dropped); err != nil {
			return discovery.Summary{}, err
		}
	}
	a.addDropped(withRef(dropped, title))
	return withSource(iv, sum), nil
}

// withSource — конспект отзыва с источником из материала: модель источника
// не видит, его подставляет код.
func withSource(iv store.DiscoveryInterview, sum discovery.Summary) discovery.Summary {
	if iv.Kind == store.MaterialFeedback {
		sum.Respondent = discovery.Respondent{Role: iv.RespondentRole}
	}
	return sum
}

// summaryPart — конспект части материала. cacheable=false — ответ модели
// не разобрался, конспект нельзя класть в общий кэш.
func (a *analysis) summaryPart(ctx context.Context, iv store.DiscoveryInterview, ch discovery.Chunk, part, parts int) (sum discovery.Summary, dropped []discovery.Dropped, cacheable bool, err error) {
	ref := "«" + iv.Title + "»"
	var defects []string
	for attempt := 0; attempt < 2; attempt++ {
		prompt := discovery.SummaryPrompt(discovery.SummaryTask{
			Analyst: a.job.Analyst, Kind: iv.Kind, InterviewID: iv.ID, Title: iv.Title, RespondentRole: iv.RespondentRole,
			Chunk: ch, Part: part, Parts: parts, Defects: defects,
		})
		raw, err := a.complete(ctx, prompt, SummaryMaxTokens)
		if errors.Is(err, llm.ErrTruncated) {
			defects = []string{truncatedSummary}
			if attempt == 0 {
				continue
			}
			return discovery.Summary{}, nil, false, &failure{reason: "конспект " + ref + ": ответ модели оборван лимитом токенов", defects: defects}
		}
		if err != nil {
			return discovery.Summary{}, nil, false, &failure{reason: "конспект " + ref + ": модель не ответила: " + err.Error()}
		}
		// Проверка с подписью-заглушкой: отброшенное уходит в общий кэш.
		check := discovery.CheckMaterialSummary(refPlaceholder, iv.Kind, iv.Text, raw)
		if check.Retry && attempt == 0 {
			defects = defectsWithRef(check.Defects, ref)
			continue
		}
		if !check.Usable && iv.Kind == store.MaterialFeedback {
			// Отзыв без подтверждённых находок (короткий текст, исправленная
			// моделью цитата) не валит анализ: он уходит в отброшенное с
			// причиной, пустой конспект кэшируется. Неразобранный ответ
			// модели о самом отзыве ничего не говорит: причина своя, в кэш
			// он не идёт.
			if !check.Parsed {
				return discovery.Summary{}, []discovery.Dropped{{Kind: "finding", Ref: refPlaceholder, Reason: discovery.ReasonFeedbackUnparsed}}, false, nil
			}
			dropped := append(check.Dropped, discovery.Dropped{Kind: "finding", Ref: refPlaceholder, Reason: discovery.ReasonFeedbackNoFinding})
			return discovery.Summary{}, dropped, true, nil
		}
		if !check.Usable {
			return discovery.Summary{}, check.Dropped, false, &failure{reason: "конспект " + ref + " не прошёл проверку", defects: defectsWithRef(check.Defects, ref)}
		}
		a.mu.Lock()
		a.warnings = append(a.warnings, defectsWithRef(check.Defects, ref)...)
		a.mu.Unlock()
		return check.Summary, check.Dropped, true, nil
	}
	return discovery.Summary{}, nil, false, &failure{reason: "конспект " + ref + " не прошёл проверку", defects: defects}
}

// complete — запрос Аналитика с таймаутом и учётом usage (роль analyst,
// без задачи и Epic'а).
func (a *analysis) complete(ctx context.Context, prompt string, maxTokens int) (string, error) {
	timeout := a.r.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	// Временная ошибка провайдера (перегрузка, 429, 5xx, обрыв потока или
	// сети) повторяется один раз: одна перегрузка на длинном синтезе не
	// валит весь анализ. Расход каждой попытки учитывается.
	for attempt := 0; ; attempt++ {
		text, err := a.completeOnce(ctx, prompt, maxTokens, timeout)
		if err != nil && attempt == 0 && llm.IsTransient(err) && ctx.Err() == nil {
			slog.Warn("временная ошибка модели Аналитика, повтор", "run", a.job.Run.ID, "err", err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(a.r.retryDelay()):
			}
			continue
		}
		return text, err
	}
}

func (r *Runner) retryDelay() time.Duration {
	if r.RetryDelay > 0 {
		return r.RetryDelay
	}
	return 5 * time.Second
}

func (a *analysis) completeOnce(ctx context.Context, prompt string, maxTokens int, timeout time.Duration) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	text, usage, err := a.job.Model.Client.CompleteWithUsage(rctx, a.job.Model.Model, prompt, maxTokens)
	dur := int(math.Round(time.Since(start).Seconds()))
	// Провайдер принял меньший лимит ответа: ответы короче и чаще
	// обрываются, это видно предупреждением в версии.
	if usage.MaxTokens > 0 && usage.MaxTokens < maxTokens {
		a.warnOnce(fmt.Sprintf("провайдер не принял лимит ответа %d токенов, запросы шли с лимитом %d: ответы короче и чаще обрываются", maxTokens, usage.MaxTokens))
	}
	if err == nil || usage.InputTokens != nil || usage.OutputTokens != nil {
		cost := Cost(usage, a.job.Model.InputPrice, a.job.Model.OutputPrice)
		bg := context.WithoutCancel(ctx)
		if uerr := a.st().RecordUsage(bg, store.UsageInput{
			ProjectID: a.job.Run.ProjectID, Model: a.job.Model.Model, Role: domain.RoleAnalyst,
			TokensIn: usage.InputTokens, TokensOut: usage.OutputTokens, CostUSD: cost, DurationS: dur,
		}); uerr != nil {
			slog.Error("usage Аналитика", "run", a.job.Run.ID, "err", uerr)
		}
		// Время версии — время анализа целиком (recordDuration), а не
		// сумма параллельных запросов.
		if uerr := a.st().AddRunUsage(bg, a.job.Run.ID, usage.InputTokens, usage.OutputTokens, cost, 0); uerr != nil {
			slog.Error("usage версии исследования", "run", a.job.Run.ID, "err", uerr)
		}
	}
	if err != nil && rctx.Err() != nil && ctx.Err() == nil {
		return "", fmt.Errorf("нет ответа за %d минут", int(timeout.Minutes()))
	}
	return text, err
}

// Cost — стоимость запроса в долларах по ценам каталога (микродоллары за
// миллион токенов). Без токенов или цен стоимость неизвестна.
func Cost(u llm.Usage, inPrice, outPrice *int64) *float64 {
	if u.InputTokens == nil || u.OutputTokens == nil || inPrice == nil || outPrice == nil {
		return nil
	}
	v := (float64(*u.InputTokens)*float64(*inPrice) + float64(*u.OutputTokens)*float64(*outPrice)) / 1e12
	return &v
}

// warnOnce — предупреждение версии без повторов.
func (a *analysis) warnOnce(w string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.warnings {
		if x == w {
			return
		}
	}
	a.warnings = append(a.warnings, w)
}

func (a *analysis) addDropped(d []discovery.Dropped) {
	if len(d) == 0 {
		return
	}
	a.mu.Lock()
	a.dropped = append(a.dropped, d...)
	a.mu.Unlock()
}

func (a *analysis) progress(ctx context.Context, stage string, done, total int) {
	bg := context.WithoutCancel(ctx)
	if err := a.st().SetRunProgress(bg, a.job.Run.ID, stage, done, total); err != nil {
		slog.Error("ход анализа", "run", a.job.Run.ID, "err", err)
	}
	text := fmt.Sprintf("%s конспектирует: %d из %d", a.who(), done, total)
	if stage == store.StageSynthesis {
		text = a.who() + " синтезирует результат"
	}
	if _, err := a.st().AppendEvent(bg, store.EventInput{
		ActorKind: domain.ActorSystem, Type: "discovery.analysis_progress", ProjectID: a.job.Run.ProjectID, Text: text,
		Payload: map[string]any{"run_id": a.job.Run.ID, "stage": stage, "done": done, "total": total},
	}); err != nil {
		slog.Error("событие хода анализа", "run", a.job.Run.ID, "err", err)
	}
}

// recordDuration — длительность анализа целиком в версии исследования.
func (a *analysis) recordDuration(ctx context.Context) {
	sec := int(math.Round(time.Since(a.started).Seconds()))
	if err := a.st().AddRunUsage(ctx, a.job.Run.ID, nil, nil, nil, sec); err != nil {
		slog.Error("длительность анализа", "run", a.job.Run.ID, "err", err)
	}
}

func (a *analysis) finishFailed(f *failure) {
	ctx := context.Background()
	a.recordDuration(ctx)
	a.mu.Lock()
	dropped := append([]discovery.Dropped{}, a.dropped...)
	a.mu.Unlock()
	var ok bool
	err := retryWrite(func() error {
		var werr error
		ok, werr = a.st().FinishRunFailed(ctx, a.job.Run.ID, f.reason, f.defects, dropped)
		return werr
	})
	if err != nil {
		slog.Error("провал анализа", "run", a.job.Run.ID, "err", err)
		return
	}
	if ok {
		a.event(ctx, "discovery.analysis_failed", "анализ не завершён: "+f.reason, map[string]any{"error": f.reason})
	}
}

// who — подпись Аналитика в событиях: «Аналитик Вера».
func (a *analysis) who() string {
	t := a.job.Analyst.Title
	if t == "" {
		t = "Аналитик"
	}
	if a.job.Analyst.Name != "" {
		t += " " + a.job.Analyst.Name
	}
	return t
}

func (a *analysis) event(ctx context.Context, typ, text string, extra map[string]any) {
	payload := Payload(a.job.Run, a.job.AgentID, a.job.Analyst)
	for k, v := range extra {
		payload[k] = v
	}
	if _, err := a.st().AppendEvent(ctx, store.EventInput{
		ActorKind: domain.ActorSystem, Type: typ, ProjectID: a.job.Run.ProjectID, Text: text, Payload: payload,
	}); err != nil {
		slog.Error("событие анализа", "run", a.job.Run.ID, "type", typ, "err", err)
	}
}

// Payload — общая часть событий анализа: версия, агент, роль и модель
// (api-contract «Реальное время»).
func Payload(run store.DiscoveryRun, agentID string, an discovery.Analyst) map[string]any {
	var agent any
	if agentID != "" {
		agent = map[string]any{"id": agentID, "name": an.Name}
	}
	title := an.Title
	if title == "" {
		title = "Аналитик"
	}
	return map[string]any{
		"run_id": run.ID, "version": run.Version, "agent": agent,
		"role":  map[string]any{"id": domain.RoleAnalyst, "title": title},
		"model": run.Model,
	}
}
