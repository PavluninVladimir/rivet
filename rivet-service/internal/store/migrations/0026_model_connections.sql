-- Подключения к провайдерам, агрегаторам и локальным серверам моделей
-- (add-model-connections, спека backend/model-connections). Заменяют
-- llm_providers: произвольный идентификатор, тип API, base URL, список
-- моделей. Ключ и секретные заголовки — secretbox, наружу только префикс.
CREATE TABLE model_connections (
    id           text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name         text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('vendor', 'aggregator', 'local')),
    api          text NOT NULL CHECK (api IN ('anthropic', 'openai')),
    base_url     text NOT NULL,
    key_prefix   text NOT NULL DEFAULT '',
    key_enc      bytea,
    headers      jsonb NOT NULL DEFAULT '[]',
    models       jsonb NOT NULL DEFAULT '[]',
    enabled      boolean NOT NULL DEFAULT true,
    state        text NOT NULL DEFAULT 'unchecked' CHECK (state IN ('ok', 'invalid', 'unchecked')),
    check_detail text NOT NULL DEFAULT '',
    checked_at   timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid NOT NULL REFERENCES users(id)
);

-- Настройки установки как ключ/значение: модель планировщика
-- (planner_model = {connection_id, model}) и будущие настройки.
CREATE TABLE installation_settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES users(id)
);

-- Перенос провайдеров: ключи не расшифровываются, модель строки становится
-- ручной записью списка, активная строка — моделью планировщика.
INSERT INTO model_connections (id, name, kind, api, base_url, key_prefix, key_enc, models, state, check_detail, checked_at, updated_at, updated_by)
SELECT provider,
       CASE provider WHEN 'anthropic' THEN 'Anthropic' ELSE 'DeepSeek' END,
       'vendor',
       CASE provider WHEN 'anthropic' THEN 'anthropic' ELSE 'openai' END,
       CASE provider WHEN 'anthropic' THEN 'https://api.anthropic.com' ELSE 'https://api.deepseek.com' END,
       key_prefix, key_enc,
       jsonb_build_array(jsonb_build_object(
           'id', CASE WHEN model <> '' THEN model WHEN provider = 'anthropic' THEN 'claude-opus-5' ELSE 'deepseek-v4-flash' END,
           'label', CASE WHEN model <> '' THEN model WHEN provider = 'anthropic' THEN 'claude-opus-5' ELSE 'deepseek-v4-flash' END,
           'source', 'manual', 'hidden', false, 'missing', false)),
       state, check_detail, checked_at, updated_at, updated_by
FROM llm_providers;

INSERT INTO installation_settings (key, value, updated_at, updated_by)
SELECT 'planner_model',
       jsonb_build_object('connection_id', provider,
           'model', CASE WHEN model <> '' THEN model WHEN provider = 'anthropic' THEN 'claude-opus-5' ELSE 'deepseek-v4-flash' END),
       updated_at, updated_by
FROM llm_providers WHERE active
ORDER BY updated_at DESC LIMIT 1;

DROP TABLE llm_providers;
