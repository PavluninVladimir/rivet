-- Прозрачность затрат (change add-cost-transparency, спеки monetization,
-- orchestration «Бюджет Epic»).

-- Бюджет Epic в токенах: NULL — без бюджета. Расход Epic считается по
-- usage_records(epic_id) — индекс usage_by_epic существует с 0001.
ALTER TABLE epics ADD COLUMN token_budget bigint;
