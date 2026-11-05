#!/bin/sh
# Фикстура задачи описания архитектуры для fake-агентов (add-architecture-c4,
# add-architecture-erd-sequence-adr): кладёт в рабочую копию эталонное
# описание демо-CRM (C4, ERD, сценарий, ADR) и генерирует представления той
# же подкомандой runner'а, что и настоящий Архитектор. От адаптера агента
# не зависит: скрипт зовёт любой fake по маркеру задания describe.
#
# Запуск из корня рабочей копии: sh scripts/fake-describe.sh [каталог-фикстуры]
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
SRC=${1:-"$HERE/../internal/architecture/testdata/crm"}
DIR=docs/architecture
: "${RIVET_RUNNER_BIN:?нужен rivet-runner с поддержкой проверки архитектуры}"

mkdir -p "$DIR"
# Модель, сценарии и ADR берутся из фикстуры, представления строятся заново.
for part in architecture.yaml model scenarios adr; do
  if [ -e "$SRC/$part" ]; then
    cp -R "$SRC/$part" "$DIR/"
  fi
done
"$RIVET_RUNNER_BIN" architecture views -dir "$DIR"
"$RIVET_RUNNER_BIN" architecture validate -dir "$DIR"
