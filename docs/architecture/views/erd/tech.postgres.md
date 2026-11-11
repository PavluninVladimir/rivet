<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные: PostgreSQL (`tech.postgres`)

Объекты данных хранилища и их связи. [Общая схема](all.md).

```mermaid
erDiagram
  n_data_access_token["Токен доступа"] {
    uuid id PK
    timestamptz created_at
    timestamptz expires_at "может быть пустым"
    timestamptz last_used_at "может быть пустым"
    text name
    text prefix
    text token_hash UK
    uuid user_id FK "→ data.user.id"
  }
  n_data_agent["Агент"] {
    text id PK
    text adapter
    jsonb args
    text capabilities "тип: text[]"
    text command
    jsonb default_model "может быть пустым"
    boolean enabled
    jsonb env
    jsonb limits
    jsonb models
    text name
    boolean preset
    text secrets
    timestamptz updated_at
    uuid updated_by FK "→ data.user.id, может быть пустым"
  }
  n_data_agent_role["Роль агента"] {
    text id PK
    text agent
    boolean builtin
    text capability
    text color
    jsonb default_model "может быть пустым"
    text description
    boolean enabled
    text instruction
    integer position
    text title
    timestamptz updated_at
    uuid updated_by FK "→ data.user.id, может быть пустым"
  }
  n_data_attention["Запрос внимания"] {
    uuid id PK
    text claimed_by "может быть пустым"
    text code
    timestamptz created_at
    uuid deployment_id FK "→ data.deployment.id, может быть пустым"
    text message
    uuid project_id FK "→ data.project.id"
    text reason
    timestamptz resolved_at "может быть пустым"
    text status "open, claimed, resolved"
    uuid task_id FK "→ data.task.id, может быть пустым"
  }
  n_data_auth_session["Сессия входа"] {
    uuid id PK
    timestamptz created_at
    timestamptz expires_at
    text token_hash UK
    uuid user_id FK "→ data.user.id"
  }
  n_data_deployment["Публикация"] {
    uuid id PK
    timestamptz created_at
    text detail
    timestamptz ended_at "может быть пустым"
    uuid env_id FK "→ data.environment.id"
    text external_run_id
    text external_url
    integer incident_attempts
    boolean incident_pending
    timestamptz incident_retry_at "может быть пустым"
    text initiator "auto, manual, rollback"
    text log_ref "может быть пустым"
    boolean rollback
    text runner_id
    timestamptz started_at "может быть пустым"
    text status "queued, running, done, failed"
    text version
    timestamptz version_at
  }
  n_data_discovery_interview["Интервью и отзыв"] {
    uuid id PK
    integer chars
    timestamptz created_at
    uuid created_by FK "→ data.user.id, может быть пустым"
    timestamptz deleted_at "может быть пустым"
    date held_at "может быть пустым"
    text kind "interview, feedback"
    integer lines
    uuid project_id FK "→ data.project.id"
    text respondent_role
    text sha256
    text text
    text title
  }
  n_data_discovery_run["Запуск исследования"] {
    uuid id PK
    text agent_id
    text agent_name
    timestamptz approved_at "может быть пустым"
    uuid approved_by FK "→ data.user.id, может быть пустым"
    jsonb approved_epics "может быть пустым"
    jsonb backlog "может быть пустым"
    integer backlog_rev
    text connection_id
    numeric_12_6 cost_usd "тип: numeric(12,6), может быть пустым"
    jsonb decompose
    jsonb defects
    jsonb dropped
    integer duration_s
    text error
    integer feedback_synthesized "может быть пустым"
    integer feedback_total "может быть пустым"
    integer feedback_without_findings "может быть пустым"
    timestamptz finished_at "может быть пустым"
    text instruction_sha
    jsonb interviews
    text model
    boolean outdated
    integer progress_done
    integer progress_total
    uuid project_id FK "→ data.project.id"
    jsonb result "может быть пустым"
    text role_title
    text stage "summaries, synthesis, backlog"
    timestamptz started_at
    text status
    bigint tokens_in "может быть пустым"
    bigint tokens_out "может быть пустым"
    integer version
  }
  n_data_discovery_summary["Конспект исследования"] {
    text instruction_sha PK
    text interview_sha256 PK
    text model PK
    timestamptz created_at
    jsonb dropped
    jsonb summary
  }
  n_data_environment["Окружение"] {
    uuid id PK
    jsonb config
    timestamptz created_at
    text exec_type "ssh, script, k8s"
    text name UK
    boolean paused
    uuid project_id FK "→ data.project.id"
    text runner_caps "тип: text[]"
    text trigger "manual, auto, merge"
  }
  n_data_epic["Epic"] {
    uuid id PK
    timestamptz created_at
    uuid discovery_run_id FK "→ data.discovery-run.id, может быть пустым"
    timestamptz done_at "может быть пустым"
    timestamptz due_at "может быть пустым"
    text goal
    text kind "work, architecture, discovery"
    text priority "может быть пустым"
    uuid project_id FK "→ data.project.id"
    text source_key "может быть пустым"
    timestamptz started_at "может быть пустым"
    text status "planned, active, blocked, done, cancelled"
    text title
    bigint token_budget "может быть пустым"
  }
  n_data_epic_dep["Зависимость Epic'ов"] {
    uuid depends_on PK, FK "→ data.epic.id"
    uuid epic_id PK, FK "→ data.epic.id"
  }
  n_data_epic_need["Потребность Epic"] {
    uuid epic_id PK, FK "→ data.epic.id"
    text need_id PK
    uuid run_id PK, FK "→ data.discovery-run.id"
    jsonb need
  }
  n_data_event["Событие журнала"] {
    bigserial id PK
    text actor_id
    text actor_kind
    uuid epic_id FK "→ data.epic.id, может быть пустым"
    jsonb payload
    uuid project_id FK "→ data.project.id, может быть пустым"
    uuid task_id FK "→ data.task.id, может быть пустым"
    text text
    timestamptz ts
    text type
  }
  n_data_incident["Инцидент"] {
    uuid id PK
    text bug_cancelled_by
    boolean bug_pending
    uuid bug_task_id FK "→ data.task.id, может быть пустым"
    uuid deployment_id FK "→ data.deployment.id, может быть пустым"
    jsonb description
    text description_agent_id FK "→ data.team-agent.id, может быть пустым"
    integer description_attempts
    timestamptz description_requested_at
    text description_status
    uuid env_id FK "→ data.environment.id"
    text error_line
    jsonb evidence
    text fingerprint
    timestamptz first_seen
    timestamptz last_seen
    timestamptz mitigated_at "может быть пустым"
    uuid mitigated_release_id FK "→ data.release.id, может быть пустым"
    boolean needs_human
    integer occurrences
    integer occurrences_at_mitigation
    uuid project_id FK "→ data.project.id"
    text resolution
    timestamptz resolved_at "может быть пустым"
    text severity
    boolean skipped_daily
    text source
    text status "open, mitigated, resolved, dismissed"
    text title
  }
  n_data_incident_event["Событие инцидента"] {
    bigserial id PK
    text actor
    timestamptz at
    uuid incident_id FK "→ data.incident.id"
    text note
    text status
  }
  n_data_installation_setting["Настройка установки"] {
    text key PK
    timestamptz updated_at
    uuid updated_by FK "→ data.user.id, может быть пустым"
    jsonb value
  }
  n_data_model_connection["Подключение модели"] {
    text id PK
    text api
    text base_url
    text check_detail
    timestamptz checked_at "может быть пустым"
    boolean enabled
    jsonb headers
    bytea key_enc "может быть пустым"
    text key_prefix
    text kind "openai, anthropic, deepseek, openrouter, ollama, lmstudio"
    jsonb models
    text name
    text state
    timestamptz updated_at
    uuid updated_by FK "→ data.user.id"
  }
  n_data_monitor_run["Проверка наблюдения"] {
    bigserial id PK
    timestamptz at
    integer duration_ms
    uuid env_id FK "→ data.environment.id"
    boolean ok
    text output
  }
  n_data_monitor_state["Состояние наблюдения"] {
    uuid env_id PK, FK "→ data.environment.id"
    integer consecutive_fail
    integer consecutive_ok
    timestamptz last_at "может быть пустым"
    timestamptz last_change_at
    text last_output
    timestamptz next_at
    text pending_check_id
    text pending_runner
    timestamptz pending_since "может быть пустым"
    text reason
    timestamptz recheck_at "может быть пустым"
    text state "unknown, ok, fail"
  }
  n_data_policy_version["Версия политики"] {
    uuid id PK
    jsonb content
    timestamptz created_at
    text created_by
    text hash
    uuid project_id FK "→ data.project.id, может быть пустым"
    text scope
    integer version
  }
  n_data_project["Проект"] {
    uuid id PK
    timestamptz architecture_auto_at "может быть пустым"
    text base_url
    jsonb checks "массив проверок политики проекта"
    timestamptz created_at
    uuid credential_id FK "→ data.scm-credential.id, может быть пустым"
    text default_branch
    text name "имя репозитория в консоли"
    text policy_file_id
    text policy_source "store или file"
    text provider "github, gitlab или gitea"
    text repo_path "owner/repo или путь в инстансе"
    integer webhook_events
    boolean webhook_registered
    timestamptz webhook_registered_at "может быть пустым"
    text webhook_secret "может быть пустым"
  }
  n_data_project_architecture["Снимок архитектуры"] {
    uuid project_id PK, FK "→ data.project.id"
    jsonb data "может быть пустым"
    jsonb defects
    text error
    text error_kind
    timestamptz read_at "может быть пустым"
    text sha
    text status
    text tree_sha
    timestamptz updated_at
    jsonb valid_data "может быть пустым"
    timestamptz valid_read_at "может быть пустым"
    text valid_sha
  }
  n_data_project_member["Участник проекта"] {
    uuid project_id PK, FK "→ data.project.id"
    uuid user_id PK, FK "→ data.user.id"
    timestamptz added_at
    text role
  }
  n_data_release["Релиз"] {
    uuid id PK
    boolean by_time
    jsonb composition
    timestamptz created_at
    uuid deployment_id FK, UK "→ data.deployment.id"
    integer dropped_items
    uuid env_id FK "→ data.environment.id"
    text initiator
    text notes_agent_id FK "→ data.team-agent.id, может быть пустым"
    integer notes_attempts
    text notes_md
    text notes_model
    timestamptz notes_requested_at
    text notes_status "pending, ready, failed"
    text prev_version
    uuid project_id FK "→ data.project.id"
    text version
  }
  n_data_runner["Runner"] {
    text id PK
    text adapter
    text agent
    text boot_id
    text capabilities "тип: text[]"
    boolean catalog
    boolean context_channel
    integer ctx_pct "может быть пустым"
    text declared_capabilities "тип: text[]"
    text declared_models "тип: text[]"
    uuid deployment_id FK "→ data.deployment.id, может быть пустым"
    text depth
    text display_name
    boolean display_name_locked
    boolean draining
    text features "тип: text[]"
    text host
    timestamptz last_seen
    text model
    text models "тип: text[]"
    boolean network_isolated
    text offline_reason
    text protocol
    text registered_name
    boolean retired
    boolean secure
    text stages "тип: text[]"
    text status "idle, busy, draining, offline"
    uuid task_id FK "→ data.task.id, может быть пустым"
    uuid token_id FK "→ data.runner-token.id, может быть пустым"
  }
  n_data_runner_token["Токен runner'а"] {
    uuid id PK
    timestamptz created_at
    uuid created_by FK "→ data.user.id"
    timestamptz expires_at "может быть пустым"
    timestamptz last_used_at "может быть пустым"
    text name
    text prefix
    timestamptz revoked_at "может быть пустым"
    text token_hash UK
  }
  n_data_runner_token_binding["Привязка токена runner'а"] {
    text runner_id PK
    uuid token_id PK, FK "→ data.runner-token.id"
    timestamptz bound_at
    text source
  }
  n_data_scm_credential["Учётные данные SCM"] {
    uuid id PK
    text base_url
    timestamptz checked_at "может быть пустым"
    timestamptz created_at
    text owner
    text provider
    text state "unchecked, ok, fail"
    bytea token_enc
    text token_prefix
  }
  n_data_session["Сессия агента"] {
    uuid id PK
    text agent
    integer attempt "может быть пустым"
    text depth
    text driver_id
    text driver_kind
    timestamptz ended_at "может быть пустым"
    text files "тип: text[], может быть пустым"
    text last_step
    text model
    text outcome
    text participant
    text policy_hash
    boolean private
    text prompt
    text scope "может быть пустым"
    timestamptz started_at
    text step_id
    uuid task_id FK "→ data.task.id, может быть пустым"
    bigint tokens "может быть пустым"
    text transcript_ref "ссылка s3:// на транскрипт, может быть пустым"
  }
  n_data_task["Задача"] {
    uuid id PK
    integer attempt_limit
    integer attempt_used
    text block_reason "может быть пустым"
    uuid blocked_by FK "→ data.task.id, может быть пустым"
    text branch "может быть пустым"
    text capabilities "тип: text[]"
    text checked_sha
    timestamptz created_at
    jsonb criteria
    text description
    timestamptz done_at "может быть пустым"
    uuid epic_id FK "→ data.epic.id"
    integer estimate
    uuid incident_id FK "→ data.incident.id, может быть пустым"
    text kind "work, architecture, review, delivery, monitor"
    timestamptz merged_at "может быть пустым"
    bigint num UK "номер задачи в рамках Epic"
    text pr_url "может быть пустым"
    jsonb process "может быть пустым"
    text process_hash
    integer review_rejections
    text reviewer_id "может быть пустым"
    text runner_id "может быть пустым"
    integer source_ord "может быть пустым"
    timestamptz started_at "может быть пустым"
    text status "queued, running, review, done, blocked, cancelled"
    integer step_closed_gen
    text step_entry
    integer step_gen
    text step_id
    jsonb step_rejections
    text title
    timestamptz updated_at
    text wait_kind
    text wait_reason
    timestamptz wait_since "может быть пустым"
  }
  n_data_task_dep["Зависимость задач"] {
    uuid dep_id PK, FK "→ data.task.id"
    uuid task_id PK, FK "→ data.task.id"
  }
  n_data_task_step_run["Этап задачи"] {
    bigserial id PK
    text agent_id FK "→ data.team-agent.id, может быть пустым"
    text agent_kind
    text agent_verdict
    text assigned_model
    text capabilities "тип: text[]"
    timestamptz created_at
    integer defer_count
    timestamptz deferred_since "может быть пустым"
    text detail
    timestamptz finished_at "может быть пустым"
    text model
    timestamptz not_before "может быть пустым"
    text participant "имя роли-участника"
    integer pass
    text pref_agent
    text pref_model
    text role
    text role_agent
    text runner_id FK "→ data.runner.id, может быть пустым"
    uuid session_id "может быть пустым"
    text step_id
    text step_kind "code, test, review, merge"
    uuid task_id FK "→ data.task.id"
    text user_login
    text user_role
    text verdict "может быть пустым"
    text verdict_by
  }
  n_data_team_agent["Агент команды"] {
    text id PK
    text agent
    text connection_id
    boolean enabled
    text instruction_extra
    text model
    text name
    integer position
    text role_id FK "→ data.agent-role.id"
    timestamptz updated_at
    uuid updated_by FK "→ data.user.id, может быть пустым"
  }
  n_data_usage_record["Запись использования модели"] {
    bigserial id PK
    numeric_12_6 cost_usd "тип: numeric(12,6), может быть пустым"
    integer duration_s
    uuid epic_id FK "→ data.epic.id, может быть пустым"
    text model
    uuid project_id FK "→ data.project.id"
    text role "может быть пустым"
    text runner_id "может быть пустым"
    uuid session_id "может быть пустым"
    text source_msg_id UK "может быть пустым"
    uuid task_id FK "→ data.task.id, может быть пустым"
    bigint tokens_cache_write "может быть пустым"
    bigint tokens_cached "может быть пустым"
    bigint tokens_in "может быть пустым"
    bigint tokens_out "может быть пустым"
    timestamptz ts
  }
  n_data_user["Пользователь"] {
    uuid id PK
    timestamptz created_at
    boolean disabled
    boolean is_admin
    text login UK
    boolean must_change_password
    text name
    text password_hash
  }
  n_data_access_token }o--|| n_data_user : "user_id"
  n_data_agent }o--o| n_data_user : "updated_by"
  n_data_agent_role }o--o| n_data_user : "updated_by"
  n_data_attention }o--o| n_data_deployment : "deployment_id"
  n_data_attention }o--|| n_data_project : "project_id"
  n_data_attention }o--o| n_data_task : "task_id"
  n_data_auth_session }o--|| n_data_user : "user_id"
  n_data_deployment }o--|| n_data_environment : "env_id"
  n_data_discovery_interview }o--|| n_data_project : "project_id"
  n_data_discovery_interview }o--o| n_data_user : "created_by"
  n_data_discovery_run }o--|| n_data_project : "project_id"
  n_data_discovery_run }o--o| n_data_user : "approved_by"
  n_data_environment }o--|| n_data_project : "project_id"
  n_data_epic }o--o| n_data_discovery_run : "discovery_run_id"
  n_data_epic }o--|| n_data_project : "project_id"
  n_data_epic_dep }o--o{ n_data_epic : "зависит от"
  n_data_epic_need }o--|| n_data_discovery_run : "run_id"
  n_data_epic_need }o--o{ n_data_epic : "потребности"
  n_data_event }o--o| n_data_epic : "epic_id"
  n_data_event }o--o| n_data_project : "project_id"
  n_data_event }o--o| n_data_task : "task_id"
  n_data_incident }o--o| n_data_deployment : "deployment_id"
  n_data_incident }o--|| n_data_environment : "env_id"
  n_data_incident }o--|| n_data_project : "project_id"
  n_data_incident }o--o| n_data_release : "mitigated_release_id"
  n_data_incident }o--o| n_data_task : "bug_task_id"
  n_data_incident }o--o| n_data_team_agent : "description_agent_id"
  n_data_incident_event }o--|| n_data_incident : "incident_id"
  n_data_installation_setting }o--o| n_data_user : "updated_by"
  n_data_model_connection }o--|| n_data_user : "updated_by"
  n_data_monitor_run }o--|| n_data_environment : "env_id"
  n_data_monitor_state }o--|| n_data_environment : "env_id"
  n_data_policy_version }o--o| n_data_project : "project_id"
  n_data_project }o--o| n_data_scm_credential : "credential_id"
  n_data_project_architecture }o--|| n_data_project : "project_id"
  n_data_project_member }o--|| n_data_project : "project_id"
  n_data_project_member }o--o{ n_data_user : "участники"
  n_data_release }o--|| n_data_deployment : "deployment_id"
  n_data_release }o--|| n_data_environment : "env_id"
  n_data_release }o--|| n_data_project : "project_id"
  n_data_release }o--o| n_data_team_agent : "notes_agent_id"
  n_data_runner }o--o| n_data_deployment : "deployment_id"
  n_data_runner }o--o| n_data_runner_token : "token_id"
  n_data_runner }o--o| n_data_task : "task_id"
  n_data_runner_token }o--|| n_data_user : "created_by"
  n_data_runner_token_binding }o--|| n_data_runner_token : "token_id"
  n_data_session }o--o| n_data_task : "task_id"
  n_data_task }o--|| n_data_epic : "epic_id"
  n_data_task }o--o| n_data_incident : "incident_id"
  n_data_task }o--o| n_data_task : "blocked_by"
  n_data_task_dep }o--o{ n_data_task : "зависит от"
  n_data_task_step_run }o--o| n_data_runner : "runner_id"
  n_data_task_step_run }o--|| n_data_task : "task_id"
  n_data_task_step_run }o--o| n_data_team_agent : "agent_id"
  n_data_team_agent }o--|| n_data_agent_role : "role_id"
  n_data_team_agent }o--o| n_data_user : "updated_by"
  n_data_usage_record }o--o| n_data_epic : "epic_id"
  n_data_usage_record }o--|| n_data_project : "project_id"
  n_data_usage_record }o--o| n_data_task : "task_id"
```

