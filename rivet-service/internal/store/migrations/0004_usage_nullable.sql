-- Учёт различает «данных нет» и ноль (спека observability «Учёт usage»):
-- NULL = источник не сообщил значение, оно не участвует в агрегатах.
ALTER TABLE usage_records
    ALTER COLUMN tokens_in  DROP NOT NULL, ALTER COLUMN tokens_in  DROP DEFAULT,
    ALTER COLUMN tokens_out DROP NOT NULL, ALTER COLUMN tokens_out DROP DEFAULT,
    ALTER COLUMN cost_usd   DROP NOT NULL, ALTER COLUMN cost_usd   DROP DEFAULT;

-- Backfill: до этой миграции runner физически не заполнял токены и стоимость,
-- поэтому строки, где все три значения нулевые, — заглушки, а не реальные нули.
UPDATE usage_records SET tokens_in = NULL, tokens_out = NULL, cost_usd = NULL
WHERE tokens_in = 0 AND tokens_out = 0 AND cost_usd = 0;

-- Отчёт агента — внешний ввод; отрицательные значения испортили бы агрегаты
-- (NULL проходит CHECK: «нет данных» — валидное состояние).
ALTER TABLE usage_records ADD CONSTRAINT usage_records_nonnegative CHECK (
    tokens_in >= 0 AND tokens_out >= 0 AND cost_usd >= 0 AND duration_s >= 0);

-- Токены сессии: NULL = ни одна usage-запись сессии не содержала токенов.
ALTER TABLE sessions
    ALTER COLUMN tokens DROP NOT NULL, ALTER COLUMN tokens DROP DEFAULT;
UPDATE sessions SET tokens = NULL WHERE tokens = 0;

-- Заполненность контекста runner'а: NULL = неизвестна (агент не отчитался).
ALTER TABLE runners
    ALTER COLUMN ctx_pct DROP NOT NULL, ALTER COLUMN ctx_pct DROP DEFAULT;
UPDATE runners SET ctx_pct = NULL WHERE ctx_pct = 0;
ALTER TABLE runners ADD CONSTRAINT runners_ctx_pct_range CHECK (ctx_pct BETWEEN 0 AND 100);
