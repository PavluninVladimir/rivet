-- Сопровождение передаёт GitHub Issues в существующее исследование.
CREATE TABLE project_issue_intake (
    project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    enabled boolean NOT NULL DEFAULT false,
    auto_research boolean NOT NULL DEFAULT false,
    label text NOT NULL DEFAULT 'rivet',
    revision bigint NOT NULL DEFAULT 1,
    next_poll_at timestamptz NOT NULL DEFAULT now(),
    lease_token uuid,
    lease_until timestamptz,
    next_page int NOT NULL DEFAULT 1 CHECK (next_page > 0),
    etag text NOT NULL DEFAULT '',
    last_check_at timestamptz,
    error text NOT NULL DEFAULT ''
);
CREATE TABLE github_issue_sources (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    repo text NOT NULL,
    number bigint NOT NULL CHECK (number > 0),
    url text NOT NULL,
    title text NOT NULL,
    material_id uuid NOT NULL REFERENCES discovery_interviews(id),
    research_run_id uuid REFERENCES discovery_runs(id),
    attempts int NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, repo, number)
);
CREATE INDEX github_issue_sources_pending ON github_issue_sources(project_id, created_at);