## Токен доступа (`data.access-token`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `expires_at` | timestamptz |  | да |  |
| `last_used_at` | timestamptz |  | да |  |
| `name` | text |  | нет |  |
| `prefix` | text |  | нет |  |
| `token_hash` | text | UK | нет |  |
| `user_id` | uuid | FK → `data.user.id` | нет |  |

## Агент (`data.agent`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | text | PK | нет |  |
| `adapter` | text |  | нет |  |
| `args` | jsonb |  | нет |  |
| `capabilities` | text[] |  | нет |  |
| `command` | text |  | нет |  |
| `default_model` | jsonb |  | да |  |
| `enabled` | boolean |  | нет |  |
| `env` | jsonb |  | нет |  |
| `limits` | jsonb |  | нет |  |
| `models` | jsonb |  | нет |  |
| `name` | text |  | нет |  |
| `preset` | boolean |  | нет |  |
| `secrets` | text |  | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `updated_by` | uuid | FK → `data.user.id` | да |  |

## Роль агента (`data.agent-role`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | text | PK | нет |  |
| `agent` | text |  | нет |  |
| `builtin` | boolean |  | нет |  |
| `capability` | text |  | нет |  |
| `color` | text |  | нет |  |
| `default_model` | jsonb |  | да |  |
| `description` | text |  | нет |  |
| `enabled` | boolean |  | нет |  |
| `instruction` | text |  | нет |  |
| `position` | integer |  | нет |  |
| `title` | text |  | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `updated_by` | uuid | FK → `data.user.id` | да |  |

