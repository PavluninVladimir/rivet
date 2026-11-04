-- Роли агентов (add-agent-roles, спека backend/agent-roles): шаблоны ролей
-- с инструкциями, цветом, capability и моделью по умолчанию; команда
-- именованных агентов, собранная из шаблонов; роль и агент в запусках шага;
-- имя runner'а. Модель Тимлида хранится в прежней настройке planner_model
-- (на неё завязаны проверки ссылок подключений), поэтому в строке lead и у
-- агентов-тимлидов модель не заполняется.
CREATE TABLE agent_roles (
    id            text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{1,31}$'),
    title         text NOT NULL,
    description   text NOT NULL DEFAULT '',
    instruction   text NOT NULL DEFAULT '',
    color         text NOT NULL DEFAULT 'slate',
    capability    text NOT NULL DEFAULT '',
    agent         text NOT NULL DEFAULT '',
    default_model jsonb,
    enabled       boolean NOT NULL DEFAULT true,
    builtin       boolean NOT NULL DEFAULT false,
    position      integer NOT NULL DEFAULT 100,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid REFERENCES users(id)
);

INSERT INTO agent_roles (id, title, description, instruction, color, capability, builtin, position) VALUES
  ('analyst', 'Аналитик',
   'Разбирает интервью пользователей и превращает их в бэклог Epic''ов.',
   'Ты бизнес-аналитик. Разбирай интервью пользователей: выделяй персоны, их потребности и боли и подкрепляй каждый вывод цитатой из интервью. Формулируй Epic''и с целью, обоснованием и приоритетом. Не придумывай того, чего нет в интервью: если данных не хватает, так и напиши.',
   'indigo', '', true, 5),
  ('architect', 'Архитектор',
   'Описывает текущую и целевую архитектуру системы, держит её согласованной.',
   'Ты отвечаешь за архитектуру системы. Изучи код и устройство проекта, прежде чем что-то описывать, и называй компоненты так же, как они называются в коде. Описание архитектуры веди в формате Rivet в каталоге docs/architecture и проверяй его командами проверки. Отмечай разрывы между текущим и целевым состоянием и не пиши продуктовый код.',
   'violet', 'explore', true, 10),
  ('lead', 'Тимлид',
   'Разбивает Epic на задачи, определяет зависимости и критерии приёмки.',
   'Ты тимлид команды. Разбей цель Epic на небольшие задачи, каждая из которых доводит одну функцию до конца: логика, тесты, разметка и подключение. Указывай зависимость только там, где задача действительно не может начаться раньше другой, чтобы независимые задачи шли параллельно. Критерии приёмки формулируй так, чтобы их можно было проверить тестом или глазами за минуту.',
   'amber', '', true, 20),
  ('developer', 'Разработчик',
   'Реализует задачу целиком: код, тесты на свою логику, подключение.',
   'Ты разработчик. Реализуй задачу целиком и аккуратно, следуя устройству и стилю проекта, и не трогай то, что к задаче не относится. Покрой свою логику тестами. Перед завершением запусти проверки проекта и убедись, что они проходят.',
   'blue', 'coding', true, 30),
  ('tester', 'Тестировщик',
   'Проверяет задачу по каждому критерию приёмки и пишет на это тесты.',
   'Ты тестировщик. Проверь, что задача выполнена по каждому критерию приёмки, и напиши или дополни автоматические тесты так, чтобы каждый критерий был проверен. Меняй только тесты: продуктовый код не трогай. Если критерий не выполняется из-за кода, опиши проблему и шаги воспроизведения в вердикте.',
   'teal', 'coding', true, 40),
  ('reviewer', 'Ревьюер',
   'Независимо проверяет изменения перед merge.',
   'Ты независимый ревьюер. Читай diff как опытный коллега: корректность, соответствие критериям приёмки, ошибки на границах, безопасность, читаемость и лишние изменения. Замечания давай конкретные, с файлом и причиной. Одобряй только то, что готово к merge.',
   'rose', 'review', true, 50);

-- Команда агентов: именованные агенты из шаблонов. Пустые agent,
-- connection_id и model — как в шаблоне роли; instruction_extra дополняет
-- инструкцию шаблона. Агент ведёт одну сессию одновременно.
CREATE TABLE team_agents (
    id                text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name              text NOT NULL,
    role_id           text NOT NULL REFERENCES agent_roles(id),
    agent             text NOT NULL DEFAULT '',
    connection_id     text NOT NULL DEFAULT '',
    model             text NOT NULL DEFAULT '',
    instruction_extra text NOT NULL DEFAULT '',
    enabled           boolean NOT NULL DEFAULT true,
    position          integer NOT NULL DEFAULT 100,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    updated_by        uuid REFERENCES users(id)
);

