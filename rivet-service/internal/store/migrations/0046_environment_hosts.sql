-- Хост окружения от провайдера инфраструктуры (change
-- add-environment-provisioning, спека deployment «Хост окружения от
-- провайдера инфраструктуры», «Подключения к провайдерам инфраструктуры»).

-- Подключение к провайдеру инфраструктуры: токен: secretbox, наружу
-- только префикс (как у подключений моделей). Адрес API задаёт установка.
CREATE TABLE infra_connections (
    id               text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    provider         text NOT NULL CHECK (provider IN ('ruvds')),
    name             text NOT NULL,
    token_prefix     text NOT NULL DEFAULT '',
    token_enc        bytea,
    enabled          boolean NOT NULL DEFAULT true,
    state            text NOT NULL DEFAULT 'unchecked' CHECK (state IN ('ok', 'invalid', 'unchecked')),
    check_detail     text NOT NULL DEFAULT '',
    checked_at       timestamptz,
    balance_amount   double precision,
    balance_currency text NOT NULL DEFAULT '',
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid NOT NULL REFERENCES users(id)
);

-- Хост окружения: одна строка на попытку получить хост. spec: снимок
-- привязки окружения на момент запроса. Переходы: CAS по state.
CREATE TABLE env_hosts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    env_id              uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    provider            text NOT NULL CHECK (provider IN ('local', 'ruvds')),
    connection_id       text REFERENCES infra_connections(id),
    spec                jsonb NOT NULL DEFAULT '{}',
    -- названия дата-центра, тарифа и ОС из справочника провайдера на
    -- момент расчёта цены: решение человек читает без справочника
    spec_labels         jsonb NOT NULL DEFAULT '{}',
    state               text NOT NULL CHECK (state IN ('quoting', 'awaiting_approval', 'approved',
                            'creating', 'bootstrapping', 'ready', 'delete_requested', 'deleting',
                            'deleted', 'rejected', 'failed')),
    -- состояние до запроса удаления: отмена возвращает его
    prev_state          text NOT NULL DEFAULT '',
    requested_by        text NOT NULL DEFAULT '',
    -- цена и баланс на момент расчёта
    quote_amount        double precision,
    quote_currency      text NOT NULL DEFAULT '',
    quote_period        integer NOT NULL DEFAULT 0,
    balance_amount      double precision,
    balance_currency    text NOT NULL DEFAULT '',
    -- решение человека о создании
    approved_by         text NOT NULL DEFAULT '',
    approved_at         timestamptz,
    approved_amount     double precision,
    -- создание отправлено провайдеру (исход мог потеряться)
    create_sent_at      timestamptz,
    server_id           text NOT NULL DEFAULT '',
    action_id           text NOT NULL DEFAULT '',
    address             text NOT NULL DEFAULT '',
    host_key            text NOT NULL DEFAULT '',
    waiting_payment     boolean NOT NULL DEFAULT false,
    port                integer NOT NULL DEFAULT 0,
    -- задание deploy-runner'а (bootstrap или снятие)
    job_id              text NOT NULL DEFAULT '',
    job_kind            text NOT NULL DEFAULT '',
    job_runner          text NOT NULL DEFAULT '',
    job_started_at      timestamptz,
    attempts            integer NOT NULL DEFAULT 0,
    needs_retry         boolean NOT NULL DEFAULT false,
    -- удаление
    remove_volumes      boolean NOT NULL DEFAULT false,
    delete_requested_by text NOT NULL DEFAULT '',
    delete_confirmed_by text NOT NULL DEFAULT '',
    delete_sent_at      timestamptz,
    -- стоимость продления и дата оплаты
    paid_till           timestamptz,
    cost_amount         double precision,
    cost_currency       text NOT NULL DEFAULT '',
    cost_period         integer NOT NULL DEFAULT 0,
    cost_checked_at     timestamptz,
    failure             text NOT NULL DEFAULT '',
    detail              text NOT NULL DEFAULT '',
    -- тик: когда хост пора обработать и чей захват действует
    next_attempt_at     timestamptz NOT NULL DEFAULT now(),
    lease_until         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    ended_at            timestamptz,
    -- Деньги и необратимые действия только по решению человека: правило
    -- держит и база, а не только код (design, решения 5 и 8).
    CONSTRAINT env_hosts_create_approved CHECK (create_sent_at IS NULL OR approved_by <> ''),
    CONSTRAINT env_hosts_delete_confirmed CHECK (delete_sent_at IS NULL OR delete_confirmed_by <> '')
);

-- Живой (не в конечном состоянии) хост у окружения один.
CREATE UNIQUE INDEX env_hosts_one_live ON env_hosts(env_id)
    WHERE state NOT IN ('deleted', 'rejected', 'failed');
CREATE INDEX env_hosts_due ON env_hosts(next_attempt_at)
    WHERE state NOT IN ('deleted', 'rejected', 'failed');
CREATE INDEX env_hosts_by_env ON env_hosts(env_id, created_at DESC);

-- Эскалация хоста: ни задачи, ни публикации, предмет: хост.
ALTER TABLE attention ADD COLUMN host_id uuid REFERENCES env_hosts(id) ON DELETE CASCADE;
ALTER TABLE attention ADD CONSTRAINT attention_host_subject
    CHECK (host_id IS NULL OR (task_id IS NULL AND deployment_id IS NULL));

-- CHECK предмета эскалации расширяется, а не переписывается: параллельные
-- change'и добавляют в него свои причины уровня проекта, и полный текст
-- здесь потерял бы их (design, решение 12).
DO $$
DECLARE def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint
    WHERE conname = 'attention_subject' AND conrelid = 'attention'::regclass;
    IF def IS NULL OR def NOT LIKE 'CHECK %' THEN
        RAISE EXCEPTION 'attention_subject: неожиданное определение %', def;
    END IF;
    EXECUTE 'ALTER TABLE attention DROP CONSTRAINT attention_subject';
    EXECUTE 'ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK ('
        || substr(def, 7)
        || ' OR (task_id IS NULL AND deployment_id IS NULL AND host_id IS NOT NULL))';
END $$;

-- «Одна эскалация уровня проекта на причину» не касается хостов: два
-- окружения проекта ждут решения одновременно. У хоста: одна на причину.
DROP INDEX attention_open_project_reason;
CREATE UNIQUE INDEX attention_open_project_reason ON attention(project_id, reason)
    WHERE status <> 'resolved' AND task_id IS NULL AND deployment_id IS NULL AND host_id IS NULL;
CREATE UNIQUE INDEX attention_open_host_reason ON attention(host_id, reason)
    WHERE status <> 'resolved' AND host_id IS NOT NULL;