## Запрос внимания (`data.attention`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `claimed_by` | text |  | да |  |
| `code` | text |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `deployment_id` | uuid | FK → `data.deployment.id` | да |  |
| `message` | text |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `reason` | text |  | нет |  |
| `resolved_at` | timestamptz |  | да |  |
| `status` | text |  | нет | open, claimed, resolved |
| `task_id` | uuid | FK → `data.task.id` | да |  |

## Сессия входа (`data.auth-session`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `expires_at` | timestamptz |  | нет |  |
| `token_hash` | text | UK | нет |  |
| `user_id` | uuid | FK → `data.user.id` | нет |  |

## Публикация (`data.deployment`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `detail` | text |  | нет |  |
| `ended_at` | timestamptz |  | да |  |
| `env_id` | uuid | FK → `data.environment.id` | нет |  |
| `external_run_id` | text |  | нет |  |
| `external_url` | text |  | нет |  |
| `incident_attempts` | integer |  | нет |  |
| `incident_pending` | boolean |  | нет |  |
| `incident_retry_at` | timestamptz |  | да |  |
| `initiator` | text |  | нет | auto, manual, rollback |
| `log_ref` | text |  | да |  |
| `rollback` | boolean |  | нет |  |
| `runner_id` | text |  | нет |  |
| `started_at` | timestamptz |  | да |  |
| `status` | text |  | нет | queued, running, done, failed |
| `version` | text |  | нет |  |
| `version_at` | timestamptz |  | нет |  |

