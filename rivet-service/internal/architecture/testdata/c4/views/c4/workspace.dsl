// Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается.
workspace "Платформа сборок" "Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок" {
    !identifiers flat

    model {
        biz_admin = person "Администратор" "" ""
        biz_auditor = person "Аудитор" "Проверяет историю сборок снаружи компании" "External"
        biz_dev = person "Разработчик" "" ""
        system = softwareSystem "Платформа сборок" "Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок" "" {
            app_api = container "API" "" "Go, net/http" "" {
                app_api_auth = component "Аутентификация" "" "Go" ""
                app_api_builds = component "Сборки" "Ставит сборки в очередь и отдаёт их статус" "Go" ""
            }
            app_web = container "Консоль" "Веб-интерфейс платформы" "React" ""
            app_worker = container "Воркер сборок" "" "Go" ""
            tech_nats = container "NATS" "Очередь заданий сборки" "NATS JetStream" "Queue"
            tech_postgres = container "PostgreSQL" "" "PostgreSQL 16" "Database"
        }
        app_github = softwareSystem "GitHub" "Хостинг репозиториев и событий push" "External"

        app_api_builds -> app_github "вызывает"
        app_api_builds -> tech_nats "использует"
        app_api_builds -> tech_postgres "пишет Сборка"
        app_web -> app_api "передаёт"
        app_worker -> app_github "передаёт"
        app_worker -> tech_nats "использует"
        app_worker -> tech_postgres "читает и пишет Сборка"
        biz_admin -> app_web "использует"
        biz_auditor -> app_web "использует"
        biz_dev -> app_api_auth "использует"
        biz_dev -> app_web "использует"

        deploymentEnvironment "Основное" {
            deploymentNode "Виртуальная машина" "" "Ubuntu 24.04" {
                deploymentNode "Docker" "" "системное ПО" {
                    containerInstance app_api
                    containerInstance app_web
                    containerInstance app_worker
                }
                containerInstance tech_postgres
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
        component app_api "components-app_api" {
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
