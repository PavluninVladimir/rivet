-- Нативный адаптер Claude Code (change add-claude-code-adapter, спеки
-- agent-integration, runners).

-- Адаптер подключения агента и глубина данных runner'а: объявляются при
-- регистрации, сессии runner'а создаются с его глубиной.
ALTER TABLE runners ADD COLUMN adapter text NOT NULL DEFAULT 'wrap';
ALTER TABLE runners ADD COLUMN depth text NOT NULL DEFAULT 'minimal'
    CHECK (depth IN ('full', 'partial', 'minimal'));

-- Затронутые сессией файлы (пути от корня рабочей копии, уникальные).
-- NULL — недоступно для способа подключения (минимальная глубина),
-- '{}' — полная глубина, файлов пока нет («недоступно ≠ пусто»).
ALTER TABLE sessions ADD COLUMN files text[];
