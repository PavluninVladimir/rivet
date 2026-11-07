-- Отзывы пользователей (add-user-feedback, спека product-discovery
-- «Обратная связь пользователей»): материал исследования вида feedback в
-- той же таблице, что интервью. Нормализация, нумерация строк, мягкое
-- удаление и проверка цитат у них общие. Прежние строки — интервью.
-- Источник отзыва хранится в respondent_role, название — «Отзыв · <источник>».
ALTER TABLE discovery_interviews ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'interview';
ALTER TABLE discovery_interviews DROP CONSTRAINT IF EXISTS discovery_interviews_kind_check;
ALTER TABLE discovery_interviews ADD CONSTRAINT discovery_interviews_kind_check CHECK (kind IN ('interview', 'feedback'));

-- Сколько отзывов в версии, сколько вошло во вход синтеза и сколько не
-- дали подтверждённых находок: отзыв в наборе версии ещё не значит, что
-- Аналитик его читал (вход синтеза сокращается при сотнях отзывов, отзыв
-- без находок во вход не входит). NULL у версий без этих сведений.
ALTER TABLE discovery_runs ADD COLUMN IF NOT EXISTS feedback_total int;
ALTER TABLE discovery_runs ADD COLUMN IF NOT EXISTS feedback_synthesized int;
ALTER TABLE discovery_runs ADD COLUMN IF NOT EXISTS feedback_without_findings int;
