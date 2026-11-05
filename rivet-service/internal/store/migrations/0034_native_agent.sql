-- Встроенный агент Rivet (add-native-agent, design решение 7): runner сам
-- ведёт агентский цикл, сторонних CLI-агентов в продукте нет. Каталог
-- агентов сводится к одному профилю rivet с адаптером native, все ссылки на
-- удалённые профили (claude-code, codex, opencode, свои профили-обёртки и
-- агенты вне каталога) переходят на rivet.
--
-- Документы политик и снимки процессов задач переводит шаг миграции на Go
-- (store/migrate_native.go): он выполняется в этой же транзакции сразу
-- после SQL. Повторное выполнение миграции ничего не меняет.

-- Лимиты сессии профиля: ходы, токены (некешированный вход плюс выход),
-- минуты; пустой объект — значения цикла по умолчанию.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS limits jsonb NOT NULL DEFAULT '{}';
ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_adapter_check;

-- Снимок удаляемых профилей до перевода ссылок: режим секретов, модель по
-- умолчанию и число ссылок (runner'ы, агенты команды, шаблоны ролей,
-- открытые запуски, снимки процессов задач в работе, действующие версии
-- политик). Профиль без ссылок модель по умолчанию и режимы secure/always
-- агенту Rivet не передаёт, а режим never передаёт всегда. Таблицу читает
-- и шаг на Go: при конфликте переопределений agent_models проекта
-- выигрывает профиль с большим числом ссылок.
DROP TABLE IF EXISTS pg_temp.rivet_retired_agents;
CREATE TEMP TABLE rivet_retired_agents ON COMMIT DROP AS
SELECT a.id, a.secrets, a.default_model,
       (SELECT count(*) FROM runners r WHERE r.agent = a.id)
     + (SELECT count(*) FROM team_agents t WHERE t.agent = a.id)
     + (SELECT count(*) FROM agent_roles ro WHERE ro.agent = a.id)
     + (SELECT count(*) FROM task_step_runs sr
        WHERE sr.verdict IS NULL AND a.id IN (sr.agent_kind, sr.role_agent, sr.pref_agent))
     + (SELECT count(*) FROM tasks t
        WHERE t.process IS NOT NULL AND t.status NOT IN ('done', 'cancelled')
          AND jsonb_path_exists(t.process, '$.steps[*].participants[*].agent.kind ? (@ == $id)', jsonb_build_object('id', a.id)))
     + (SELECT count(*) FROM (SELECT DISTINCT ON (scope, project_id) content FROM policy_versions
                              ORDER BY scope, project_id, version DESC) v
        WHERE COALESCE(v.content->'agent_models' ? a.id, false)
           OR jsonb_path_exists(v.content, '$.role_models.*.agent ? (@ == $id)', jsonb_build_object('id', a.id))
           OR jsonb_path_exists(v.content, '$.process.steps[*].participants[*].agent.kind ? (@ == $id)', jsonb_build_object('id', a.id))
       ) AS refs
FROM agents a WHERE a.adapter <> 'native';

-- Профиль rivet. Режим секретов — самый строгий среди удаляемых профилей
-- со ссылками и профилей в режиме never (never строже secure, secure
-- строже always): решение администратора не отдавать ключи runner'ам не
-- теряется, даже если runner'ов и агентов команды на этом профиле ещё нет.
-- Без таких профилей — secure. Уже существующий профиль rivet режим не
-- ослабляет.
WITH carry AS (
    SELECT (ARRAY['never', 'secure', 'always'])[min(array_position(ARRAY['never', 'secure', 'always'], secrets))] AS secrets
    FROM rivet_retired_agents WHERE refs > 0 OR secrets = 'never'
)
INSERT INTO agents (id, name, adapter, command, capabilities, env, args, secrets, preset)
SELECT 'rivet', 'Агент Rivet', 'native', '', '{}', '[]', '[]', COALESCE(carry.secrets, 'secure'), true FROM carry
ON CONFLICT (id) DO UPDATE SET adapter = 'native', command = '', env = '[]', args = '[]', preset = true,
    secrets = (ARRAY['never', 'secure', 'always'])[LEAST(
        array_position(ARRAY['never', 'secure', 'always'], agents.secrets),
        array_position(ARRAY['never', 'secure', 'always'], (SELECT secrets FROM carry)))];

