-- Редактор процесса (change add-process-editor): runner объявляет стадии,
-- которые исполняет; шаг prompt назначается только runner'ам с PROMPT.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS stages text[] NOT NULL DEFAULT '{}';
