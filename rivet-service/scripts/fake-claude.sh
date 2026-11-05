#!/bin/sh
# Fake Claude Code для e2e-стендов и отладки нативного адаптера: печатает
# stream-json (init, assistant, result) и шлёт события хуков через
# $RIVET_HOOK_CMD, как настоящий claude через settings-файл. Поведение
# повторяет fake-agent.sh: [e2e-block] без ответа человека -> BLOCKED,
# иначе правит e2e-result.txt (коммитит и пушит runner).
set -eu

# Промпт приходит на stdin (адаптер передаёт его так, а не в argv).
PROMPT=$(cat)

emit() { printf '%s\n' "$1"; }

hook() { # $1 - JSON события хука
  [ -n "${RIVET_HOOK_CMD:-}" ] || return 0
  # Команда приходит с shell-кавычками (путь бинарника может нести пробелы).
  # Контекст от Rivet приходит хуку в stderr с кодом возврата 2 (обратный
  # канал, add-context-channel) — настоящий агент увидел бы его как
  # сообщение системы, fake печатает его текстом ассистента.
  ctx=$(printf '%s' "$1" | sh -c "$RIVET_HOOK_CMD" 2>&1 >/dev/null || true)
  [ -n "$ctx" ] || return 0
  ctx=$(json_escape "$ctx")
  emit "{\"type\":\"assistant\",\"message\":{\"model\":\"claude-fake-1\",\"content\":[{\"type\":\"text\",\"text\":\"$ctx\"}]}}"
}

json_escape() { printf '%s' "$1" | tr '\n' ' ' | sed 's/\\/\\\\/g; s/"/\\"/g'; }

emit '{"type":"system","subtype":"init","session_id":"fake-claude-1","model":"claude-fake-1"}'

# Политика проекта приезжает в назначении и попадает в промпт стадии
# (add-policy-delivery): fake повторяет её строку в поток, чтобы доставка
# была видна в транскрипте, как у настоящего агента в рассуждениях.
case "$PROMPT" in
*"# Политика проекта"*)
  POLICY_LINE=$(printf '%s\n' "$PROMPT" | grep -m1 -A2 '^# Политика проекта' | tr '\n' ' ')
  emit "{\"type\":\"assistant\",\"message\":{\"model\":\"claude-fake-1\",\"content\":[{\"type\":\"text\",\"text\":\"Учитываю политику: $(json_escape "$POLICY_LINE")\"}]}}"
  ;;
esac
hook '{"hook_event_name":"SessionStart","session_id":"fake-claude-1"}'

# Тестировщик (add-agent-roles): тест на критерии в тестовом каталоге и
# вердикт маркером. [e2e-tester-changes] — замечания, пока разработчик не
# сделал второй проход; [e2e-tester-code] — правка src/, которую runner
# откатит как выход за границу роли.
case "$PROMPT" in
*"проверяешь задачу"*)
  mkdir -p tests
  emit '{"type":"assistant","message":{"model":"claude-fake-1","usage":{"input_tokens":1100,"output_tokens":90},"content":[{"type":"text","text":"Пишу тесты по критериям приёмки."},{"type":"tool_use","name":"Edit","input":{"file_path":"'"$PWD"'/tests/e2e-tests.txt"}}]}}'
  printf 'test %s\n' "$(date +%s)" >> tests/e2e-tests.txt
  hook "{\"hook_event_name\":\"PostToolUse\",\"session_id\":\"fake-claude-1\",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$PWD/tests/e2e-tests.txt\"}}"
  VERDICT="VERDICT: OK"
  case "$PROMPT" in
  *"[e2e-tester-code]"*)
    mkdir -p src
    printf 'tester %s\n' "$(date +%s)" >> src/tester-touch.txt
    ;;
  *"[e2e-tester-changes]"*)
    if [ "$(wc -l < e2e-result.txt 2>/dev/null || echo 0)" -lt 2 ]; then
      VERDICT="VERDICT: CHANGES: e2e-тестировщик: критерий 1 не выполнен, повторите реализацию"
    fi
    ;;
  esac
  hook '{"hook_event_name":"Stop","session_id":"fake-claude-1"}'
  emit '{"type":"result","subtype":"success","result":"Тесты готовы.\n'"$VERDICT"'","total_cost_usd":0.02,"usage":{"input_tokens":1100,"output_tokens":90}}'
  exit 0
  ;;
