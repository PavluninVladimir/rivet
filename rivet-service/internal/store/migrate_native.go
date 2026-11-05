package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Перевод ссылок на удалённые профили агентов в документах (миграция 0034,
// add-native-agent, задача 1.11). SQL миграции переводит строки таблиц
// (агенты команды, шаблоны ролей, открытые запуски) и удаляет профили.
// Документы политик и снимки процессов задач разбираются здесь: новой
// версии политики нужен канонический хэш (policy.Hash), а ключи
// agent_models при переводе могут столкнуться.
//
// Шаг выполняется в транзакции миграции сразу после её SQL и пишет в базу
// только явными запросами по колонкам, которые есть на момент 0034: код
// хранилища следующих версий может ждать колонок более поздних миграций.

// migrationHooks — шаги миграций на Go по имени файла миграции.
var migrationHooks = map[string]func(context.Context, pgx.Tx) error{
	"0034_native_agent.sql": migrateNativeAgentDocs,
}

// policyMigrationActor — автор версий политик, созданных миграцией.
const policyMigrationActor = "system"

// migrationCatalog — модели агента Rivet на момент миграции (видимые модели
// включённых подключений) и число ссылок на удалённые профили.
type migrationCatalog struct {
	pairs  map[string]bool
	models map[string]bool
	refs   map[string]int64
}

func (c migrationCatalog) hasPair(connID, model string) bool { return c.pairs[connID+"/"+model] }

// modelKept — явная модель участника остаётся после перевода на rivet: она
// есть среди моделей каталога или каталог пуст (проверка процесса пустым
// каталогом модели не ограничивает).
func (c migrationCatalog) modelKept(model string) bool {
	return model == "" || len(c.models) == 0 || c.models[model]
}

func loadMigrationCatalog(ctx context.Context, tx pgx.Tx) (migrationCatalog, error) {
	cat := migrationCatalog{pairs: map[string]bool{}, models: map[string]bool{}, refs: map[string]int64{}}
	rows, err := tx.Query(ctx, `
		SELECT c.id, m->>'id' FROM model_connections c, jsonb_array_elements(c.models) m
		WHERE c.enabled AND NOT COALESCE((m->>'hidden')::boolean, false) AND NOT COALESCE((m->>'missing')::boolean, false)`)
	if err != nil {
		return cat, err
	}
	for rows.Next() {
		var conn, model string
		if err := rows.Scan(&conn, &model); err != nil {
			rows.Close()
			return cat, err
		}
		cat.pairs[conn+"/"+model] = true
		cat.models[model] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cat, err
	}
	// Снимок удаляемых профилей создаёт SQL миграции; без него (шаг
	// вызван отдельно) все профили равны.
	var snapshot bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('pg_temp.rivet_retired_agents') IS NOT NULL`).Scan(&snapshot); err != nil {
		return cat, err
	}
	if !snapshot {
		return cat, nil
	}
	rows, err = tx.Query(ctx, `SELECT id, refs FROM pg_temp.rivet_retired_agents`)
	if err != nil {
		return cat, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return cat, err
		}
		cat.refs[id] = n
	}
	return cat, rows.Err()
}

// migrateNativeAgentDocs — шаг миграции 0034: действующие версии политик со
// ссылками на другие агенты получают новую версию от системного актора с
// событием policy.migrated (прежние версии остаются в истории), снимки
// процессов незавершённых задач правятся на месте.
func migrateNativeAgentDocs(ctx context.Context, tx pgx.Tx) error {
	cat, err := loadMigrationCatalog(ctx, tx)
	if err != nil {
		return err
	}
	if err := migratePolicyAgentRefs(ctx, tx, cat); err != nil {
		return fmt.Errorf("политики: %w", err)
	}
	if err := migrateTaskProcessAgentRefs(ctx, tx, cat); err != nil {
		return fmt.Errorf("снимки процессов задач: %w", err)
	}
	return nil
}

func migratePolicyAgentRefs(ctx context.Context, tx pgx.Tx, cat migrationCatalog) error {
	type doc struct {
		scope, projectID string
		version          int
		content          []byte
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (scope, project_id) scope, COALESCE(project_id::text, ''), version, content
		FROM policy_versions ORDER BY scope, project_id, version DESC`)
	if err != nil {
		return err
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.scope, &d.projectID, &d.version, &d.content); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range docs {
		var content any
		var changes []string
		switch d.scope {
		case PolicyScopeInstallation:
			var p policy.Presets
			if json.Unmarshal(d.content, &p) != nil {
				continue // нечитаемая версия и так не действует: не трогаем
			}
			changes = migrateAgentRefs(p.Process, &p.AgentModels, p.RoleModels, cat)
			content = p
		case PolicyScopeProject:
			var o policy.Overrides
			if json.Unmarshal(d.content, &o) != nil {
				continue
			}
			changes = migrateAgentRefs(o.Process, &o.AgentModels, o.RoleModels, cat)
			content = o
		default:
			continue
		}
		if len(changes) == 0 {
			continue
		}
		raw, err := json.Marshal(content)
		if err != nil {
			return err
		}
		hash := policy.Hash(content)
		version := d.version + 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO policy_versions (scope, project_id, version, hash, content, created_by)
			VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6)`,
			d.scope, d.projectID, version, hash, raw, policyMigrationActor); err != nil {
			return err
		}
		payload := map[string]any{"scope": d.scope, "version": version, "from_version": d.version,
			"hash": hash, "changes": changes}
		what := "политика установки"
		if d.projectID != "" {
			payload["project_id"] = d.projectID
			what = "политика проекта"
		}
		pj, _ := json.Marshal(payload)
		if _, err := tx.Exec(ctx, `
			INSERT INTO events (actor_kind, actor_id, type, project_id, text, payload)
			VALUES ('system', 'migration', 'policy.migrated', NULLIF($1, '')::uuid, $2, $3)`,
			d.projectID, fmt.Sprintf("%s переведена на агента Rivet: версия %d (%s), изменений %d",
				what, version, shortHash(hash), len(changes)), pj); err != nil {
			return err
		}
	}
	return nil
}

// migrateAgentRefs переводит ссылки документа политики на агента Rivet и
// возвращает список изменений для события policy.migrated:
//   - ключи agent_models: переопределение переходит на ключ rivet, если его
//     ещё нет и пара есть в каталоге (при нескольких кандидатах — профиль с
//     большим числом ссылок), остальные снимаются;
//   - role_models: агент переопределения становится rivet, переопределение
//     с парой вне каталога снимается;
//   - участники процесса: тип агента становится rivet, модель вне каталога
//     снимается.
//
// После перевода документ проходит проверки сохранения политики по агенту
// и моделям: устаревшее переопределение не блокирует правку политики.
func migrateAgentRefs(proc *policy.Process, am *map[string]policy.AgentModel, rm map[string]policy.RoleModel, cat migrationCatalog) []string {
	var changes []string
	if len(*am) > 0 {
		var old []string
		for k := range *am {
			if k != AgentRivet {
				old = append(old, k)
			}
		}
		sort.Slice(old, func(i, j int) bool {
			mi, mj := (*am)[old[i]], (*am)[old[j]]
			if vi, vj := cat.hasPair(mi.ConnectionID, mi.Model), cat.hasPair(mj.ConnectionID, mj.Model); vi != vj {
				return vi
			}
			if cat.refs[old[i]] != cat.refs[old[j]] {
				return cat.refs[old[i]] > cat.refs[old[j]]
			}
			return old[i] < old[j]
		})
		_, haveRivet := (*am)[AgentRivet]
		for _, k := range old {
			m := (*am)[k]
			delete(*am, k)
			switch {
			case haveRivet:
				changes = append(changes, fmt.Sprintf("agent_models.%s снято: модель агента rivet уже задана", k))
			case !cat.hasPair(m.ConnectionID, m.Model):
				changes = append(changes, fmt.Sprintf("agent_models.%s снято: модели %s/%s нет в каталоге подключений", k, m.ConnectionID, m.Model))
			default:
				(*am)[AgentRivet] = m
				haveRivet = true
				changes = append(changes, fmt.Sprintf("agent_models.%s → agent_models.rivet (%s/%s)", k, m.ConnectionID, m.Model))
			}
		}
	}
	roles := make([]string, 0, len(rm))
	for id := range rm {
		roles = append(roles, id)
	}
	sort.Strings(roles)
	for _, id := range roles {
		m := rm[id]
		if m.Agent == "" || m.Agent == AgentRivet {
			continue
		}
		if !cat.hasPair(m.ConnectionID, m.Model) {
			delete(rm, id)
			changes = append(changes, fmt.Sprintf("role_models.%s снято: модели %s/%s нет в каталоге подключений (агент %s)", id, m.ConnectionID, m.Model, m.Agent))
			continue
		}
		changes = append(changes, fmt.Sprintf("role_models.%s.agent: %s → rivet", id, m.Agent))
		m.Agent = AgentRivet
		rm[id] = m
	}
	if proc != nil {
		for si := range proc.Steps {
			st := &proc.Steps[si]
			for pi := range st.Participants {
				a := st.Participants[pi].Agent
				if a == nil || a.Kind == "" || a.Kind == AgentRivet {
					continue
				}
				field := fmt.Sprintf("process.%s.participants[%d].agent", st.ID, pi)
				changes = append(changes, fmt.Sprintf("%s.kind: %s → rivet", field, a.Kind))
				a.Kind = AgentRivet
				if !cat.modelKept(a.Model) {
					changes = append(changes, fmt.Sprintf("%s.model %s снята: модели нет в каталоге подключений", field, a.Model))
					a.Model = ""
				}
			}
		}
	}
	return changes
}

// migrateTaskProcessAgentRefs переводит участников снимков процессов
// незавершённых задач на rivet по тем же правилам, что участников
// процесса в политике. Снимок разбирается как произвольный JSON, чтобы
// не потерять поля, которых нет в структурах этой версии.
func migrateTaskProcessAgentRefs(ctx context.Context, tx pgx.Tx, cat migrationCatalog) error {
	rows, err := tx.Query(ctx, `
		SELECT id::text, process FROM tasks
		WHERE process IS NOT NULL AND status NOT IN ('done', 'cancelled')
		  AND jsonb_path_exists(process, '$.steps[*].participants[*].agent.kind ? (@ != "" && @ != "rivet")')`)
	if err != nil {
		return err
	}
	type snap struct {
		id  string
		raw []byte
	}
	var snaps []snap
	for rows.Next() {
		var s snap
		if err := rows.Scan(&s.id, &s.raw); err != nil {
			rows.Close()
			return err
		}
		snaps = append(snaps, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range snaps {
		var doc map[string]any
		if json.Unmarshal(s.raw, &doc) != nil {
			continue
		}
		changed := false
		steps, _ := doc["steps"].([]any)
		for _, st := range steps {
			step, _ := st.(map[string]any)
			parts, _ := step["participants"].([]any)
			for _, p := range parts {
				part, _ := p.(map[string]any)
				agent, _ := part["agent"].(map[string]any)
				kind, _ := agent["kind"].(string)
				if agent == nil || kind == "" || kind == AgentRivet {
					continue
				}
				agent["kind"] = AgentRivet
				if model, _ := agent["model"].(string); !cat.modelKept(model) {
					agent["model"] = ""
				}
				changed = true
			}
		}
		if !changed {
			continue
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET process=$2 WHERE id=$1::uuid`, s.id, raw); err != nil {
			return err
		}
	}
	return nil
}