## Интервью и отзыв (`data.discovery-interview`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `chars` | integer |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `created_by` | uuid | FK → `data.user.id` | да |  |
| `deleted_at` | timestamptz |  | да |  |
| `held_at` | date |  | да |  |
| `kind` | text |  | нет | interview, feedback |
| `lines` | integer |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `respondent_role` | text |  | нет |  |
| `sha256` | text |  | нет |  |
| `text` | text |  | нет |  |
| `title` | text |  | нет |  |

## Запуск исследования (`data.discovery-run`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `agent_id` | text |  | нет |  |
| `agent_name` | text |  | нет |  |
| `approved_at` | timestamptz |  | да |  |
| `approved_by` | uuid | FK → `data.user.id` | да |  |
| `approved_epics` | jsonb |  | да |  |
| `backlog` | jsonb |  | да |  |
| `backlog_rev` | integer |  | нет |  |
| `connection_id` | text |  | нет |  |
| `cost_usd` | numeric(12,6) |  | да |  |
| `decompose` | jsonb |  | нет |  |
| `defects` | jsonb |  | нет |  |
| `dropped` | jsonb |  | нет |  |
| `duration_s` | integer |  | нет |  |
| `error` | text |  | нет |  |
| `feedback_synthesized` | integer |  | да |  |
| `feedback_total` | integer |  | да |  |
| `feedback_without_findings` | integer |  | да |  |
| `finished_at` | timestamptz |  | да |  |
| `instruction_sha` | text |  | нет |  |
| `interviews` | jsonb |  | нет |  |
| `model` | text |  | нет |  |
| `outdated` | boolean |  | нет |  |
| `progress_done` | integer |  | нет |  |
| `progress_total` | integer |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `result` | jsonb |  | да |  |
| `role_title` | text |  | нет |  |
| `stage` | text |  | нет | summaries, synthesis, backlog |
| `started_at` | timestamptz |  | нет |  |
| `status` | text |  | нет |  |
| `tokens_in` | bigint |  | да |  |
| `tokens_out` | bigint |  | да |  |
| `version` | integer |  | нет |  |

