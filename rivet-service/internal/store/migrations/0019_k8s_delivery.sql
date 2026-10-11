-- Публикация в Kubernetes (change add-k8s-delivery, спека deployment
-- «Собственная способность деплоить»).

-- Тип исполнения окружения: собственная доставка в кластер манифестами
-- или helm-чартом. Команды собирает control plane, исполняет deploy-runner.
ALTER TABLE environments DROP CONSTRAINT environments_exec_type_check;
ALTER TABLE environments ADD CONSTRAINT environments_exec_type_check
    CHECK (exec_type IN ('ssh', 'pipeline', 'k8s'));

-- Требуемые capability runner'а публикации: доступ к кластеру и к
-- закрытому периметру даёт окружение конкретных runner'ов, и публикация
-- должна попадать именно к ним (спека deployment «Собственная
-- способность деплоить»).
ALTER TABLE environments ADD COLUMN runner_caps text[] NOT NULL DEFAULT '{}';
