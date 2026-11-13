-- Charge the UTC day of the actual reservation, not the age of a queued run.
ALTER TABLE assistant_runs ADD COLUMN reserved_at timestamptz;
-- Existing unknown reservation times are conservatively charged to today.
UPDATE assistant_runs SET reserved_at=now() WHERE reserved_tokens>0 OR reserved_cost_usd>0;
CREATE INDEX assistant_daily_reservations ON assistant_runs(project_id,reserved_at) WHERE reserved_at IS NOT NULL;
