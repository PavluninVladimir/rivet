-- Обратный канал контекста (change add-context-channel, спеки
-- agent-integration, runners).

-- Поддерживает ли адаптер runner'а доставку контекста работающему агенту.
-- Объявляется при регистрации; runner'ам без канала control plane контекст
-- не отправляет (режим «только отправка»).
ALTER TABLE runners ADD COLUMN context_channel boolean NOT NULL DEFAULT false;
