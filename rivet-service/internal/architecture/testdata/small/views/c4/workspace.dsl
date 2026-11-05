// Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается.
workspace "Учёт заявок" "Принимает заявки клиентов и ведёт их до закрытия" {
    !identifiers flat

    model {
        biz_client = person "Клиент" "" ""
        system = softwareSystem "Учёт заявок" "Принимает заявки клиентов и ведёт их до закрытия" "" {
            app_api = container "API заявок" "" "Go" ""
            app_web = container "Веб-кабинет" "Страницы для клиентов" "TypeScript, React" ""
            tech_db = container "PostgreSQL" "" "PostgreSQL 16" "Database"
        }

        app_api -> tech_db "использует, читает и пишет Заявка"
        app_web -> app_api "передаёт"
        biz_client -> app_web "использует"
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
