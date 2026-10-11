#!/bin/sh
# Fake-агент для e2e-стендов: играет роль CLI-агента без модели и токенов.
# Поведение управляется содержимым промпта (RIVET_PROMPT_FILE):
#   - промпт ревьюера  -> VERDICT: APPROVED
#   - метка [e2e-block] без ответа человека -> BLOCKED: вопрос
#   - иначе -> правит файл в рабочей копии (коммитит и пушит runner)
set -eu
PROMPT=$(cat "$RIVET_PROMPT_FILE")

# Профиль агента (add-agent-profiles): модель и окружение из назначения
# видны в транскрипте, секреты runner маскирует сам.
echo "AGENT_MODEL=${RIVET_MODEL:-} FAKE_BASE_URL=${FAKE_BASE_URL:-} FAKE_KEY=${FAKE_KEY:-} ARGS=$*"

# Отчёт о расходе (спека agent-integration «Отчёт usage через универсальную
# обёртку»): даёт консоли ненулевые usage-цифры в e2e.
usage() {
  echo 'USAGE: {"tokens_in": 1200, "tokens_out": 340, "cost_usd": 0.042, "ctx_pct": 37}'
}

case "$PROMPT" in
*"Задание шага процесса"*)
  # Шаг prompt (add-process-editor): по метке [e2e-changes] в задании —
  # замечания, иначе выполнено.
  echo "Выполняю задание шага."
  usage
  case "$PROMPT" in
  *"[e2e-changes]"*) echo "VERDICT: CHANGES: e2e-агент просит поправить"; ;;
  *) echo "VERDICT: OK"; ;;
  esac
  exit 0
  ;;
*"независимый ревьюер"*|*"VERDICT"*)
  echo "Просмотрел изменения, замечаний нет."
  usage
  echo "VERDICT: APPROVED"
  exit 0
  ;;
esac

case "$PROMPT" in
*"[e2e-block]"*)
  case "$PROMPT" in
  *"Уточнение человека"*) ;; # ответ получен, работаем дальше
  *)
    echo "Не могу однозначно понять требование."
    echo "BLOCKED: вопрос от e2e-агента: подтвердите поведение"
    exit 0
    ;;
  esac
  ;;
esac

echo "Реализую задачу..."
# «Секрет» в выводе: e2e проверяет, что в сохранённом транскрипте он
# замаскирован (спека team-visibility «Секрет в транскрипте»).
echo "export GH_TOKEN=ghp_e2eFakeSecret0123456789"
printf 'work %s\n' "$(date +%s)" >> e2e-result.txt
usage
echo "Готово."
