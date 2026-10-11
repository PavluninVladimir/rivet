-- Движок политик (change add-policy-engine, спека access-policy).

-- Эскалация уровня проекта: движок политик недоступен, автоматика проекта
-- остановлена. У такой эскалации нет ни задачи, ни публикации — прежний
-- CHECK требовал ровно одну из них.
ALTER TABLE attention DROP CONSTRAINT attention_subject;
ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK (
    (task_id IS NOT NULL AND deployment_id IS NULL) OR
    (task_id IS NULL AND deployment_id IS NOT NULL) OR
    (task_id IS NULL AND deployment_id IS NULL AND reason = 'POLICY_ENGINE'));

-- Одна открытая эскалация на проект и причину: движок падает на каждом
-- тике, очередь «needs attention» не должна расти.
CREATE UNIQUE INDEX attention_open_project_reason ON attention(project_id, reason)
    WHERE status <> 'resolved' AND task_id IS NULL AND deployment_id IS NULL;
