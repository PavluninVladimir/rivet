// Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается.
workspace "Rivet" "Автономная система разработки: превращает эпики и исследование продукта в выполненные задачи, публикует изменения через SCM и ведёт релизы с наблюдением за окружениями." {
    !identifiers flat

    model {
        biz_admin = person "Администратор установки" "Управляет пользователями, подключениями моделей, агентами и окружениями." "External"
        biz_analyst = person "Аналитик" "Роль агента: исследует интервью и отзывы, формирует потребности и бэклог." ""
        biz_architect = person "Архитектор" "Роль агента: описывает и проверяет архитектуру проекта в формате Rivet." ""
        biz_developer = person "Разработчик" "Роль агента: пишет код задачи в рабочей ветке." ""
        biz_devops = person "Релиз-инженер" "Роль агента: публикует изменения в окружения и оформляет релиз." ""
        biz_lead = person "Тимлид" "Роль агента: раскладывает Epic на план задач и руководит процессом." ""
        biz_member = person "Участник проекта" "Человек с доступом к задачам, релизам и сессиям проекта." "External"
        biz_owner = person "Владелец проекта" "Человек, который ставит цели, утверждает бэклог и следит за результатом." "External"
        biz_reviewer = person "Ревьюер" "Роль агента: проверяет изменения перед слиянием в базовую ветку." ""
        biz_sre = person "Инженер сопровождения" "Роль агента: наблюдает за окружениями и описывает инциденты." ""
        biz_tester = person "Тестировщик" "Роль агента: проверяет результат задачи и фиксирует вердикт." ""
        system = softwareSystem "Rivet" "Автономная система разработки: превращает эпики и исследование продукта в выполненные задачи, публикует изменения через SCM и ведёт релизы с наблюдением за окружениями." "" {
            app_cli = container "rivet CLI" "Командная утилита администратора и участника: настройка подключений, импорт истории, проверка архитектуры." "Go 1.26" ""
            app_rivetd = container "rivetd" "Control plane Rivet: HTTP API, gRPC-хаб runner'ов, оркестрация и встроенная веб-консоль." "Go 1.26, net/http, gRPC, pgx" "" {
                app_analyst = component "Аналитик продукта" "Фоновый анализ интервью: конспекты, синтез потребностей и черновик бэклога." "Go" ""
                app_api = component "REST API и SSE" "HTTP API консоли и CLI, SSE-канал событий и приём webhook'ов SCM." "Go net/http" ""
                app_architecture = component "Снимок архитектуры" "Читает docs/architecture, проверяет модель и рисует представления в формате Rivet." "Go" ""
                app_cycle = component "Цикл разработки" "Агрегирует фазы релизов и инцидентов для обзора проекта." "Go" ""
                app_discovery = component "Проверка исследования" "Проверяет и нормализует результаты анализа, связи потребностей и бэклог." "Go" ""
                app_history = component "Импорт истории" "Разбирает архив OpenSpec и манифест истории, привязывает PR к Epic'ам и задачам." "Go" ""
                app_llm = component "Клиенты моделей" "Единый клиент к API-совместимым моделям разных провайдеров." "Go" ""
                app_ops = component "Релизы и инциденты" "Формирует release notes и описания инцидентов с помощью модели." "Go" ""
                app_orchestrator = component "Оркестратор" "Детерминированный конвейер: диспетчеризация этапов, реакции на вердикты, публикации и слияние." "Go" ""
                app_overview = component "Обзор проекта" "Строит отчёт о ходе проекта для руководителя." "Go" ""
                app_planner = component "Планировщик" "Декомпозиция Epic в план задач с помощью модели; проверка плана и материализация задач." "Go" ""
                app_policy = component "Движок политик" "Вычисляет действующую политику проекта и проверяет решения гейтов, включая режимы OPA/Rego." "Go, OPA Rego" ""
                app_redact = component "Маскировка секретов" "Удаляет секреты из транскриптов сессий перед хранением." "Go" ""
                app_scm = component "SCM-адаптеры" "Адаптеры хостингов репозиториев и разбор событий webhook." "Go" ""
                app_stream = component "gRPC-хаб runner'ов" "Двунаправленный gRPC-поток PlaneMsg между control plane и runner'ами." "gRPC" ""
                app_webui = component "Веб-консоль" "Встроенная одностраничная консоль, раздаваемая rivetd." "Web-сборка в internal/webui" ""
            }
            app_runner = container "rivet-runner" "Агент на машине пользователя: подключается к control plane и выполняет этапы задач, публикации и проверки." "Go 1.26, gRPC" "" {
                app_runner_agent = component "Агентский цикл Rivet" "Цикл агента роли: чтение репозитория, вызов модели, инструменты и формирование вердикта." "Go" ""
                app_runner_engine = component "Движок runner'а" "Подключение к control plane, получение назначений, исполнение стадий публикации и наблюдения." "Go" ""
            }
            tech_postgres = container "PostgreSQL" "Реляционная база с изменяемой моделью Rivet: проекты, эпики, задачи, сессии, релизы, агенты и настройки." "PostgreSQL 17" "Database"
            tech_s3 = container "Объектное хранилище" "Хранит тяжёлые артефакты сессий — транскрипты; в базе остаются только ссылки s3://." "S3-совместимое хранилище (RustFS в dev, S3 в проде)" "Database"
        }
        ext_gitea = softwareSystem "Gitea / Forgejo" "Хостинг репозитория: REST API, клонирование, PR и webhook'и." "External"
        ext_github = softwareSystem "GitHub" "Хостинг репозитория: REST API, клонирование, PR и webhook'и." "External"
        ext_gitlab = softwareSystem "GitLab" "Хостинг репозитория: REST API, клонирование, MR и webhook'и." "External"
        ext_llm = softwareSystem "LLM-провайдеры" "Внешние API моделей: OpenAI, Anthropic, DeepSeek, OpenRouter, Ollama и LM Studio." "External"

        app_analyst -> app_discovery "передаёт"
        app_analyst -> app_llm "использует, передаёт"
        app_analyst -> tech_postgres "читает Интервью и отзыв, читает и пишет Запуск исследования, читает и пишет Конспект исследования"
        app_api -> app_analyst "передаёт"
        app_api -> app_history "передаёт"
        app_api -> app_llm "передаёт"
        app_api -> app_orchestrator "передаёт"
        app_api -> app_planner "передаёт"
        app_api -> app_scm "использует"
        app_api -> tech_postgres "читает Запись использования модели, читает Этап задачи, читает и пишет Epic, читает и пишет Runner, читает и пишет Агент, читает и пишет Агент команды, читает и пишет Версия политики, читает и пишет Задача, читает и пишет Запрос внимания, читает и пишет Запуск исследования, читает и пишет Интервью и отзыв, читает и пишет Инцидент, читает и пишет Настройка установки, читает и пишет Окружение, читает и пишет Подключение модели, читает и пишет Пользователь, читает и пишет Проект, читает и пишет Публикация, читает и пишет Релиз, читает и пишет Роль агента, читает и пишет Сессия агента, читает и пишет Сессия входа, читает и пишет Снимок архитектуры, читает и пишет Событие журнала, читает и пишет Токен доступа, читает и пишет Участник проекта, читает и пишет Учётные данные SCM"
        app_architecture -> tech_postgres "читает Epic, читает Задача, читает и пишет Снимок архитектуры"
        app_cli -> app_api "использует"
        app_cycle -> tech_postgres "читает Инцидент, читает Публикация, читает Релиз"
        app_discovery -> tech_postgres "читает Запуск исследования, читает и пишет Epic, читает и пишет Потребность Epic"
        app_history -> tech_postgres "читает и пишет Epic, читает и пишет Задача, читает и пишет Событие журнала"
        app_llm -> ext_llm "использует"
        app_ops -> app_llm "использует, передаёт"
        app_ops -> tech_postgres "читает и пишет Инцидент, читает и пишет Релиз"
        app_orchestrator -> app_architecture "передаёт"
        app_orchestrator -> app_ops "передаёт"
        app_orchestrator -> app_scm "использует"
        app_orchestrator -> app_stream "передаёт"
        app_orchestrator -> tech_postgres "читает Runner, читает Версия политики, читает Сессия агента, читает и пишет Epic, читает и пишет Задача, читает и пишет Инцидент, читает и пишет Публикация, читает и пишет Релиз, читает и пишет Событие журнала, читает и пишет Этап задачи"
        app_overview -> tech_postgres "читает Epic, читает Задача, читает Запись использования модели, читает Проект, читает Событие журнала"
        app_planner -> app_llm "использует, передаёт"
        app_planner -> tech_postgres "читает Потребность Epic, читает и пишет Epic, читает и пишет Задача"
        app_policy -> tech_postgres "читает Версия политики"
        app_runner -> app_stream "использует"
        app_runner_agent -> ext_llm "использует"
        app_runner_engine -> app_runner_agent "передаёт"
        app_runner_engine -> app_stream "использует"
        app_scm -> ext_gitea "использует"
        app_scm -> ext_github "использует"
        app_scm -> ext_gitlab "использует"
        app_stream -> app_orchestrator "передаёт"
        app_stream -> tech_postgres "читает Runner, читает Привязка токена runner'а, читает Токен runner'а"
        app_webui -> app_api "использует"
        biz_admin -> app_api "использует"
        biz_admin -> app_webui "использует"
        biz_analyst -> app_analyst "использует"
        biz_analyst -> app_api "использует"
        biz_architect -> app_api "использует"
        biz_architect -> app_architecture "использует"
        biz_devops -> app_api "использует"
        biz_devops -> app_cycle "использует"
        biz_lead -> app_api "использует"
        biz_lead -> app_planner "использует"
        biz_member -> app_api "использует"
        biz_member -> app_webui "использует"
        biz_owner -> app_api "использует"
        biz_owner -> app_overview "использует"
        biz_owner -> app_webui "использует"
        biz_sre -> app_api "использует"
        biz_sre -> app_cycle "использует"
        ext_gitea -> app_api "передаёт"
        ext_github -> app_api "передаёт"
        ext_gitlab -> app_api "передаёт"

        deploymentEnvironment "Основное" {
            deploymentNode "Сервер установки Rivet" "Docker-хост, на котором разворачиваются rivetd, PostgreSQL и объектное хранилище." "Docker Compose (Linux)" {
                containerInstance app_rivetd
                containerInstance tech_postgres
                containerInstance tech_s3
            }
            deploymentNode "Машина runner'а" "Рабочая машина пользователя, где runner'у доступны репозиторий и песочница." "Linux" {
                containerInstance app_runner
            }
        }
    }

    views {
        systemContext system "context" {
            include *
            autoLayout lr
        }
        container system "containers-system" {
            include *
            autoLayout lr
        }
        component app_rivetd "components-app_rivetd" {
            include *
            autoLayout lr
        }
        component app_runner "components-app_runner" {
            include *
            autoLayout lr
        }
        deployment * "Основное" "deployment" {
            include *
            autoLayout lr
        }

        styles {
            element "Person" {
                shape Person
            }
            element "Database" {
                shape Cylinder
            }
            element "Queue" {
                shape Pipe
            }
            element "External" {
                background #8a8a8a
            }
        }
    }
}
