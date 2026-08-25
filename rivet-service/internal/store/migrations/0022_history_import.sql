-- Импорт истории проекта (change add-history-import, спека domain-model
-- «Импорт истории проекта»).

-- Ключ источника у Epic'а: импортированная история узнаётся по нему и не
-- дублируется при повторном импорте. Живые Epic'и ключа не имеют.
ALTER TABLE epics ADD COLUMN source_key text;
CREATE UNIQUE INDEX epics_source_key ON epics(project_id, source_key) WHERE source_key IS NOT NULL;

-- Порядковый номер задачи в манифесте: по нему повторный импорт обновляет
-- задачу, а не создаёт новую.
ALTER TABLE tasks ADD COLUMN source_ord int;
CREATE UNIQUE INDEX tasks_source_ord ON tasks(epic_id, source_ord) WHERE source_ord IS NOT NULL;