## Конспект исследования (`data.discovery-summary`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `instruction_sha` | text | PK | нет |  |
| `interview_sha256` | text | PK | нет |  |
| `model` | text | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `dropped` | jsonb |  | нет |  |
| `summary` | jsonb |  | нет |  |

## Окружение (`data.environment`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `config` | jsonb |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `exec_type` | text |  | нет | ssh, script, k8s |
| `name` | text | UK | нет |  |
| `paused` | boolean |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `runner_caps` | text[] |  | нет |  |
| `trigger` | text |  | нет | manual, auto, merge |

## Epic (`data.epic`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `discovery_run_id` | uuid | FK → `data.discovery-run.id` | да |  |
| `done_at` | timestamptz |  | да |  |
| `due_at` | timestamptz |  | да |  |
| `goal` | text |  | нет |  |
| `kind` | text |  | нет | work, architecture, discovery |
| `priority` | text |  | да |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `source_key` | text |  | да |  |
| `started_at` | timestamptz |  | да |  |
| `status` | text |  | нет | planned, active, blocked, done, cancelled |
| `title` | text |  | нет |  |
| `token_budget` | bigint |  | да |  |

## Зависимость Epic'ов (`data.epic-dep`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `depends_on` | uuid | PK, FK → `data.epic.id` | нет |  |
| `epic_id` | uuid | PK, FK → `data.epic.id` | нет |  |

