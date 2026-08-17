-- Подключение репозитория к проекту (change add-repo-onboarding, спека
-- backend/scm-integration). Хостинг перестаёт быть глобальной настройкой:
-- провайдер, инстанс и учётные данные живут у проекта.

-- Учётные данные хостинга: токен хранится шифротекстом AES-GCM, ключ —
-- в окружении rivetd. Наружу отдаются только owner и token_prefix.
CREATE TABLE scm_credentials (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider     text NOT NULL CHECK (provider IN ('github','gitlab','fake')),
    base_url     text NOT NULL,
    owner        text NOT NULL DEFAULT '',   -- владелец учётной записи токена
    token_prefix text NOT NULL DEFAULT '',   -- первые символы, чтобы узнать токен
    token_enc    bytea NOT NULL,             -- nonce || шифротекст
    state        text NOT NULL DEFAULT 'unchecked'
                 CHECK (state IN ('ok','invalid','unchecked')),
    checked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE projects
    ADD COLUMN provider       text NOT NULL DEFAULT 'github'
                              CHECK (provider IN ('github','gitlab','fake')),
    ADD COLUMN base_url       text NOT NULL DEFAULT 'https://github.com',
    ADD COLUMN repo_path      text NOT NULL DEFAULT '',
    ADD COLUMN default_branch text NOT NULL DEFAULT 'main',
    -- секрет подписи webhook на проект; NULL — проверяем секретом установки
    ADD COLUMN webhook_secret text,
    ADD COLUMN webhook_registered boolean NOT NULL DEFAULT false,
    ADD COLUMN credential_id  uuid REFERENCES scm_credentials(id) ON DELETE SET NULL;

-- Backfill: до этого изменения все проекты жили на github.com под глобальным
-- токеном установки, repo хранил owner/name.
UPDATE projects SET repo_path = repo WHERE repo_path = '';
ALTER TABLE projects DROP COLUMN repo;

-- repo_path обязателен: проекта без подключённого репозитория не бывает
-- (спека domain-model «Репозиторий проекта»).
ALTER TABLE projects ADD CONSTRAINT projects_repo_path_present CHECK (repo_path <> '');
CREATE INDEX projects_by_repo ON projects(provider, repo_path);
