-- Локальный compose-проект живёт на одной машине. Потеря runner не
-- разрешает переносить команды на другую машину.
ALTER TABLE env_hosts ADD COLUMN local_runner text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX environments_local_port ON environments
    (((config->'provision'->>'port')::int))
    WHERE config->'provision'->>'provider' = 'local'
      AND (config->'provision'->>'port')::int > 0;
