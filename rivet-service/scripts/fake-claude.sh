#!/bin/sh
# Fake Claude Code для e2e-стендов и отладки нативного адаптера: печатает
# stream-json (init, assistant, result) и шлёт события хуков через
# $RIVET_HOOK_CMD, как настоящий claude через settings-файл. Поведение
# повторяет fake-agent.sh: [e2e-block] без ответа человека -> BLOCKED,
# иначе правит e2e-result.txt (коммитит и пушит runner).
set -eu

# Промпт приходит на stdin (адаптер передаёт его так, а не в argv).
PROMPT=$(cat)

hook() { # $1 - JSON события хука
  if [ -n "${RIVET_HOOK_CMD:-}" ]; then
    # Команда приходит с shell-кавычками (путь бинарника может нести пробелы).
    printf '%s' "$1" | sh -c "$RIVET_HOOK_CMD" || true
  fi
}

emit() { printf '%s\n' "$1"; }

emit '{"type":"system","subtype":"init","session_id":"fake-claude-1","model":"claude-fake-1"}'
hook '{"hook_event_name":"SessionStart","session_id":"fake-claude-1"}'

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
*"[e2e-slow]"*) sleep 6 ;;
esac

hook '{"hook_event_name":"Stop","session_id":"fake-claude-1"}'
emit '{"type":"result","subtype":"success","result":"Готово: результат записан в e2e-result.txt.","total_cost_usd":0.042,"usage":{"input_tokens":2600,"cache_read_input_tokens":100000,"output_tokens":300},"modelUsage":{"claude-fake-1":{"contextWindow":200000}}}'
