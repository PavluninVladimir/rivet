# Описание архитектуры проекта

Rivet читает описание архитектуры из репозитория проекта, из каталога `docs/architecture/`. Описание необязательно: если каталога нет, Rivet считает, что у проекта нет описания, и это не ошибка.

Описание пишут агенты и люди. Rivet проверяет его детерминированным кодом, без модели, и рисует диаграммы Mermaid, которые GitHub показывает прямо в PR. Модель повторяет слои TOGAF (бизнес, приложения, данные, технологии) на подмножестве ArchiMate 3.2, бизнес-процессы хранятся в BPMN 2.0.

Полный пример лежит в [`rivet-service/internal/architecture/testdata/shop`](../rivet-service/internal/architecture/testdata/shop): небольшой интернет-магазин с четырьмя слоями, двумя процессами, собственным viewpoint'ом и изменением «Возвраты заказов».

## Быстрый старт

```sh
rivet architecture init -system "Интернет-магазин"   # пустое описание в docs/architecture
rivet architecture validate                          # проверить описание и все изменения
rivet architecture views                             # перерисовать views/
```

Все команды работают с локальным каталогом и не обращаются к control plane. Флаг `-dir` задаёт другой каталог описания.

## Раскладка

```
docs/architecture/
  architecture.yaml          манифест
  model/
    business.yaml            бизнес-слой
    application.yaml         слой приложений
    data.yaml                слой данных
    technology.yaml          слой технологий
    <viewpoint>.yaml         документ собственного viewpoint'а
  processes/<id>.bpmn        процессы, имя файла совпадает с id элемента process
  viewpoints/<id>.yaml       определения собственных viewpoint'ов
  views/                     сгенерированные представления, руками не править
  changes/<id>/              изменения, которые ещё не применены
  changes/archive/<дата>-<id>/
```

Пути выводятся из манифеста и модели, поэтому описание читается без обхода каталога. Отсутствующий файл слоя равен пустому слою. Симлинк, который ведёт за пределы каталога описания, не читается и не перезаписывается: команда завершается ошибкой. О лишних файлах в `model/`, `processes/` и `viewpoints/` проверка предупреждает: чаще всего это опечатка в имени.

## Манифест

```yaml
format: rivet.architecture/v1
system: Интернет-магазин
principles:
  - id: p.single-order-source
    title: У заказа один источник истины
    rationale: Состояние заказа меняет только сервис заказов
viewpoints: [security]
```

`format` проверяется первым. Если версия неизвестна, проверка останавливается с ошибкой `format.unsupported` и остальные файлы не разбирает. Принципы архитектуры задают правила проекта, которые видят агенты и ревьюер, идентификаторы принципов не должны повторяться. `viewpoints` перечисляет собственные viewpoint'ы проекта.

## Модель

Файл слоя содержит массив `elements`:

```yaml
elements:
  - id: app.orders
    type: component
    name: Сервис заказов
    description: Хранит заказы и меняет их статус
    props: {language: go, replicas: 3}
    relations:
      - {type: realization, to: app.order-api}
      - {type: access, to: data.order, mode: write}
```

`id`, `type` и `name` обязательны. Идентификатор подходит под шаблон `^[a-z][a-z0-9-]*(\.[a-z0-9-]+)+$` и уникален во всём описании. Префикс слоя (`biz.`, `app.`, `data.`, `tech.`) не обязателен, но помогает читать связи. В `props` лежат строки, числа и логические значения. Неизвестные поля дают ошибку: опечатка в имени поля не должна молча пропадать.

YAML разбирается строго: один документ в файле, без якорей, ссылок `*name`, ключей слияния `<<` и нестандартных тегов.

### Типы элементов

| Слой | Файл | Типы |
|---|---|---|
| Бизнес | `model/business.yaml` | `actor`, `role`, `business-service`, `process`, `business-object` |
| Приложения | `model/application.yaml` | `component`, `interface`, `application-service` |
| Данные | `model/data.yaml` | `data-object` |
| Технологии | `model/technology.yaml` | `node`, `system-software` |

Элемент лежит в файле своего слоя.

### Связи

Связь хранится у источника и указывает на цель полем `to`. Допустимые пары:

| Связь | Источник → цель |
|---|---|
| `composition` | элементы одного слоя |
| `association` | любые элементы |
| `assignment` | actor → role · actor, role → process · node → system-software |
| `realization` | process → business-service · component → application-service · data-object → business-object · component, application-service → process |
| `serving` | business-service → actor, role, process · application-service, interface, component → component, process, role, actor · node, system-software → component, node, system-software |
| `access` | process, component, application-service → business-object, data-object · system-software → data-object |
| `flow`, `triggering` | process → process · component → component |

У `access` есть необязательный `mode`: `read`, `write` или `readwrite`. У других связей `mode` не задаётся.

## Бизнес-процессы

Процесс описывается элементом типа `process` в бизнес-слое и файлом `processes/<id>.bpmn` в формате BPMN 2.0 XML. Файл можно рисовать в Camunda Modeler или bpmn.io, координаты диаграммы (BPMN DI) необязательны.

