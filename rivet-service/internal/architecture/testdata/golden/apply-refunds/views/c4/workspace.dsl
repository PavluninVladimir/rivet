// Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается.
workspace "Интернет-магазин" "" {
    !identifiers flat

    model {
        biz_buyer = person "Покупатель (biz.buyer)" "" ""
        biz_customer = person "Покупатель (biz.customer)" "" ""
        biz_manager = person "Менеджер заказов" "" ""
        biz_staff = person "Сотрудник магазина" "" ""
        system = softwareSystem "Интернет-магазин" "" "" {
            app_orders = container "Сервис заказов" "" "" ""
            app_refunds = container "Сервис возвратов" "" "" ""
            app_storefront = container "Витрина" "Веб-приложение для покупателей" "" ""
            tech_postgres = container "PostgreSQL" "" "" "Database"
        }

        app_orders -> tech_postgres "использует, пишет Запись заказа"
        app_refunds -> tech_postgres "пишет Запись заказа"
        app_storefront -> app_orders "использует, передаёт"
        app_storefront -> app_refunds "передаёт"

        deploymentEnvironment "Основное" {
            deploymentNode "Кластер Kubernetes" "" "узел" {
                containerInstance app_orders
                containerInstance app_refunds
                containerInstance app_storefront
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
