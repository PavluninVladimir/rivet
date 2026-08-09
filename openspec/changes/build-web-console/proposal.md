## Зачем

Бэкенд-срез работает и провёл первую задачу через полный цикл, но наблюдать за системой можно только через curl. Консоль — глаза Rivet: дашборд Epic с DAG, эскалации, live-ход задач и кнопка merge. API-контракт v1 и эталонный дизайн-прототип готовы — пора реализовать веб-клиента.

## Что меняется

- В `rivet-web` появляется React-приложение: представления Projects/Epics/Tasks/Runners/Activity, дашборд Epic с DAG-графом и фильтрами, деталка задачи с timeline и live-потоком, командная палитра, действия человека (answer/retry/cancel/merge, управление Epic).
- Живые обновления через SSE `/api/v1/stream` с реплеем по `Last-Event-ID`.
- `rivetd` раздаёт прод-сборку консоли (go:embed) — self-hosted получает UI из одного бинарника.
- Эталон UX — `agent-orchestration-console.html`; отклонения фиксируются в design.

## Capabilities

### New Capabilities

Нет — изменение реализует существующую спеку `clients/web-console` (`skip_specs: true`, требования не меняются).

### Modified Capabilities

Нет.

## Затронутые репозитории

- [x] `rivet` — раздача статики консоли
- [x] `rivet-web` — само приложение
- [ ] `rivet-android`
- [ ] `rivet-ios`

## Impact

- `rivet-web` перестаёт быть пустым репозиторием: Vite + React + TypeScript.
- `rivetd` получает embed-статику; API не меняется (консоль — чистый потребитель api-contract v1).
- Лента команды из спеки web-console реализуется в объёме текущего API (задачи и runner'ы; сессии людей появятся вместе с бэкендом team-visibility).