Ссылки на модель задаёт атрибут `rivet:ref` в пространстве имён `urn:rivet:architecture:1`:

```xml
<bpmn:definitions xmlns:bpmn="http://www.omg.org/spec/BPMN/20100524/MODEL"
                  xmlns:rivet="urn:rivet:architecture:1">
  <bpmn:process id="checkout">
    <bpmn:laneSet id="lanes">
      <bpmn:lane id="lane-buyer" name="Покупатель" rivet:ref="biz.buyer">
        <bpmn:flowNodeRef>start</bpmn:flowNodeRef>
      </bpmn:lane>
    </bpmn:laneSet>
    <bpmn:serviceTask id="create-order" name="Создать заказ" rivet:ref="app.order-api"/>
    ...
```

Дорожка ссылается на `role` или `actor`, сервисная задача на `component` или `application-service`. Редакторы на bpmn-js сохраняют атрибут при правке, но в интерфейсе его не показывают, поэтому добавлять его проще в XML.

Поддерживается подмножество: процесс, дорожки, события `startEvent`, `intermediateCatchEvent`, `intermediateThrowEvent`, `endEvent`, задачи `task`, `userTask`, `serviceTask`, `manualTask`, шлюзы `exclusiveGateway`, `parallelGateway`, потоки `sequenceFlow`. Остальные элементы (подпроцессы, граничные события, другие шлюзы и задачи) дают предупреждение `bpmn.unsupported`. Через них всё равно проходит обход графа, поэтому ложных тупиков нет. Если в файле несколько пулов, проверяется первый.

Структура проверяется так:

- Есть хотя бы одно начальное и одно конечное событие.
- Каждый узел достижим из начального события.
- Из каждого узла достижимо конечное событие.
- Источник и цель каждого потока существуют.
- У ветвящего шлюза не меньше двух исходящих потоков.

## Собственные viewpoint'ы

Viewpoint определяет часть архитектуры: какой документ её описывает, как его проверять, какое задание получает агент и как её рисовать. Четыре встроенных viewpoint'а совпадают со слоями. Свой viewpoint проект объявляет в манифесте и описывает файлом `viewpoints/<id>.yaml`:

```yaml
id: security
title: Угрозы и меры защиты
prompt: |
  Опиши угрозы для компонентов и интерфейсов и меры против них.
items: /threats              # JSON Pointer на массив записей с полем id
schema:                      # JSON Schema документа model/security.yaml
  type: object
  required: [threats]
  properties:
    threats:
      type: array
      items:
        type: object
        required: [id, title, targets]
        properties:
          id: {type: string}
          title: {type: string}
          targets: {type: array, items: {type: string, x-rivet-ref: true}}
render:
  kind: graph                # graph или table
  label: title
  edges: [targets]
```

- `prompt` получает агент, когда описывает этот viewpoint.
- `items` указывает на массив записей. Записи адресуются по `id`, по нему же их меняет дельта. Идентификаторы записей подчиняются тому же шаблону, что элементы модели, и не должны совпадать с ними.
- `schema` задаёт JSON Schema документа (draft 2020-12). Внешние `$ref` (файлы и URL) не загружаются.
- `x-rivet-ref: true` помечает строку как ссылку на элемент основной модели. Такие ссылки проверяются так же, как связи. Пометка учитывается в `properties`, `items`, `prefixItems`, `patternProperties`, `additionalProperties`, `allOf` и локальных `$ref` вида `#/$defs/x` или `#якорь` (`$anchor`). Другие `$ref` и `$dynamicRef` в схеме собственного viewpoint'а запрещены. В условных ветках (`if`/`then`/`else`, `anyOf`, `oneOf`, `not`, `contains`, `dependentSchemas`) она запрещена: применимость ветки зависит от значения, и проверка не может однозначно сказать, ссылка это или текст.
- `render.kind: graph` рисует записи узлами и проводит рёбра по полям из `edges`. Поля в `edges` должны быть помечены `x-rivet-ref`. `render.kind: table` рисует таблицу, колонки задаёт `columns` или берутся все поля записей.

Идентификатор собственного viewpoint'а не может совпадать со встроенным.

## Изменение архитектуры

Изменение описывает переход от текущего состояния к целевому и лежит в `changes/<id>/`:

```
changes/refunds/
  change.yaml              id, title, необязательная ссылка на Epic (epic)
  vision.md                видение: цель, рамки, заинтересованные лица
  delta/<viewpoint>.yaml   дельта по viewpoint'у
  processes/<id>.bpmn      целевые процессы
  gap.yaml                 разрывы
```

Дельта состоит из трёх разделов:

```yaml
added:                     # новые элементы целиком
  - {id: app.refunds, type: component, name: Сервис возвратов}
modified:                  # изменённые элементы целиком, вместе со всеми связями
  - {id: app.storefront, type: component, name: Витрина, relations: [...]}
removed:                   # удаляемые элементы с причиной
  - {id: app.mail-refunds, reason: Возвраты переезжают в сервис возвратов}
```

