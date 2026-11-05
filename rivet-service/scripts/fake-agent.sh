#!/bin/sh
# Fake-агент для e2e-стендов: играет роль CLI-агента без модели и токенов.
# Поведение управляется содержимым промпта (RIVET_PROMPT_FILE):
#   - промпт тестировщика -> тест в tests/e2e-tests.txt и VERDICT: OK
#     ([e2e-tester-changes] — CHANGES на первом проходе, [e2e-tester-code] —
#     правка src/, которую runner откатит)
#   - промпт ревьюера  -> VERDICT: APPROVED
#   - маркер задания describe -> эталонное описание архитектуры (fake-describe.sh)
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
*"проверяешь задачу"*)
  # Тестировщик (add-agent-roles): тест на критерии в тестовом каталоге.
  echo "Пишу тесты по критериям приёмки."
  mkdir -p tests
  printf 'test %s\n' "$(date +%s)" >> tests/e2e-tests.txt
  usage
  case "$PROMPT" in
  *"[e2e-tester-code]"*)
    mkdir -p src
    printf 'tester %s\n' "$(date +%s)" >> src/tester-touch.txt
    echo "VERDICT: OK"
    ;;
  *"[e2e-tester-changes]"*)
    # Первый проход разработчика оставил одну строку результата: критерий
    # не выполнен; после исправления строк две — всё в порядке.
    if [ "$(wc -l < e2e-result.txt 2>/dev/null || echo 0)" -ge 2 ]; then
      echo "VERDICT: OK"
    else
      echo "VERDICT: CHANGES: e2e-тестировщик: критерий 1 не выполнен, повторите реализацию"
    fi
    ;;
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
*"[rivet:architecture-describe]"*)
  # Задача описания архитектуры (add-architecture-c4): эталонное описание
  # демо-CRM. Маркер проверяется после ревьюера: его промпт тоже содержит
  # текст задания.
  echo "Описываю архитектуру проекта."
  sh "$(dirname "$0")/fake-describe.sh"
  usage
  echo "Готово."
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

# Цикл сопровождения (add-sdlc-cycle): метка [e2e-break] ломает проверку
# наблюдения стенда, задача-баг (её критерий — проверка наблюдения
# окружения) чинит её. Сбой пишется словом fail: в «broken» есть подстрока
# «ok», и проверка `grep -q ok` его бы пропустила (проверка — grep -qx ok).
case "$PROMPT" in
*"[e2e-break]"*) echo "fail" > health.txt ;;
*"Проверка наблюдения окружения"*) echo "ok" > health.txt ;;
esac

echo "Реализую задачу..."
# «Секрет» в выводе: e2e проверяет, что в сохранённом транскрипте он
# замаскирован (спека team-visibility «Секрет в транскрипте»).
echo "export GH_TOKEN=ghp_e2eFakeSecret0123456789"
printf 'work %s\n' "$(date +%s)" >> e2e-result.txt
usage
echo "Готово."
