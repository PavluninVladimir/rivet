-- Команда агентов, доработки после e2e (add-agent-roles): занятость агентов
-- в event log установки, вид причины ожидания задачи и индекс ленты аудита.

-- ─── занятость агентов ──────────────────────────────────────────────────
--
-- Поток проекта отдаёт события своего проекта и события установки из
-- RoleStreamTypes. Назначение и освобождение агента — события проекта его
-- задачи, поэтому панель «Команда» в другом проекте не видела, что агент
-- занят или свободен. События team.agent_busy и team.agent_free уровня
-- установки идут в потоки всех проектов, консоль перечитывает по ним
-- GET /team.
--
-- Агент занят, пока у него есть открытый запуск шага: agent_id задан,
-- verdict пуст (так же считает GET /team). Запуск получает и теряет агента
-- во многих местах: назначение, вердикт, отмена, откладывание, отвязка
-- недоставленной стадии, перезапуск runner'а, удаление агента. Событие
-- пишет триггер, чтобы ни один путь не остался без сигнала, в той же
-- транзакции, что и сама правка. Агент ведёт одну сессию (уникальный
-- индекс task_step_runs_agent_open), поэтому переход запуска совпадает с
-- переходом агента.
--
-- В событии только идентификатор агента и состояние. Задача, проект и Epic
-- агента видны лишь участникам проекта (GET /team), а событие получают
-- потоки всех проектов. Удалённый агент сигнала не даёт: консоль
-- перечитывает команду по team.agent_deleted.

CREATE OR REPLACE FUNCTION rivet_team_agent_work() RETURNS trigger AS $$
DECLARE
    was_agent text;
    now_agent text;
BEGIN
    IF TG_OP <> 'INSERT' AND OLD.verdict IS NULL THEN
        was_agent := OLD.agent_id;
    END IF;
    IF TG_OP <> 'DELETE' AND NEW.verdict IS NULL THEN
        now_agent := NEW.agent_id;
    END IF;
    IF was_agent IS NOT DISTINCT FROM now_agent THEN
        RETURN NULL;
    END IF;
    IF was_agent IS NOT NULL THEN
        INSERT INTO events (actor_kind, type, text, payload)
        SELECT 'scheduler', 'team.agent_free', 'агент команды «' || a.name || '» свободен',
               jsonb_build_object('id', a.id, 'busy', false)
        FROM team_agents a WHERE a.id = was_agent;
    END IF;
    IF now_agent IS NOT NULL THEN
        INSERT INTO events (actor_kind, type, text, payload)
        SELECT 'scheduler', 'team.agent_busy', 'агент команды «' || a.name || '» занят',
               jsonb_build_object('id', a.id, 'busy', true)
        FROM team_agents a WHERE a.id = now_agent;
    END IF;
    RETURN NULL;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS task_step_runs_agent_work ON task_step_runs;
CREATE TRIGGER task_step_runs_agent_work
    AFTER INSERT OR UPDATE OF agent_id, verdict OR DELETE ON task_step_runs
    FOR EACH ROW EXECUTE FUNCTION rivet_team_agent_work();

-- ─── вид причины ожидания ───────────────────────────────────────────────
--
-- wait_kind = 'team' — причину писал markTeamWaiting: «нет свободного
-- агента роли» или «нет runner'а для свободных агентов роли». Пусто —
-- прочие причины: нет runner'а для участника без команды, отложенный старт
-- стадии.
--
-- Причину снимают только назначение и вход на шаг, а откладывание старта
-- пишет свою. Задача, которая ждала агента, а потом попала под исключение
-- (бюджет, зависимости Epic'а), держала бы «нет свободного агента» и после
-- того, как агенты освободились. По виду планировщик снимает у таких задач
-- командную причину, не трогая прочие.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wait_kind text NOT NULL DEFAULT '';

-- Уже записанные командные причины узнаются по тексту.
UPDATE tasks SET wait_kind = 'team'
WHERE wait_kind = ''
  AND (wait_reason LIKE 'нет свободного агента роли «%'
       OR wait_reason LIKE 'нет runner''а для свободных агентов роли «%');

-- ─── лента аудита ───────────────────────────────────────────────────────
--
-- Лента аудита без фильтра по типу скрывает занятость агентов (store,
-- auditFeedSQL): этих событий на порядки больше, чем записей аудита, и без
-- своего индекса каждая страница листала бы их. Предикат совпадает с
-- условием запроса буква в букву.
CREATE INDEX IF NOT EXISTS events_audit ON events (id)
    WHERE project_id IS NULL AND type NOT IN ('team.agent_busy', 'team.agent_free');