## Потребность Epic (`data.epic-need`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `epic_id` | uuid | PK, FK → `data.epic.id` | нет |  |
| `need_id` | text | PK | нет |  |
| `run_id` | uuid | PK, FK → `data.discovery-run.id` | нет |  |
| `need` | jsonb |  | нет |  |

## Событие журнала (`data.event`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | bigserial | PK | нет |  |
| `actor_id` | text |  | нет |  |
| `actor_kind` | text |  | нет |  |
| `epic_id` | uuid | FK → `data.epic.id` | да |  |
| `payload` | jsonb |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | да |  |
| `task_id` | uuid | FK → `data.task.id` | да |  |
| `text` | text |  | нет |  |
| `ts` | timestamptz |  | нет |  |
| `type` | text |  | нет |  |

## Инцидент (`data.incident`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `bug_cancelled_by` | text |  | нет |  |
| `bug_pending` | boolean |  | нет |  |
| `bug_task_id` | uuid | FK → `data.task.id` | да |  |
| `deployment_id` | uuid | FK → `data.deployment.id` | да |  |
| `description` | jsonb |  | нет |  |
| `description_agent_id` | text | FK → `data.team-agent.id` | да |  |
| `description_attempts` | integer |  | нет |  |
| `description_requested_at` | timestamptz |  | нет |  |
| `description_status` | text |  | нет |  |
| `env_id` | uuid | FK → `data.environment.id` | нет |  |
| `error_line` | text |  | нет |  |
| `evidence` | jsonb |  | нет |  |
| `fingerprint` | text |  | нет |  |
| `first_seen` | timestamptz |  | нет |  |
| `last_seen` | timestamptz |  | нет |  |
| `mitigated_at` | timestamptz |  | да |  |
| `mitigated_release_id` | uuid | FK → `data.release.id` | да |  |
| `needs_human` | boolean |  | нет |  |
| `occurrences` | integer |  | нет |  |
| `occurrences_at_mitigation` | integer |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `resolution` | text |  | нет |  |
| `resolved_at` | timestamptz |  | да |  |
| `severity` | text |  | нет |  |
| `skipped_daily` | boolean |  | нет |  |
| `source` | text |  | нет |  |
| `status` | text |  | нет | open, mitigated, resolved, dismissed |
| `title` | text |  | нет |  |

## Событие инцидента (`data.incident-event`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | bigserial | PK | нет |  |
| `actor` | text |  | нет |  |
| `at` | timestamptz |  | нет |  |
| `incident_id` | uuid | FK → `data.incident.id` | нет |  |
| `note` | text |  | нет |  |
| `status` | text |  | нет |  |

## Настройка установки (`data.installation-setting`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `key` | text | PK | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `updated_by` | uuid | FK → `data.user.id` | да |  |
| `value` | jsonb |  | нет |  |

## Подключение модели (`data.model-connection`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | text | PK | нет |  |
| `api` | text |  | нет |  |
| `base_url` | text |  | нет |  |
| `check_detail` | text |  | нет |  |
| `checked_at` | timestamptz |  | да |  |
| `enabled` | boolean |  | нет |  |
| `headers` | jsonb |  | нет |  |
| `key_enc` | bytea |  | да |  |
| `key_prefix` | text |  | нет |  |
| `kind` | text |  | нет | openai, anthropic, deepseek, openrouter, ollama, lmstudio |
| `models` | jsonb |  | нет |  |
| `name` | text |  | нет |  |
| `state` | text |  | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `updated_by` | uuid | FK → `data.user.id` | нет |  |

## Проверка наблюдения (`data.monitor-run`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | bigserial | PK | нет |  |
| `at` | timestamptz |  | нет |  |
| `duration_ms` | integer |  | нет |  |
| `env_id` | uuid | FK → `data.environment.id` | нет |  |
| `ok` | boolean |  | нет |  |
| `output` | text |  | нет |  |

## Состояние наблюдения (`data.monitor-state`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `env_id` | uuid | PK, FK → `data.environment.id` | нет |  |
| `consecutive_fail` | integer |  | нет |  |
| `consecutive_ok` | integer |  | нет |  |
| `last_at` | timestamptz |  | да |  |
| `last_change_at` | timestamptz |  | нет |  |
| `last_output` | text |  | нет |  |
| `next_at` | timestamptz |  | нет |  |
| `pending_check_id` | text |  | нет |  |
| `pending_runner` | text |  | нет |  |
| `pending_since` | timestamptz |  | да |  |
| `reason` | text |  | нет |  |
| `recheck_at` | timestamptz |  | да |  |
| `state` | text |  | нет | unknown, ok, fail |

## Версия политики (`data.policy-version`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `content` | jsonb |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `created_by` | text |  | нет |  |
| `hash` | text |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | да |  |
| `scope` | text |  | нет |  |
| `version` | integer |  | нет |  |

