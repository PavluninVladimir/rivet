// Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается.
workspace "CRM" "Учёт клиентов и контактов для небольшого отдела продаж. Один процесс Node.js раздаёт веб-клиент и JSON API, данные лежат в JSON-файле." {
    !identifiers flat

    model {
        biz_head = person "Руководитель отдела продаж" "" ""
        biz_manager = person "Менеджер по продажам" "" ""
        system = softwareSystem "CRM" "Учёт клиентов и контактов для небольшого отдела продаж. Один процесс Node.js раздаёт веб-клиент и JSON API, данные лежат в JSON-файле." "" {
            app_api = container "API-сервер" "JSON API и раздача статики веб-клиента на одном порту (server/)" "Node.js, node:http" "" {
                app_api_domain = component "Домен контактов" "Создание, правка и проверка полей контакта, чистые функции (server/domain)" "JavaScript" ""
                app_api_routes = component "Маршруты" "Обработчики /api/health и /api/contacts (server/routes)" "node:http" ""
                app_api_storage = component "Хранилище контактов" "Чтение и атомарная запись JSON-файла, очередь записей (server/storage/json-store.js)" "node:fs" ""
            }
            app_web = container "Веб-клиент" "Список контактов с поиском и постраничной загрузкой, форма добавления (web/)" "HTML, ES-модули" ""
            tech_json_file = container "JSON-файл" "data/crm.json, путь задаёт CRM_DATA_FILE" "JSON, атомарная запись через rename" "Database"
        }

        app_api_routes -> app_api_domain "передаёт"
        app_api_routes -> app_api_storage "передаёт"
        app_api_storage -> tech_json_file "читает и пишет Контакт"
        app_web -> app_api "передаёт"
        biz_head -> app_web "использует"
        biz_manager -> app_web "использует"

        deploymentEnvironment "Основное" {
            deploymentNode "Сервер" "" "Node.js 22" {
                containerInstance app_api
                containerInstance tech_json_file
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
