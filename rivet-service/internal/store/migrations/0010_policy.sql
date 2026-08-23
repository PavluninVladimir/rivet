-- Политики конвейера пресетами (change add-policy-presets, спеки
-- access-policy, orchestration, task-pipeline).

-- Версии политик: документ пресетов установки или переопределений проекта.
-- История append-only: активная версия области — с наибольшим номером.
-- content — Presets (installation) либо Overrides (project, поля nullable),
-- hash — sha256 канонического JSON содержимого.
CREATE TABLE policy_versions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope      text NOT NULL CHECK (scope IN ('installation', 'project')),
    project_id uuid REFERENCES projects(id),
    version    int NOT NULL,
    hash       text NOT NULL,
    content    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text NOT NULL DEFAULT '',
    CHECK ((scope = 'installation' AND project_id IS NULL) OR (scope = 'project' AND project_id IS NOT NULL))
);
-- Номер версии уникален в пределах области: для установки — один ряд
-- версий, для проекта — свой.
CREATE UNIQUE INDEX policy_versions_installation ON policy_versions(version) WHERE scope = 'installation';
CREATE UNIQUE INDEX policy_versions_project ON policy_versions(project_id, version) WHERE scope = 'project';

-- Отдельный счётчик отказов review: лимит отказов review из политики
-- действует независимо от общего лимита попыток.
ALTER TABLE tasks ADD COLUMN review_rejections int NOT NULL DEFAULT 0;
