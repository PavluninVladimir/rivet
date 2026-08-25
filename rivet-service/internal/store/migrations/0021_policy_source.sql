-- Git-провайдер политик (change add-policy-git-provider, спека
-- access-policy «Хранение политик — провайдеры и модель угроз»).

-- Источник политики проекта: собственное хранилище (правка из консоли)
-- или файл в доверенной ветке репозитория.
ALTER TABLE projects ADD COLUMN policy_source text NOT NULL DEFAULT 'store'
    CHECK (policy_source IN ('store', 'git'));

-- Идентификатор версии файла, из которого создана последняя версия
-- политики: по нему видно, что содержимое не менялось.
ALTER TABLE projects ADD COLUMN policy_file_id text NOT NULL DEFAULT '';

-- Эскалация «политика проекта в репозитории сломана» — уровня проекта:
-- ни задачи, ни публикации у неё нет.
ALTER TABLE attention DROP CONSTRAINT attention_subject;
ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK (
    (task_id IS NOT NULL AND deployment_id IS NULL) OR
    (task_id IS NULL AND deployment_id IS NOT NULL) OR
    (task_id IS NULL AND deployment_id IS NULL AND reason IN ('POLICY_ENGINE', 'POLICY_SOURCE')));
