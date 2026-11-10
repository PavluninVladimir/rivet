-- История переходов задачи по шагам для графа процесса в деталке
-- (change fix-web-console-tails). Частичный индекс по образцу
-- events_task_phase из 0032: запрос истории берёт только события task.step
-- задачи и не читает её session.step, которых у длинной задачи тысячи.
-- Предикат совпадает с литералом в запросе Store.TaskStepHistory.
CREATE INDEX IF NOT EXISTS events_task_step ON events(task_id, id) WHERE type = 'task.step';