-- Модель по умолчанию — от удаляемого профиля с наибольшим числом ссылок
-- (при равенстве claude-code), если его пара есть среди видимых моделей
-- включённых подключений. Иначе действует первая модель каталога.
UPDATE agents SET default_model = d.default_model
FROM (SELECT o.default_model FROM rivet_retired_agents o
      WHERE o.refs > 0 AND o.default_model IS NOT NULL
        AND EXISTS (SELECT 1 FROM model_connections c, jsonb_array_elements(c.models) m
                    WHERE c.enabled AND c.id = o.default_model->>'connection_id' AND m->>'id' = o.default_model->>'model'
                      AND NOT COALESCE((m->>'hidden')::boolean, false) AND NOT COALESCE((m->>'missing')::boolean, false))
      ORDER BY o.refs DESC, (o.id = 'claude-code') DESC, o.id
      LIMIT 1) d
WHERE agents.id = 'rivet' AND agents.default_model IS NULL;

-- Ссылки агентов команды и шаблонов ролей переходят на rivet. Пара
-- подключение/модель агента команды сохраняется: модели rivet — все модели
-- каталога, а недоступная модель помечается при назначении.
UPDATE team_agents SET agent = 'rivet' WHERE agent NOT IN ('', 'rivet');
UPDATE agent_roles SET agent = 'rivet' WHERE agent NOT IN ('', 'rivet');

-- Открытые запуски шагов (ожидающие и идущие): тип агента участника,
-- профиль шаблона и предпочтение проекта переходят на rivet, иначе запуск
-- не подошёл бы ни одному runner'у. Явная модель участника, которой нет
-- среди моделей агента Rivet, снимается: требование, которого не выполнит
-- ни один runner, держало бы задачу в ожидании. Пустой каталог моделей
-- ничего не ограничивает (как проверка процесса).
UPDATE task_step_runs r SET model = ''
WHERE r.verdict IS NULL AND r.agent_kind NOT IN ('', 'rivet') AND r.model <> ''
  AND EXISTS (SELECT 1 FROM model_connections c, jsonb_array_elements(c.models) m
              WHERE c.enabled AND NOT COALESCE((m->>'hidden')::boolean, false) AND NOT COALESCE((m->>'missing')::boolean, false))
  AND NOT EXISTS (SELECT 1 FROM model_connections c, jsonb_array_elements(c.models) m
                  WHERE c.enabled AND m->>'id' = r.model
                    AND NOT COALESCE((m->>'hidden')::boolean, false) AND NOT COALESCE((m->>'missing')::boolean, false));
UPDATE task_step_runs SET
    agent_kind = CASE WHEN agent_kind IN ('', 'rivet') THEN agent_kind ELSE 'rivet' END,
    role_agent = CASE WHEN role_agent IN ('', 'rivet') THEN role_agent ELSE 'rivet' END,
    pref_agent = CASE WHEN pref_agent IN ('', 'rivet') THEN pref_agent ELSE 'rivet' END
WHERE verdict IS NULL
  AND (agent_kind NOT IN ('', 'rivet') OR role_agent NOT IN ('', 'rivet') OR pref_agent NOT IN ('', 'rivet'));

-- Сторонние профили удаляются (claude-code, codex, opencode и свои
-- профили-обёртки).
DELETE FROM agents WHERE adapter <> 'native';
ALTER TABLE agents ADD CONSTRAINT agents_adapter_check CHECK (adapter IN ('native'));

-- Runner: изолированы ли команды агента от сети (объявляет сам runner) и
-- снят ли он с работы (агент вне каталога: следующая регистрация получит
-- отказ). Runner выкатки (без агента) остаётся.
--
-- Статус снятого runner'а миграция не трогает: давний last_seen отдаёт его
-- watchdog'у (MarkStaleRunnersOffline), и первый же тик снимает runner
-- штатно, с попыткой задачи, освобождением запусков и агента команды.
-- Прямой status='offline' оставил бы задачу и открытый запуск на runner'е
-- навсегда: watchdog берёт только runner'ы не в offline, а регистрация
-- старого runner'а отклоняется до освобождения его стадий.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS network_isolated boolean NOT NULL DEFAULT false;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS retired boolean NOT NULL DEFAULT false;
UPDATE runners SET retired = true, last_seen = LEAST(last_seen, now() - interval '1 day')
WHERE agent NOT IN ('', 'rivet');

-- Usage агента Rivet: кешированный вход (чтение из кеша провайдера) и
-- запись в кеш, оба входят в tokens_in. NULL — источник не сообщил.
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS tokens_cached bigint;
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS tokens_cache_write bigint;
