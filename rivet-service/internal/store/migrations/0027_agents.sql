-- Каталог агентов (add-agent-profiles, спека backend/agents): кто и как
-- запускает модель. Привязки моделей — пары подключение/модель из каталога
-- подключений, шаблон окружения с подстановками, режим доставки секретов.
CREATE TABLE agents (
    id            text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    name          text NOT NULL,
    adapter       text NOT NULL CHECK (adapter IN ('claude-code', 'wrap')),
    command       text NOT NULL DEFAULT '',
    capabilities  text[] NOT NULL DEFAULT '{}',
    models        jsonb NOT NULL DEFAULT '[]',
    default_model jsonb,
    env           jsonb NOT NULL DEFAULT '[]',
    args          jsonb NOT NULL DEFAULT '[]',
    secrets       text NOT NULL DEFAULT 'secure' CHECK (secrets IN ('never', 'secure', 'always')),
    enabled       boolean NOT NULL DEFAULT true,
    preset        boolean NOT NULL DEFAULT false,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid REFERENCES users(id)
);

-- Runner: агент из каталога или вне его, защищённость канала, объявленные
-- при регистрации модели (возвращаются, если профиль отключён или удалён).
ALTER TABLE runners ADD COLUMN IF NOT EXISTS catalog boolean NOT NULL DEFAULT false;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS secure boolean NOT NULL DEFAULT false;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS declared_models text[] NOT NULL DEFAULT '{}';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS declared_capabilities text[] NOT NULL DEFAULT '{}';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS protocol text NOT NULL DEFAULT '';
UPDATE runners SET declared_models = models, declared_capabilities = capabilities;

-- Предустановленные профили с готовыми шаблонами окружения.
INSERT INTO agents (id, name, adapter, command, capabilities, env, args, preset) VALUES
  ('claude-code', 'Claude Code', 'claude-code', '', '{}',
   '[{"name":"ANTHROPIC_API_KEY","value":"{{key}}"},{"name":"ANTHROPIC_BASE_URL","value":"{{base_url}}"}]', '[]', true),
  ('codex', 'Codex', 'wrap', 'codex exec --skip-git-repo-check "$(cat "$RIVET_PROMPT_FILE")"', '{}',
   '[{"name":"OPENAI_API_KEY","value":"{{key}}"},{"name":"OPENAI_BASE_URL","value":"{{base_url}}"}]', '["-m","{{model}}"]', true),
  ('opencode', 'OpenCode', 'wrap', 'opencode run "$(cat "$RIVET_PROMPT_FILE")"', '{}',
   '[{"name":"OPENAI_API_KEY","value":"{{key}}"},{"name":"OPENAI_BASE_URL","value":"{{base_url}}"}]', '["--model","{{model}}"]', true);

-- Существующие стенды: если есть подключение anthropic с моделями,
-- claude-code получает их привязками, первая — по умолчанию.
UPDATE agents a SET
  models = (SELECT COALESCE(jsonb_agg(jsonb_build_object('connection_id', 'anthropic', 'model', m->>'id')), '[]')
            FROM model_connections c, jsonb_array_elements(c.models) m
            WHERE c.id = 'anthropic' AND NOT COALESCE((m->>'hidden')::boolean, false) AND NOT COALESCE((m->>'missing')::boolean, false)),
  default_model = (SELECT jsonb_build_object('connection_id', 'anthropic', 'model', m->>'id')
                   FROM model_connections c, jsonb_array_elements(c.models) m
                   WHERE c.id = 'anthropic' AND NOT COALESCE((m->>'hidden')::boolean, false) AND NOT COALESCE((m->>'missing')::boolean, false) LIMIT 1)
WHERE a.id = 'claude-code' AND EXISTS (SELECT 1 FROM model_connections WHERE id = 'anthropic');

-- Runner'ы с агентом из каталога: модели из привязок, если они есть.
UPDATE runners r SET catalog = true,
  models = CASE WHEN jsonb_array_length(a.models) > 0
                THEN (SELECT array_agg(DISTINCT m->>'model') FROM jsonb_array_elements(a.models) m)
                ELSE r.models END
FROM agents a WHERE a.id = r.agent AND a.enabled;
