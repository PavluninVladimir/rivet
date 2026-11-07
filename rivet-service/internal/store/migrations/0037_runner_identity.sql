-- Привязка токенов регистрации к идентичности runner'ов (change
-- add-runner-identity, спека runners «Токены регистрации runner'ов»).

-- Для каких runner'ов выпущен токен. Токен без строк привязывается к
-- первому runner'у, который с ним зарегистрируется (source first_use).
-- issue: список при выпуске, migration: перенос общих токенов при
-- обновлении. Внешнего ключа на runners нет: токен выпускают до первой
-- регистрации runner'а.
CREATE TABLE runner_token_bindings (
    token_id  uuid NOT NULL REFERENCES runner_tokens(id) ON DELETE CASCADE,
    runner_id text NOT NULL,
    source    text NOT NULL CHECK (source IN ('issue', 'first_use', 'migration')),
    bound_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (token_id, runner_id)
);

-- Вопрос «к каким действующим токенам привязан runner» при привязке
-- токена на первой регистрации.
CREATE INDEX runner_token_bindings_runner ON runner_token_bindings(runner_id);

-- Общие токены до этого change'а: привязка к runner'ам, которые уже
-- регистрировались с ними. Эти runner'ы подключаются прежней командой,
-- новый идентификатор по такому токену не проходит. runners.token_id
-- хранит только токен последней регистрации, поэтому прежние токены
-- runner'а берутся ещё из журнала: событие runner.registered пишет
-- token_id с add-operations-management. Событие уровня установки, отсюда
-- project_id IS NULL (частичный индекс events_installation).
INSERT INTO runner_token_bindings (token_id, runner_id, source)
SELECT token_id, id, 'migration' FROM runners WHERE token_id IS NOT NULL;

INSERT INTO runner_token_bindings (token_id, runner_id, source)
SELECT DISTINCT t.id, e.actor_id, 'migration'
FROM events e JOIN runner_tokens t ON t.id::text = e.payload->>'token_id'
WHERE e.project_id IS NULL AND e.type = 'runner.registered' AND e.actor_id <> ''
ON CONFLICT (token_id, runner_id) DO NOTHING;