esac

# Задача описания архитектуры (add-architecture-c4): эталонное описание
# демо-CRM через fake-describe.sh. Вывод подкоманды идёт текстом
# ассистента, чтобы не ломать stream-json. Файлы вне docs/architecture не
# трогаются: проверка задачи описания (-only-docs) их не пропустит.
case "$PROMPT" in
*"[rivet:architecture-describe]"*)
  if OUT=$(sh "$(dirname "$0")/fake-describe.sh" 2>&1); then
    emit "{\"type\":\"assistant\",\"message\":{\"model\":\"claude-fake-1\",\"usage\":{\"input_tokens\":1500,\"output_tokens\":200},\"content\":[{\"type\":\"text\",\"text\":\"Описываю архитектуру проекта. $(json_escape "$OUT")\"}]}}"
    hook '{"hook_event_name":"Stop","session_id":"fake-claude-1"}'
    emit '{"type":"result","subtype":"success","result":"Готово: описание архитектуры в docs/architecture.","total_cost_usd":0.03,"usage":{"input_tokens":1500,"output_tokens":200}}'
    exit 0
  fi
  emit "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"result\":\"fake-describe.sh: $(json_escape "$OUT")\"}"
  exit 1
  ;;
esac

case "$PROMPT" in
*"[e2e-block]"*)
  case "$PROMPT" in
  *"Уточнение человека"*) ;; # ответ получен, работаем дальше
  *)
    emit '{"type":"assistant","message":{"model":"claude-fake-1","usage":{"input_tokens":900,"output_tokens":40},"content":[{"type":"text","text":"Не могу однозначно понять требование."}]}}'
    hook '{"hook_event_name":"Stop","session_id":"fake-claude-1"}'
    emit '{"type":"result","subtype":"success","result":"BLOCKED: вопрос от e2e-агента: подтвердите поведение","total_cost_usd":0.01,"usage":{"input_tokens":900,"output_tokens":40}}'
    exit 0
    ;;
  esac
  ;;
esac

emit '{"type":"assistant","message":{"model":"claude-fake-1","usage":{"input_tokens":1200,"cache_read_input_tokens":50000,"output_tokens":120},"content":[{"type":"text","text":"Реализую задачу."},{"type":"tool_use","name":"Bash","input":{"command":"date +%s"}}]}}'
hook "{\"hook_event_name\":\"PostToolUse\",\"session_id\":\"fake-claude-1\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"date +%s\"}}"

# «Секрет» в тексте ассистента: e2e проверяет маскирование транскрипта
# (спека team-visibility «Секрет в транскрипте»).
emit '{"type":"assistant","message":{"model":"claude-fake-1","usage":{"input_tokens":1400,"cache_read_input_tokens":50000,"output_tokens":180},"content":[{"type":"text","text":"export GH_TOKEN=ghp_e2eFakeSecret0123456789"},{"type":"tool_use","name":"Edit","input":{"file_path":"'"$PWD"'/e2e-result.txt"}}]}}'
printf 'work %s\n' "$(date +%s)" >> e2e-result.txt
hook "{\"hook_event_name\":\"PostToolUse\",\"session_id\":\"fake-claude-1\",\"tool_name\":\"Edit\",\"tool_input\":{\"file_path\":\"$PWD/e2e-result.txt\"}}"

# [e2e-slow]: подержать сессию открытой после шага с файлом — e2e смотрит
# реестр активных сессий и пересечения работ (add-team-visibility).
case "$PROMPT" in
*"[e2e-slow]"*)
  sleep 6
  # Ещё один вызов инструмента после паузы: обратный канал доставляет
  # контекст именно на PostToolUse (add-context-channel).
  hook "{\"hook_event_name\":\"PostToolUse\",\"session_id\":\"fake-claude-1\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"git status --porcelain\"}}"
  ;;
esac

hook '{"hook_event_name":"Stop","session_id":"fake-claude-1"}'
emit '{"type":"result","subtype":"success","result":"Готово: результат записан в e2e-result.txt.","total_cost_usd":0.042,"usage":{"input_tokens":2600,"cache_read_input_tokens":100000,"output_tokens":300},"modelUsage":{"claude-fake-1":{"contextWindow":200000}}}'