## Проект (`data.project`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `architecture_auto_at` | timestamptz |  | да |  |
| `base_url` | text |  | нет |  |
| `checks` | jsonb |  | нет | массив проверок политики проекта |
| `created_at` | timestamptz |  | нет |  |
| `credential_id` | uuid | FK → `data.scm-credential.id` | да |  |
| `default_branch` | text |  | нет |  |
| `name` | text |  | нет | имя репозитория в консоли |
| `policy_file_id` | text |  | нет |  |
| `policy_source` | text |  | нет | store или file |
| `provider` | text |  | нет | github, gitlab или gitea |
| `repo_path` | text |  | нет | owner/repo или путь в инстансе |
| `webhook_events` | integer |  | нет |  |
| `webhook_registered` | boolean |  | нет |  |
| `webhook_registered_at` | timestamptz |  | да |  |
| `webhook_secret` | text |  | да |  |

## Снимок архитектуры (`data.project-architecture`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `project_id` | uuid | PK, FK → `data.project.id` | нет |  |
| `data` | jsonb |  | да |  |
| `defects` | jsonb |  | нет |  |
| `error` | text |  | нет |  |
| `error_kind` | text |  | нет |  |
| `read_at` | timestamptz |  | да |  |
| `sha` | text |  | нет |  |
| `status` | text |  | нет |  |
| `tree_sha` | text |  | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `valid_data` | jsonb |  | да |  |
| `valid_read_at` | timestamptz |  | да |  |
| `valid_sha` | text |  | нет |  |

## Участник проекта (`data.project-member`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `project_id` | uuid | PK, FK → `data.project.id` | нет |  |
| `user_id` | uuid | PK, FK → `data.user.id` | нет |  |
| `added_at` | timestamptz |  | нет |  |
| `role` | text |  | нет |  |

## Релиз (`data.release`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `by_time` | boolean |  | нет |  |
| `composition` | jsonb |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `deployment_id` | uuid | FK → `data.deployment.id`, UK | нет |  |
| `dropped_items` | integer |  | нет |  |
| `env_id` | uuid | FK → `data.environment.id` | нет |  |
| `initiator` | text |  | нет |  |
| `notes_agent_id` | text | FK → `data.team-agent.id` | да |  |
| `notes_attempts` | integer |  | нет |  |
| `notes_md` | text |  | нет |  |
| `notes_model` | text |  | нет |  |
| `notes_requested_at` | timestamptz |  | нет |  |
| `notes_status` | text |  | нет | pending, ready, failed |
| `prev_version` | text |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `version` | text |  | нет |  |

## Runner (`data.runner`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | text | PK | нет |  |
| `adapter` | text |  | нет |  |
| `agent` | text |  | нет |  |
| `boot_id` | text |  | нет |  |
| `capabilities` | text[] |  | нет |  |
| `catalog` | boolean |  | нет |  |
| `context_channel` | boolean |  | нет |  |
| `ctx_pct` | integer |  | да |  |
| `declared_capabilities` | text[] |  | нет |  |
| `declared_models` | text[] |  | нет |  |
| `deployment_id` | uuid | FK → `data.deployment.id` | да |  |
| `depth` | text |  | нет |  |
| `display_name` | text |  | нет |  |
| `display_name_locked` | boolean |  | нет |  |
| `draining` | boolean |  | нет |  |
| `features` | text[] |  | нет |  |
| `host` | text |  | нет |  |
| `last_seen` | timestamptz |  | нет |  |
| `model` | text |  | нет |  |
| `models` | text[] |  | нет |  |
| `network_isolated` | boolean |  | нет |  |
| `offline_reason` | text |  | нет |  |
| `protocol` | text |  | нет |  |
| `registered_name` | text |  | нет |  |
| `retired` | boolean |  | нет |  |
| `secure` | boolean |  | нет |  |
| `stages` | text[] |  | нет |  |
| `status` | text |  | нет | idle, busy, draining, offline |
| `task_id` | uuid | FK → `data.task.id` | да |  |
| `token_id` | uuid | FK → `data.runner-token.id` | да |  |

## Токен runner'а (`data.runner-token`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `created_by` | uuid | FK → `data.user.id` | нет |  |
| `expires_at` | timestamptz |  | да |  |
| `last_used_at` | timestamptz |  | да |  |
| `name` | text |  | нет |  |
| `prefix` | text |  | нет |  |
| `revoked_at` | timestamptz |  | да |  |
| `token_hash` | text | UK | нет |  |

## Привязка токена runner'а (`data.runner-token-binding`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `runner_id` | text | PK | нет |  |
| `token_id` | uuid | PK, FK → `data.runner-token.id` | нет |  |
| `bound_at` | timestamptz |  | нет |  |
| `source` | text |  | нет |  |

## Учётные данные SCM (`data.scm-credential`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `base_url` | text |  | нет |  |
| `checked_at` | timestamptz |  | да |  |
| `created_at` | timestamptz |  | нет |  |
| `owner` | text |  | нет |  |
| `provider` | text |  | нет |  |
| `state` | text |  | нет | unchecked, ok, fail |
| `token_enc` | bytea |  | нет |  |
| `token_prefix` | text |  | нет |  |

## Сессия агента (`data.session`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `agent` | text |  | нет |  |
| `attempt` | integer |  | да |  |
| `depth` | text |  | нет |  |
| `driver_id` | text |  | нет |  |
| `driver_kind` | text |  | нет |  |
| `ended_at` | timestamptz |  | да |  |
| `files` | text[] |  | да |  |
| `last_step` | text |  | нет |  |
| `model` | text |  | нет |  |
| `outcome` | text |  | нет |  |
| `participant` | text |  | нет |  |
| `policy_hash` | text |  | нет |  |
| `private` | boolean |  | нет |  |
| `prompt` | text |  | нет |  |
| `scope` | text |  | да |  |
| `started_at` | timestamptz |  | нет |  |
| `step_id` | text |  | нет |  |
| `task_id` | uuid | FK → `data.task.id` | да |  |
| `tokens` | bigint |  | да |  |
| `transcript_ref` | text |  | да | ссылка s3:// на транскрипт |

