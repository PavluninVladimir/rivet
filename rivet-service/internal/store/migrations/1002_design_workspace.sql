-- Additive, project-scoped immutable design history. Draft content lives only in revisions.
CREATE TABLE design_documents (
 id uuid PRIMARY KEY, project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 name text NOT NULL, revision integer NOT NULL CHECK(revision>0), content_sha text NOT NULL,
 created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id,id)
);
CREATE INDEX design_documents_page ON design_documents(project_id,id);
CREATE TABLE design_revisions (
 document_id uuid NOT NULL REFERENCES design_documents(id) ON DELETE CASCADE, revision integer NOT NULL,
 parent_revision integer, model jsonb NOT NULL, content_sha text NOT NULL, actor_id uuid NOT NULL REFERENCES users(id),
 diff jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(document_id,revision)
);
CREATE TABLE design_element_ids (
 document_id uuid NOT NULL REFERENCES design_documents(id) ON DELETE CASCADE, element_id uuid NOT NULL,
 first_revision integer NOT NULL, PRIMARY KEY(document_id,element_id)
);
CREATE TABLE design_assets (
 id uuid PRIMARY KEY, project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE, sha256 text NOT NULL,
 mime text NOT NULL, size_bytes bigint NOT NULL CHECK(size_bytes>0 AND size_bytes<=10485760), object_ref text NOT NULL,
 created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(project_id,id)
);
CREATE TABLE design_threads (
 id uuid PRIMARY KEY, project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 document_id uuid NOT NULL, anchor_revision integer NOT NULL, element_id uuid NOT NULL, property text NOT NULL DEFAULT '',
 revision integer NOT NULL DEFAULT 1, status text NOT NULL DEFAULT 'open' CHECK(status IN ('open','resolved')),
 created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(project_id,document_id) REFERENCES design_documents(project_id,id) ON DELETE CASCADE,
 FOREIGN KEY(document_id,anchor_revision) REFERENCES design_revisions(document_id,revision)
);
CREATE INDEX design_threads_page ON design_threads(document_id,id);
CREATE TABLE design_comments (
 id uuid PRIMARY KEY, thread_id uuid NOT NULL REFERENCES design_threads(id) ON DELETE CASCADE,
 revision integer NOT NULL DEFAULT 1, author_id uuid NOT NULL REFERENCES users(id), text text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), edited_at timestamptz, deleted boolean NOT NULL DEFAULT false
);
CREATE TABLE design_comment_history (
 comment_id uuid NOT NULL REFERENCES design_comments(id) ON DELETE CASCADE, revision integer NOT NULL,
 text text NOT NULL, deleted boolean NOT NULL, actor_id uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(comment_id,revision)
);
CREATE TABLE design_handoffs (
 id uuid PRIMARY KEY, project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE, document_id uuid NOT NULL,
 revision integer NOT NULL, manifest jsonb NOT NULL, manifest_sha text NOT NULL,
 created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(project_id,document_id) REFERENCES design_documents(project_id,id) ON DELETE CASCADE,
 FOREIGN KEY(document_id,revision) REFERENCES design_revisions(document_id,revision)
);
CREATE TABLE design_handoff_links (
 handoff_id uuid NOT NULL REFERENCES design_handoffs(id) ON DELETE CASCADE, kind text NOT NULL, target_id text NOT NULL,
 actor_id uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(handoff_id,kind,target_id)
);
CREATE TABLE design_requests (
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE, actor_id uuid NOT NULL REFERENCES users(id),
 key text NOT NULL, request_sha text NOT NULL, response jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(project_id,actor_id,key)
);
-- Short-lived capability URLs are independently authenticated and never expose object keys.
CREATE TABLE design_asset_reads (
 token_sha text PRIMARY KEY, asset_id uuid NOT NULL REFERENCES design_assets(id) ON DELETE CASCADE,
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE, actor_id uuid NOT NULL REFERENCES users(id),
 expires_at timestamptz NOT NULL
);
CREATE INDEX design_asset_reads_expiry ON design_asset_reads(expires_at);
