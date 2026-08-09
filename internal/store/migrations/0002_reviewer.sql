-- Review выполняет отдельный runner (≠ исполнитель): трекинг ревьюера.
ALTER TABLE tasks ADD COLUMN reviewer_id text;