## Задача (`data.task`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `attempt_limit` | integer |  | нет |  |
| `attempt_used` | integer |  | нет |  |
| `block_reason` | text |  | да |  |
| `blocked_by` | uuid | FK → `data.task.id` | да |  |
| `branch` | text |  | да |  |
| `capabilities` | text[] |  | нет |  |
| `checked_sha` | text |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `criteria` | jsonb |  | нет |  |
| `description` | text |  | нет |  |
| `done_at` | timestamptz |  | да |  |
| `epic_id` | uuid | FK → `data.epic.id` | нет |  |
| `estimate` | integer |  | нет |  |
| `incident_id` | uuid | FK → `data.incident.id` | да |  |
| `kind` | text |  | нет | work, architecture, review, delivery, monitor |
| `merged_at` | timestamptz |  | да |  |
| `num` | bigint | UK | нет | номер задачи в рамках Epic |
| `pr_url` | text |  | да |  |
| `process` | jsonb |  | да |  |
| `process_hash` | text |  | нет |  |
| `review_rejections` | integer |  | нет |  |
| `reviewer_id` | text |  | да |  |
| `runner_id` | text |  | да |  |
| `source_ord` | integer |  | да |  |
| `started_at` | timestamptz |  | да |  |
| `status` | text |  | нет | queued, running, review, done, blocked, cancelled |
| `step_closed_gen` | integer |  | нет |  |
| `step_entry` | text |  | нет |  |
| `step_gen` | integer |  | нет |  |
| `step_id` | text |  | нет |  |
| `step_rejections` | jsonb |  | нет |  |
| `title` | text |  | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `wait_kind` | text |  | нет |  |
| `wait_reason` | text |  | нет |  |
| `wait_since` | timestamptz |  | да |  |

## Зависимость задач (`data.task-dep`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `dep_id` | uuid | PK, FK → `data.task.id` | нет |  |
| `task_id` | uuid | PK, FK → `data.task.id` | нет |  |

## Этап задачи (`data.task-step-run`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | bigserial | PK | нет |  |
| `agent_id` | text | FK → `data.team-agent.id` | да |  |
| `agent_kind` | text |  | нет |  |
| `agent_verdict` | text |  | нет |  |
| `assigned_model` | text |  | нет |  |
| `capabilities` | text[] |  | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `defer_count` | integer |  | нет |  |
| `deferred_since` | timestamptz |  | да |  |
| `detail` | text |  | нет |  |
| `finished_at` | timestamptz |  | да |  |
| `model` | text |  | нет |  |
| `not_before` | timestamptz |  | да |  |
| `participant` | text |  | нет | имя роли-участника |
| `pass` | integer |  | нет |  |
| `pref_agent` | text |  | нет |  |
| `pref_model` | text |  | нет |  |
| `role` | text |  | нет |  |
| `role_agent` | text |  | нет |  |
| `runner_id` | text | FK → `data.runner.id` | да |  |
| `session_id` | uuid |  | да |  |
| `step_id` | text |  | нет |  |
| `step_kind` | text |  | нет | code, test, review, merge |
| `task_id` | uuid | FK → `data.task.id` | нет |  |
| `user_login` | text |  | нет |  |
| `user_role` | text |  | нет |  |
| `verdict` | text |  | да |  |
| `verdict_by` | text |  | нет |  |

## Агент команды (`data.team-agent`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | text | PK | нет |  |
| `agent` | text |  | нет |  |
| `connection_id` | text |  | нет |  |
| `enabled` | boolean |  | нет |  |
| `instruction_extra` | text |  | нет |  |
| `model` | text |  | нет |  |
| `name` | text |  | нет |  |
| `position` | integer |  | нет |  |
| `role_id` | text | FK → `data.agent-role.id` | нет |  |
| `updated_at` | timestamptz |  | нет |  |
| `updated_by` | uuid | FK → `data.user.id` | да |  |

## Запись использования модели (`data.usage-record`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | bigserial | PK | нет |  |
| `cost_usd` | numeric(12,6) |  | да |  |
| `duration_s` | integer |  | нет |  |
| `epic_id` | uuid | FK → `data.epic.id` | да |  |
| `model` | text |  | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `role` | text |  | да |  |
| `runner_id` | text |  | да |  |
| `session_id` | uuid |  | да |  |
| `source_msg_id` | text | UK | да |  |
| `task_id` | uuid | FK → `data.task.id` | да |  |
| `tokens_cache_write` | bigint |  | да |  |
| `tokens_cached` | bigint |  | да |  |
| `tokens_in` | bigint |  | да |  |
| `tokens_out` | bigint |  | да |  |
| `ts` | timestamptz |  | нет |  |

## Пользователь (`data.user`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `created_at` | timestamptz |  | нет |  |
| `disabled` | boolean |  | нет |  |
| `is_admin` | boolean |  | нет |  |
| `login` | text | UK | нет |  |
| `must_change_password` | boolean |  | нет |  |
| `name` | text |  | нет |  |
| `password_hash` | text |  | нет |  |
