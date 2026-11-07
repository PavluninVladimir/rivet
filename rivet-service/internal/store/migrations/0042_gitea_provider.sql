-- Адаптер Gitea и Forgejo (change add-gitea-adapter, спека scm-integration
-- «Первая волна адаптеров»): провайдер gitea у проекта и его учётных данных.
-- Имена ограничений — те, что Postgres дал CHECK столбцов в 0007.
ALTER TABLE projects DROP CONSTRAINT projects_provider_check;
ALTER TABLE projects ADD CONSTRAINT projects_provider_check
    CHECK (provider IN ('github', 'gitlab', 'gitea', 'fake'));

ALTER TABLE scm_credentials DROP CONSTRAINT scm_credentials_provider_check;
ALTER TABLE scm_credentials ADD CONSTRAINT scm_credentials_provider_check
    CHECK (provider IN ('github', 'gitlab', 'gitea', 'fake'));
