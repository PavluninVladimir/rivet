-- Пользователи, членство и credentials (change add-users-and-access).
-- Пароли — только bcrypt-хэш; секреты сессий и PAT — только SHA-256-хэш.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- login неизменяем и не переиспользуется: атрибуция в append-only event log
    -- идёт по нему (design, решение 8). Вместо удаления — деактивация.
    login         text UNIQUE NOT NULL,
    name          text NOT NULL DEFAULT '',
    password_hash text NOT NULL,
    is_admin      boolean NOT NULL DEFAULT false,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project_members (
    project_id uuid NOT NULL REFERENCES projects(id),
    user_id    uuid NOT NULL REFERENCES users(id),
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX project_members_by_user ON project_members(user_id);

-- Серверные сессии консоли: скользящий TTL, отзыв — удаление строки.
CREATE TABLE auth_sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id),
    token_hash text UNIQUE NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX auth_sessions_by_user ON auth_sessions(user_id);

-- Personal access tokens: prefix для списка, секрет показывается один раз.
CREATE TABLE access_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users(id),
    name         text NOT NULL,
    prefix       text NOT NULL,
    token_hash   text UNIQUE NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz
);
CREATE INDEX access_tokens_by_user ON access_tokens(user_id);

-- Системные события об учётных записях (user.bootstrap, user.created)
-- не привязаны к проекту: project_id становится nullable.
ALTER TABLE events ALTER COLUMN project_id DROP NOT NULL;
