#!/bin/sh
# Fake-агент для e2e-стендов: играет роль CLI-агента без модели и токенов.
# Поведение управляется содержимым промпта (RIVET_PROMPT_FILE):
#   - промпт ревьюера  -> VERDICT: APPROVED
#   - метка [e2e-block] без ответа человека -> BLOCKED: вопрос
#   - иначе -> правит файл в рабочей копии (коммитит и пушит runner)
set -eu
PROMPT=$(cat "$RIVET_PROMPT_FILE")

case "$PROMPT" in
*"независимый ревьюер"*|*"VERDICT"*)
  echo "Просмотрел изменения, замечаний нет."
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
printf 'work %s\n' "$(date +%s)" >> e2e-result.txt
echo "Готово."
