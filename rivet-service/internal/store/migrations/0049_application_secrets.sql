CREATE TABLE environment_secrets (
    env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name text NOT NULL,
    value_enc bytea NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (env_id, name)
);

CREATE TABLE local_publication_secrets (
    publication_id uuid NOT NULL REFERENCES local_publications(id) ON DELETE CASCADE,
    name text NOT NULL,
    value_enc bytea NOT NULL,
    PRIMARY KEY (publication_id, name)
);
