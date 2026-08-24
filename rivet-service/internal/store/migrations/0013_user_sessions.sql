-- Сессии людей (change add-user-sessions, спеки agent-integration,
-- team-visibility): приватность сессии задаётся при запуске и неизменна.
ALTER TABLE sessions ADD COLUMN private boolean NOT NULL DEFAULT false;
