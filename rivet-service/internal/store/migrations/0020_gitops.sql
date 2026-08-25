-- Публикация через GitOps (change add-gitops-delivery, спека deployment
-- «Дирижирование внешними системами доставки»).

-- Тип исполнения окружения: версия меняется коммитом в репозиторий
-- конфигурации, выкат делает контроллер кластера.
ALTER TABLE environments DROP CONSTRAINT environments_exec_type_check;
ALTER TABLE environments ADD CONSTRAINT environments_exec_type_check
    CHECK (exec_type IN ('ssh', 'pipeline', 'k8s', 'gitops'));