Целевой процесс с id существующего процесса заменяет его, с новым id добавляет новый. Новому элементу `process` нужен целевой процесс.

Разрыв связывает изменение с причиной:

```yaml
gaps:
  - id: gap.refund-service
    title: Нет сервиса возвратов
    kind: add                # add, change или remove
    description: Витрине нужна форма возврата, а заказам сервис, который меняет их статус
    elements: [app.refunds, app.storefront]
    processes: [biz.refund]
```

Проверка изменения требует:

- Добавляемых id нет в текущей модели, изменяемые и удаляемые есть в файле своего слоя.
- Каждая запись дельты и каждый целевой процесс покрыты хотя бы одним разрывом.
- Разрыв ссылается только на записи дельты и целевые процессы этого изменения.
- Целевая модель, то есть текущая с применённой дельтой, проходит все проверки модели и процессов.
- На удаляемый элемент в целевой модели не осталось ссылок.

Ошибки целевой модели указывают на файл, откуда пришла запись: на дельту для новых и изменённых записей и на файл модели для остальных. Если изменение исправляет ошибку текущей модели, проверка изменения её уже не показывает.

## Дефекты

Проверка находит все дефекты за один проход. Каждый дефект содержит важность (`error` или `warning`), код правила, файл, место (id записи или JSON Pointer) и сообщение. Описание корректно, если в нём нет ошибок.

```
error model/application.yaml app.a [ref.unresolved] связь flow ссылается на несуществующий элемент app.b
```

`rivet architecture validate -json` выводит дефекты массивом JSON с полями `severity`, `rule`, `file`, `location`, `message`. Коды правил стабильны: по ним агенты и тесты понимают, что исправлять. Основные коды:

| Код | Что значит |
|---|---|
| `format.unsupported` | неизвестная версия формата |
| `yaml.parse`, `schema.violation` | файл не разбирается или нарушает схему |
| `id.pattern`, `id.duplicate` | идентификатор не подходит под шаблон или занят |
| `element.type-unknown`, `element.layer` | неизвестный тип или элемент не в своём слое |
| `ref.unresolved` | ссылка на несуществующий элемент |
| `relation.not-allowed`, `relation.mode` | недопустимая связь или режим доступа не у `access` |
| `process.file-missing` | у элемента `process` нет файла BPMN |
| `bpmn.*` | разбор и структура процесса, ссылки `rivet:ref` |
| `viewpoint.*` | определение собственного viewpoint'а |
| `delta.added-exists`, `delta.target-missing` | дельта не применима к текущей модели |
| `delta.uncovered`, `process.uncovered`, `gap.ref-outside` | дельта и разрывы не сходятся |
| `delta.removed-in-use` | удаляемый элемент ещё используется |
| `views.stale` | представления не соответствуют описанию |

## Представления

`rivet architecture views` пишет в `views/` Markdown с диаграммами Mermaid:

- `README.md` с принципами и ссылками на остальные представления.
- По файлу на каждый слой и собственный viewpoint.
- `processes/<id>.md` с блок-схемой каждого процесса.
- `changes/<id>.md` для каждого корректного изменения: видение, целевая модель с подсветкой добавленного, изменённого и удалённого, таблица разрывов и целевые процессы.

Генерация детерминирована: одно и то же описание даёт побайтно одинаковые файлы. Каталог `views/` целиком создаётся генератором, ручные правки в нём перезаписываются. Слой, в котором больше 60 элементов, рисуется отдельной диаграммой на каждый тип.

## Применение изменения

```sh
rivet architecture apply -change refunds
```

Команда заново проверяет изменение, добавляет, заменяет и удаляет записи в файлах модели, кладёт целевые процессы в `processes/`, удаляет BPMN удалённых процессов, переносит папку изменения в `changes/archive/<дата>-<id>/` и перерисовывает `views/`. Файлы правятся на уровне узлов YAML: комментарии и порядок нетронутых записей сохраняются, дифф в PR остаётся минимальным.

Если текущая модель ушла вперёд, например кто-то уже добавил элемент с тем же id, команда перечисляет все конфликты и не меняет ни одного файла.

## Проверка в CI проекта

Команды завершаются с кодом 0, если ошибок нет, 1 при ошибках, конфликтах и устаревших представлениях и 2 при неверных аргументах. Рецепт для GitHub Actions:

```yaml
name: architecture
on:
  pull_request:
    paths: ['docs/architecture/**']
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/checkout@v4
        with:
          repository: PavluninVladimir/rivet
          path: .rivet
      - uses: actions/setup-go@v5
        with:
          go-version-file: .rivet/rivet-service/go.mod
      - run: cd .rivet/rivet-service && go build -o "$RUNNER_TEMP/rivet" ./cmd/rivet
      - run: $RUNNER_TEMP/rivet architecture validate
      - run: $RUNNER_TEMP/rivet architecture views --check
```
