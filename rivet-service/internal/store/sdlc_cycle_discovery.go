package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/cycle"
)

// Фазы «Исследование» и «Архитектура» агрегата цикла (fix-cycle-phase-data):
// материалы и версии исследования (add-product-discovery), снимок
// архитектуры, задачи описания и решения ADR (add-architecture-c4,
// add-architecture-erd-sequence-adr). Всё читается в транзакции агрегата.

// Лимиты артефактов фаз до общей обрезки в cycle.Build: потребностей
// меньше пяти, чтобы рядом с ними оставалось место интервью, решений
// меньше пяти, чтобы оставалось место снимку.
const (
	cycleNeedArtifacts = 3
	cycleADRArtifacts  = 4
)

// loadCycleDiscovery — материалы, не учтённые последней готовой версией
// исследования, идущий анализ и бэклог на утверждении, артефакты (интервью
// и потребности) и версии, утверждённые за период.
func loadCycleDiscovery(ctx context.Context, tx pgx.Tx, projectID string, from time.Time, in *cycle.Input) error {
	// Последняя готовая версия — последняя с результатом анализа: ready,
	// approved или superseded более новой. Её набор материалов учтён.
	var (
		lastID       string
		lastMats     []byte
		lastNeeds    []byte
		lastFinished *time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT id::text, interviews, COALESCE(result->'needs', '[]'::jsonb), COALESCE(finished_at, started_at)
		FROM discovery_runs WHERE project_id=$1 AND result IS NOT NULL
		ORDER BY version DESC LIMIT 1`, projectID).Scan(&lastID, &lastMats, &lastNeeds, &lastFinished)
	switch {
	case err == nil:
		var mats []RunInterview
		_ = json.Unmarshal(lastMats, &mats)
		in.LastRun.Included = make(map[string]bool, len(mats))
		for _, m := range mats {
			in.LastRun.Included[m.ID] = true
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT id::text, kind, title, created_at FROM discovery_interviews
		WHERE project_id=$1 AND deleted_at IS NULL ORDER BY created_at, id`, projectID)
	if err != nil {
		return err
	}
	var mats []cycle.Material
	for rows.Next() {
		var m cycle.Material
		if err := rows.Scan(&m.ID, &m.Kind, &m.Title, &m.Since); err != nil {
			rows.Close()
			return err
		}
		mats = append(mats, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Элемент фазы называет вид материала, артефакт — только название.
	// Отзыв уже назван «Отзыв · <источник>». Артефакты: пять последних
	// интервью, отзывы видны элементами фазы и её счётчиком.
	interviews := 0
	for i := len(mats) - 1; i >= 0 && interviews < cycle.MaxArtifacts; i-- {
		if m := mats[i]; m.Kind == cycle.MaterialInterview {
			in.Artifacts = append(in.Artifacts, cycle.Artifact{Phase: cycle.Discovery, Kind: "interview", ID: m.ID,
				Title: m.Title, At: m.Since, Ref: cycle.Ref{View: "discovery"}})
			interviews++
		}
	}
	for _, m := range mats {
		if m.Kind != cycle.MaterialFeedback {
			m.Title = "Интервью · " + m.Title
		}
		in.Materials = append(in.Materials, m)
	}

	// Идущий анализ и бэклог на утверждении.
	rows, err = tx.Query(ctx, `
		SELECT id::text, version, status, stage, progress_done, progress_total, outdated,
			CASE WHEN status = 'ready' THEN COALESCE(finished_at, started_at) ELSE started_at END
		FROM discovery_runs WHERE project_id=$1 AND status IN ('running','ready')`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var a cycle.Analysis
		var version, done, total int
		var stage string
		var outdated bool
		if err := rows.Scan(&a.ID, &version, &a.Status, &stage, &done, &total, &outdated, &a.Since); err != nil {
			rows.Close()
			return err
		}
		switch {
		case a.Status == RunReady && outdated:
			// Интервью версии удалено: утвердить её нельзя, человеку нужен
			// новый анализ.
			a.Title = fmt.Sprintf("Бэклог исследования · версия %d устарела, нужен новый анализ", version)
		case a.Status == RunReady:
			a.Title = fmt.Sprintf("Бэклог исследования · версия %d ждёт утверждения", version)
		default:
			a.Title = fmt.Sprintf("Анализ исследования · версия %d · %s", version, analysisAction(stage, done, total))
		}
		in.Analyses = append(in.Analyses, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Артефакты: важнейшие потребности последней готовой версии.
	if lastID != "" && lastFinished != nil {
		var needs []struct {
			ID         string `json:"id"`
			Title      string `json:"title"`
			Importance string `json:"importance"`
		}
		_ = json.Unmarshal(lastNeeds, &needs)
		// Сначала важные, внутри важности в порядке результата.
		rank := map[string]int{"high": 0, "medium": 1, "low": 2}
		imp := func(s string) int {
			if r, ok := rank[s]; ok {
				return r
			}
			return len(rank)
		}
		sort.SliceStable(needs, func(i, j int) bool { return imp(needs[i].Importance) < imp(needs[j].Importance) })
		if len(needs) > cycleNeedArtifacts {
			needs = needs[:cycleNeedArtifacts]
		}
		for i, n := range needs {
			in.Artifacts = append(in.Artifacts, cycle.Artifact{Phase: cycle.Discovery, Kind: "need",
				ID: lastID + "/" + n.ID, Title: n.Title, At: *lastFinished, Order: i,
				Ref: cycle.Ref{View: "discovery", ID: lastID}})
		}
	}

	// Есть данные — есть материалы или версии исследования. Завершённые
	// за период — версии, утверждённые за период.
	var avail bool
	var approved int
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM discovery_interviews WHERE project_id=$1 AND deleted_at IS NULL)
		    OR EXISTS (SELECT 1 FROM discovery_runs WHERE project_id=$1),
		       (SELECT count(*) FROM discovery_runs WHERE project_id=$1 AND status='approved' AND approved_at >= $2)`,
		projectID, from).Scan(&avail, &approved); err != nil {
		return err
	}
	in.Available[cycle.Discovery] = avail
	in.DonePeriod[cycle.Discovery] = approved
	return nil
}

// analysisAction — что делает Аналитик на этапе анализа.
func analysisAction(stage string, done, total int) string {
	if stage == StageSynthesis {
		return "синтезирует результат"
	}
	return fmt.Sprintf("конспектирует материалы: %d из %d", done, total)
}

// cycleAnalyses — идущие анализы установки по агенту команды: текущая
// работа Аналитика в агрегате цикла. Агент общий для проектов, а анализ
// ограничен одним на проект, поэтому Аналитик может вести анализы
// нескольких проектов сразу. Для агента берётся анализ проекта projectID,
// если он есть, иначе самый ранний: картина не зависит от порядка строк.
// Работа в чужом проекте отдаётся без подробностей хода: число интервью
// того проекта зрителю этого не показывается.
func cycleAnalyses(ctx context.Context, tx pgx.Tx, projectID string) (map[string]*cycle.Work, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, project_id::text, version, stage, progress_done, progress_total, agent_id
		FROM discovery_runs WHERE status='running' AND agent_id <> ''
		ORDER BY agent_id, project_id = $1 DESC, started_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*cycle.Work{}
	for rows.Next() {
		var w cycle.Work
		var version, done, total int
		var stage, agent string
		if err := rows.Scan(&w.ID, &w.ProjectID, &version, &stage, &done, &total, &agent); err != nil {
			return nil, err
		}
		if out[agent] != nil {
			continue
		}
		w.Kind, w.Phase = "analysis", cycle.Discovery
		w.Title = fmt.Sprintf("Исследование · версия %d", version)
		w.Action = analysisAction(stage, done, total)
		if w.ProjectID != projectID {
			w.Action = "анализирует материалы исследования"
		}
		out[agent] = &w
	}
	return out, rows.Err()
}

// adrStatusTitle — статус решения для названия артефакта; у принятого
// решения статус не пишется.
var adrStatusTitle = map[string]string{
	"proposed": "предложено", "superseded": "заменено", "deprecated": "устарело", "rejected": "отклонено",
}

// loadCycleArchitecture — снимок архитектуры и последние решения ADR его
// корректной версии (артефакты), описания архитектуры, завершённые за
// период. Незавершённые задачи описания и Epic'и exploring приходят в
// фазу из общих выборок задач и Epic'ов.
func loadCycleArchitecture(ctx context.Context, tx pgx.Tx, projectID string, from time.Time, in *cycle.Input) error {
	// Данные корректной версии: при ok текущие, иначе последней корректной.
	const valid = `(CASE WHEN pa.status = 'ok' THEN pa.data ELSE pa.valid_data END)`
	var (
		status, sha, validSHA, system string
		at                            time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT pa.status, pa.sha, pa.valid_sha, COALESCE(`+valid+`->'system'->>'name', ''),
			COALESCE(pa.read_at, pa.updated_at)
		FROM project_architecture pa WHERE pa.project_id=$1`, projectID).Scan(&status, &sha, &validSHA, &system, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		status = ""
	case err != nil:
		return err
	}
	if status != "" && status != "absent" {
		title := "Снимок архитектуры"
		if system != "" {
			title += " · " + system
		}
		switch status {
		case "invalid":
			title += " · есть ошибки проверки"
		case "error":
			title += " · не прочитан"
		}
		if s := shortSHA(sha); s != "" {
			title += " · " + s
		}
		id := sha
		if id == "" {
			id = projectID
		}
		in.Artifacts = append(in.Artifacts, cycle.Artifact{Phase: cycle.Architecture, Kind: "architecture", ID: id,
			Title: title, At: at, Ref: cycle.Ref{View: "architecture"}})
	}
	if status != "" && (status == "ok" || validSHA != "") {
		// Последние по номеру решения. У решения нет времени появления в
		// снимке, моментом служит его дата из front matter.
		rows, err := tx.Query(ctx, `
			SELECT (d->>'number')::int, COALESCE(d->>'title', ''), COALESCE(d->>'status', ''), COALESCE(d->>'date', '')
			FROM project_architecture pa,
				jsonb_array_elements(CASE WHEN jsonb_typeof(`+valid+`->'decisions') = 'array'
					THEN `+valid+`->'decisions' ELSE '[]'::jsonb END) d
			WHERE pa.project_id=$1 AND jsonb_typeof(d->'number') = 'number'
			ORDER BY 1 DESC LIMIT $2`, projectID, cycleADRArtifacts)
		if err != nil {
			return err
		}
		for rows.Next() {
			var num int
			var title, st, date string
			if err := rows.Scan(&num, &title, &st, &date); err != nil {
				rows.Close()
				return err
			}
			// Решения одного момента — новые номера первыми.
			a := cycle.Artifact{Phase: cycle.Architecture, Kind: "adr", ID: strconv.Itoa(num),
				Title: fmt.Sprintf("ADR-%04d · %s", num, title), At: at, Order: -num,
				Ref: cycle.Ref{View: "adr", ID: strconv.Itoa(num)}}
			if d, err := time.Parse("2006-01-02", date); err == nil && d.Before(at) {
				a.At = d
			}
			if t := adrStatusTitle[st]; t != "" {
				a.Title += " (" + t + ")"
			}
			in.Artifacts = append(in.Artifacts, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	// Есть данные — снимок не absent или у проекта были задачи описания.
	var described bool
	var done int
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM epics WHERE project_id=$1 AND kind='architecture'),
		       (SELECT count(*) FROM tasks t JOIN epics e ON e.id = t.epic_id
		        WHERE e.project_id=$1 AND t.kind='architecture' AND t.status='done' AND t.done_at >= $2)`,
		projectID, from).Scan(&described, &done); err != nil {
		return err
	}
	in.Available[cycle.Architecture] = (status != "" && status != "absent") || described
	in.DonePeriod[cycle.Architecture] = done
	return nil
}

// shortSHA — первые 7 символов SHA коммита.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