INSERT INTO team_agents (id, name, role_id, position) VALUES
  ('vera', 'Вера', 'analyst', 10),
  ('ada', 'Ада', 'architect', 20),
  ('timur', 'Тимур', 'lead', 30),
  ('maks', 'Макс', 'developer', 40),
  ('eva', 'Ева', 'developer', 50),
  ('lev', 'Лев', 'tester', 60),
  ('irina', 'Ирина', 'reviewer', 70);

-- Имя runner'а (инфраструктура): из регистрации (флаг -name) или из
-- консоли (locked — регистрация его не перезаписывает); registered_name —
-- последнее имя из регистрации, к нему возвращает снятие имени из консоли.
-- Features — возможности сверх версии протокола ("roles").
ALTER TABLE runners ADD COLUMN IF NOT EXISTS display_name text NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS display_name_locked boolean NOT NULL DEFAULT false;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS registered_name text NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS features text[] NOT NULL DEFAULT '{}';
-- Идентификатор поколения стадий runner'а (новый на каждом подключении):
-- смена при регистрации значит, что открытые стадии runner'а потеряны.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS boot_id text NOT NULL DEFAULT '';
-- Причина offline: undelivered — назначение не доставлено (снимает любой
-- признак жизни), stale — пропали heartbeat'ы (снимает только новый канал:
-- прежние стадии runner'а уже сняты и назначены другим).
ALTER TABLE runners ADD COLUMN IF NOT EXISTS offline_reason text NOT NULL DEFAULT '';

-- Роль участника запуска шага, предпочтительная модель роли (переопределение
-- проекта или модель шаблона: предпочтение при выборе runner'а, а не
-- требование) и агент команды, который ведёт запуск. Открытый запуск у
-- агента один: агент ведёт одну сессию одновременно.
-- role_agent — профиль шаблона: обязателен, только если у агента команды
-- своего профиля нет (или команды у роли нет); pref_agent и pref_model —
-- переопределение проекта: предпочтение при выборе runner'а.
-- agent_verdict — вердикт, который вернул сам агент (тестировщик): по нему
-- подписывается исход шага.
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS role_agent text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS pref_agent text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS pref_model text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS agent_verdict text NOT NULL DEFAULT '';
-- assigned_model — модель, с которой стадия ушла runner'у (агента команды,
-- роли или профиля); model — только явная модель участника, жёсткое
-- требование. Отвязка запуска снимает assigned_model, иначе модель прежнего
-- агента стала бы требованием к следующему. not_before — запуск не
-- назначается раньше (стадия не смогла стартовать: повтор с паузой).
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS assigned_model text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS not_before timestamptz;
-- defer_count — сколько раз подряд стадия не смогла стартовать: пауза
-- растёт (30 с, 60 с, 120 с, до 5 мин), доставка сбрасывает серию.
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS defer_count int NOT NULL DEFAULT 0;
-- До add-agent-roles модель назначения писалась в model. У открытых
-- запусков она переезжает в assigned_model, если участник снимка процесса
-- задачи модель не задаёт: иначе после отвязки она стала бы требованием.
UPDATE task_step_runs r SET assigned_model = r.model, model = ''
FROM tasks t
WHERE t.id = r.task_id AND t.process IS NOT NULL
  AND r.verdict IS NULL AND r.model <> '' AND r.assigned_model = ''
  AND NOT EXISTS (
    SELECT 1
    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(t.process->'steps') = 'array'
                                   THEN t.process->'steps' ELSE '[]'::jsonb END) st,
         jsonb_array_elements(CASE WHEN jsonb_typeof(st->'participants') = 'array'
                                   THEN st->'participants' ELSE '[]'::jsonb END) p
    WHERE st->>'id' = r.step_id AND p->>'id' = r.participant
      AND COALESCE(p->'agent'->>'model', '') <> '');
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS agent_id text REFERENCES team_agents(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX IF NOT EXISTS task_step_runs_agent_open
    ON task_step_runs (agent_id) WHERE verdict IS NULL AND agent_id IS NOT NULL;
-- Текущая стадия runner'а (DTO runner'а, список задач).
CREATE INDEX IF NOT EXISTS task_step_runs_runner_open
    ON task_step_runs (runner_id) WHERE verdict IS NULL AND runner_id IS NOT NULL;
