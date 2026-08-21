-- Эксплуатация установки (change add-operations-management, спеки runners,
-- observability, epic-decomposition).

-- Токены регистрации runner'ов: общий секрет установки, хранится хэшем
-- (по образцу access_tokens). Отзыв — revoked_at, строка остаётся для аудита.
CREATE TABLE runner_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    prefix       text NOT NULL,
    token_hash   text UNIQUE NOT NULL,
    created_by   uuid NOT NULL REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    revoked_at   timestamptz
);

-- Каким токеном пришёл runner: для аудита регистрации; токен могли отозвать
-- после, поэтому без каскада.
ALTER TABLE runners ADD COLUMN token_id uuid REFERENCES runner_tokens(id);

-- Провайдеры модели для декомпозиции: ключ шифрованным (secretbox), наружу
-- только префикс; активный провайдер один (частичный уникальный индекс).
CREATE TABLE llm_providers (
    provider     text PRIMARY KEY CHECK (provider IN ('anthropic', 'deepseek')),
    key_prefix   text NOT NULL,
    key_enc      bytea NOT NULL,
    model        text NOT NULL DEFAULT '',
    active       boolean NOT NULL DEFAULT false,
    state        text NOT NULL DEFAULT 'unchecked' CHECK (state IN ('ok', 'invalid', 'unchecked')),
    check_detail text NOT NULL DEFAULT '',
    checked_at   timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid NOT NULL REFERENCES users(id)
);
CREATE UNIQUE INDEX llm_providers_one_active ON llm_providers ((true)) WHERE active;

-- Лента аудита установки читает события без проекта: это доли процента
-- таблицы, без частичного индекса — последовательный скан.
CREATE INDEX events_installation ON events(id) WHERE project_id IS NULL;

-- Usage за период по проекту: установочный срез и обычный запрос
-- фильтруют по ts в границах проектов.
CREATE INDEX usage_by_project_ts ON usage_records(project_id, ts);
