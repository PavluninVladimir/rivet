-- Причина откладывания запуска (fix-agent-without-connection): серия
-- откладываний считается по причине. Смена причины (стадия не стартовала
-- по другой причине, ожидание подключения к модели) начинает новую серию:
-- пауза с начала, событие ожидания пишется заново.
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS defer_reason text NOT NULL DEFAULT '';

-- Подключение, с которым стадия ушла runner'у, и признак, что ключ или
-- секретные заголовки каталога ушли в назначении. Отказ провайдера во
-- время стадии разбирается по ним, а не по текущим настройкам: модель
-- проекта и канал runner'а могли смениться, пока стадия шла.
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS assigned_connection text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS catalog_key boolean NOT NULL DEFAULT false;
