-- Каскадная блокировка: ссылка на задачу-первопричину.
-- Отличает каскадный blocked (снимается автоматически, без эскалации)
-- от блокировки вопросом агента (blocked_by IS NULL + запись в attention).
ALTER TABLE tasks ADD COLUMN blocked_by uuid REFERENCES tasks(id);

-- Backfill: задачи, каскадно заблокированные старой логикой, узнаваемы по её
-- фиксированному тексту причины. Без blocked_by они были бы неотличимы от
-- human-blocked и не снялись бы автоматически.
UPDATE tasks t SET blocked_by = (
    SELECT dt.id FROM task_deps d JOIN tasks dt ON dt.id = d.dep_id
    WHERE d.task_id = t.id AND dt.status IN ('failed','cancelled')
    ORDER BY dt.num LIMIT 1
)
WHERE t.status = 'blocked'
  AND t.block_reason = 'зависимость завершилась неуспешно';

-- Остатки: каскадные блокировки, у которых прямой failed/cancelled-зависимости
-- уже нет (первопричину успели решить, старая логика не разблокировала).
-- Возвращаем в планирование, актуальное состояние выведет RecomputeEpic.
UPDATE tasks SET status = 'queued', block_reason = NULL
WHERE status = 'blocked'
  AND block_reason = 'зависимость завершилась неуспешно'
  AND blocked_by IS NULL;
