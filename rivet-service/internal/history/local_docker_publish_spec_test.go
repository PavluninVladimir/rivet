package history

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Change local-docker-publish не меняет функциональный код: его единственный
// результат — спецификация. Поэтому критерии приёмки проверяются по тексту
// OpenSpec change'а: тесты читают markdown и убеждаются, что каждый запрет,
// каждое состояние публикации и каждый шаг восстановления зафиксированы явно.
//
// Критерий 1 — change создан и описывает локальную Docker-публикацию и
// API-контракт:
//   TestLocalDockerPublishChangeStructure, TestLocalDockerPublishAPIContract,
//   TestLocalDockerPublishPublishedAPI.
//
// Критерий 2 — bind только на loopback и запрет ручных
// shell/Dockerfile/nginx/token:
//   TestLocalDockerPublishLoopbackOnly,
//   TestLocalDockerPublishNonLoopbackAlwaysProhibited,
//   TestLocalDockerPublishNoManualShellCommands,
//   TestLocalDockerPublishNoManualDockerfileNginxToken.
//
// Критерий 3 — состояния, восстановление после перезапуска Docker и
// безопасный redeploy:
//   TestLocalDockerPublishStates, TestLocalDockerPublishDockerRestartRecovery,
//   TestLocalDockerPublishSafeRedeploy,
//   TestLocalDockerPublishErrorsAndHealthcheck,
//   TestLocalDockerPublishStatePersistence.

const (
	localDockerChangeName = "local-docker-publish"
	localDockerSpecDoc    = "specs/local-docker-publish/spec.md"
)

// localDockerStatuses — полный набор состояний публикации из API-контракта.
var localDockerStatuses = []string{
	"queued", "building", "starting", "verifying", "running",
	"unhealthy", "stopping", "stopped", "failed", "unknown",
}

// localDockerReasonCodes — полный набор кодов причины провала публикации.
var localDockerReasonCodes = []string{
	"config_invalid", "recipe_not_detected", "build_failed",
	"healthcheck_failed", "loopback_not_bound", "port_conflict",
	"docker_unavailable", "container_exited", "image_missing", "timeout",
	"agent_lost",
}

// localDockerChangeDir поднимается от каталога теста до корня рабочей копии,
// в котором лежит OpenSpec store (openspec/changes).
func localDockerChangeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("не удалось определить рабочий каталог теста: %v", err)
	}
	for {
		changes := filepath.Join(dir, "openspec", "changes")
		if st, statErr := os.Stat(changes); statErr == nil && st.IsDir() {
			return filepath.Join(changes, localDockerChangeName)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("OpenSpec store не найден: ожидался каталог openspec/changes в рабочей копии репозитория rivet")
		}
		dir = parent
	}
}

// readChangeDoc читает файл change'а и падает, если его нет или он пуст.
func readChangeDoc(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join(localDockerChangeDir(t), filepath.FromSlash(rel))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("change %s: нет файла %s: %v", localDockerChangeName, rel, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		t.Fatalf("change %s: файл %s пуст", localDockerChangeName, rel)
	}
	return string(raw)
}

// specNormalize схлопывает переносы строк: в markdown одна мысль разложена по
// строкам, а проверять её нужно по смыслу, а не по разметке.
func specNormalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// specMustContain требует наличия всех формулировок (без учёта переносов строк).
func specMustContain(t *testing.T, where, text string, substrings ...string) {
	t.Helper()
	norm := specNormalize(text)
	for _, sub := range substrings {
		if !strings.Contains(norm, specNormalize(sub)) {
			t.Errorf("%s: нет обязательной формулировки %q", where, sub)
		}
	}
}

// specMustNotContain запрещает формулировки: ими проверяются инварианты,
// которые спека не должна разрешать.
func specMustNotContain(t *testing.T, where, text string, substrings ...string) {
	t.Helper()
	norm := specNormalize(text)
	for _, sub := range substrings {
		if strings.Contains(norm, specNormalize(sub)) {
			t.Errorf("%s: спека не должна содержать %q", where, sub)
		}
	}
}

// specRequirement — разобранное требование дельты спеки.
type specRequirement struct {
	title string
	body  string
}

// specRequirements разбирает дельту на требования по заголовкам
// «### Requirement: <название>».
func specRequirements(spec string) []specRequirement {
	const marker = "\n### Requirement: "
	parts := strings.Split(spec, marker)
	requirements := make([]specRequirement, 0, len(parts)-1)
	for _, part := range parts[1:] {
		title, body, _ := strings.Cut(part, "\n")
		for _, stop := range []string{"\n## ", "\n### "} {
			if i := strings.Index(body, stop); i >= 0 {
				body = body[:i]
			}
		}
		requirements = append(requirements, specRequirement{
			title: strings.TrimSpace(title),
			body:  body,
		})
	}
	return requirements
}

// specRequirementByTitle ищет требование по точному названию.
func specRequirementByTitle(t *testing.T, spec, title string) specRequirement {
	t.Helper()
	requirements := specRequirements(spec)
	for _, req := range requirements {
		if req.title == title {
			return req
		}
	}
	titles := make([]string, 0, len(requirements))
	for _, req := range requirements {
		titles = append(titles, req.title)
	}
	sort.Strings(titles)
	t.Fatalf("в %s нет требования %q; есть: %s", localDockerSpecDoc, title, strings.Join(titles, "; "))
	return specRequirement{}
}

// specScenarios возвращает блоки сценариев требования.
func specScenarios(body string) []string {
	parts := strings.Split(body, "#### Scenario: ")
	if len(parts) <= 1 {
		return nil
	}
	return parts[1:]
}

// specRequirementScenario ищет сценарий требования по названию.
func specRequirementScenario(t *testing.T, req specRequirement, title string) string {
	t.Helper()
	scenarios := specScenarios(req.body)
	titles := make([]string, 0, len(scenarios))
	for _, scenario := range scenarios {
		name, _, _ := strings.Cut(strings.TrimSpace(scenario), "\n")
		name = strings.TrimSpace(name)
		if name == title {
			return scenario
		}
		titles = append(titles, name)
	}
	sort.Strings(titles)
	t.Fatalf("у требования %q нет сценария %q; есть: %s", req.title, title, strings.Join(titles, "; "))
	return ""
}

// specParagraph вырезает абзац, начинающийся с prefix, до пустой строки.
func specParagraph(text, prefix string) string {
	i := strings.Index(text, prefix)
	if i < 0 {
		return ""
	}
	text = text[i:]
	if end := strings.Index(text, "\n\n"); end >= 0 {
		text = text[:end]
	}
	return text
}

// specSection вырезает раздел документа от заголовка heading до следующего
// заголовка того же или более высокого уровня.
func specSection(text, heading string) string {
	i := strings.Index(text, heading)
	if i < 0 {
		return ""
	}
	text = text[i+len(heading):]
	for _, stop := range []string{"\n## ", "\n### "} {
		if end := strings.Index(text, stop); end >= 0 {
			text = text[:end]
		}
	}
	return text
}

// specBacktickTokens собирает значения из `…` внутри текста.
func specBacktickTokens(text string) []string {
	parts := strings.Split(text, "`")
	tokens := make([]string, 0, len(parts)/2)
	for i := 1; i < len(parts); i += 2 {
		token := strings.TrimSpace(parts[i])
		if token == "" || strings.ContainsAny(token, " \n\t") {
			continue
		}
		tokens = append(tokens, token)
	}
	return tokens
}

// specTableFields возвращает первые ячейки строк markdown-таблицы.
func specTableFields(body string) []string {
	var fields []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		fields = append(fields, strings.Trim(strings.TrimSpace(cells[1]), "`"))
	}
	return fields
}

func specStringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func specContainsAny(text string, substrings ...string) bool {
	for _, sub := range substrings {
		if strings.Contains(text, sub) {
			return true
		}
	}
	return false
}

// TestLocalDockerPublishChangeStructure — критерий 1: в OpenSpec store есть
// change с описанием локальной Docker-публикации, оформленный по правилам
// store (proposal/design/tasks/дельта спеки со сценариями WHEN/THEN).
func TestLocalDockerPublishChangeStructure(t *testing.T) {
	for _, rel := range []string{
		"proposal.md",
		"design.md",
		"tasks.md",
		"api-contract.md",
		localDockerSpecDoc,
		"specs/deployment/spec.md",
		"specs/operations/spec.md",
	} {
		readChangeDoc(t, rel) // readChangeDoc падает, если файла нет или он пуст
	}

	proposal := readChangeDoc(t, "proposal.md")
	if !strings.HasPrefix(proposal, "# "+localDockerChangeName) {
		t.Errorf("proposal.md: ожидался заголовок %q", "# "+localDockerChangeName)
	}
	specMustContain(t, "proposal.md", proposal,
		"## Зачем",
		"## Что меняется",
		"## Чего не делаем",
		"local_docker",
		"LocalDockerEnvironmentConfig",
		"Dockerfile",
		"nginx",
		"токен",
		"127.0.0.1",
	)

	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"LocalDockerEnvironmentConfig",
		"loopback",
		"127.0.0.1",
		"сверка",
		"Восстановление после перезапуска",
	)

	tasks := readChangeDoc(t, "tasks.md")
	specMustContain(t, "tasks.md", tasks,
		"## rivet",
		"## rivet-web",
		"## rivet-e2e",
		"LocalDockerEnvironmentConfig",
		"loopback",
		"redeploy",
	)
	if !strings.Contains(tasks, "- [ ]") {
		t.Errorf("tasks.md: нет списка задач в формате OpenSpec (- [ ])")
	}

	spec := readChangeDoc(t, localDockerSpecDoc)
	if !strings.HasPrefix(spec, "# "+localDockerChangeName) {
		t.Errorf("%s: ожидался заголовок %q", localDockerSpecDoc, "# "+localDockerChangeName)
	}
	specMustContain(t, localDockerSpecDoc, spec,
		"## ADDED Requirements",
		"LocalDockerEnvironmentConfig",
		"config.local_docker",
		"container_port",
		"health_path",
		"recipe",
		"build_context",
		"startup_timeout_s",
		"stop_timeout_s",
		"api-contract.md",
	)

	requirements := specRequirements(spec)
	if len(requirements) < 10 {
		t.Fatalf("%s: ожидалось не меньше 10 требований, найдено %d", localDockerSpecDoc, len(requirements))
	}
	for _, req := range requirements {
		scenarios := specScenarios(req.body)
		if len(scenarios) == 0 {
			t.Errorf("требование %q описано без сценария WHEN/THEN", req.title)
			continue
		}
		for _, scenario := range scenarios {
			name, _, _ := strings.Cut(strings.TrimSpace(scenario), "\n")
			where := "требование " + req.title + ", сценарий " + strings.TrimSpace(name)
			specMustContain(t, where, scenario, "**WHEN**", "**THEN**")
		}
	}

	// Требования, без которых критерии приёмки не считаются закрытыми.
	for _, title := range []string{
		"Тип окружения local_docker",
		"Проверка конфигурации и разделение типов",
		"Bind только на loopback",
		"Запрет ручных shell-команд",
		"Запрет ручных Dockerfile, nginx-конфигов и токенов",
		"Рецепты сборки образа",
		"Публикация как объект с поколениями",
		"Healthcheck публикации",
		"Состояния публикации и переходы",
		"Обработка ошибок сборки и проверки",
		"Безопасный redeploy",
		"Перезапуск и остановка",
		"Логи публикации",
		"Наблюдение окружения",
		"Сохранение состояния и сверка",
		"Восстановление после перезапуска Docker",
		"API-контракт публикации",
	} {
		specRequirementByTitle(t, spec, title)
	}
}

// TestLocalDockerPublishAPIContract — критерий 1: change описывает
// API-контракт публикации, и перечисленные в нём состояния и коды причин
// совпадают с моделью спеки.
func TestLocalDockerPublishAPIContract(t *testing.T) {
	contract := readChangeDoc(t, "api-contract.md")
	specMustContain(t, "api-contract.md", contract,
		"База: `/api/v1`",
		`{"error": {"code"`,
		`"field"`,
		"POST /api/v1/projects/{id}/environments",
		"PATCH /api/v1/environments/{id}",
		"POST /api/v1/environments/{id}/publish",
		"POST /api/v1/environments/{id}/redeploy",
		"POST /api/v1/environments/{id}/restart",
		"POST /api/v1/environments/{id}/stop",
		"GET /api/v1/environments/{id}/publication",
		"GET /api/v1/environments/{id}/publication/logs",
		"синоним",
	)

	// Состояния публикации: контракт перечисляет ровно ожидаемый набор.
	statusesParagraph := specParagraph(contract, "Статусы (`status`):")
	if statusesParagraph == "" {
		t.Fatalf("api-contract.md: нет перечисления статусов публикации")
	}
	_, statusesList, _ := strings.Cut(statusesParagraph, ":")
	if i := strings.Index(statusesList, "`desired_state`"); i >= 0 {
		statusesList = statusesList[:i]
	}
	contractStatuses := specStringSet(specBacktickTokens(statusesList))
	for _, status := range localDockerStatuses {
		if !contractStatuses[status] {
			t.Errorf("api-contract.md: не описан статус %q", status)
		}
	}
	for status := range contractStatuses {
		if !specContainsAny(strings.Join(localDockerStatuses, " "), status) {
			t.Errorf("api-contract.md: неизвестный статус %q вне модели состояний", status)
		}
	}

	// Коды причин провала: контракт перечисляет ровно ожидаемый набор.
	codesParagraph := specParagraph(contract, "Коды `reason.code`:")
	if codesParagraph == "" {
		t.Fatalf("api-contract.md: нет перечисления кодов reason.code")
	}
	_, codesList, _ := strings.Cut(codesParagraph, ":")
	contractCodes := specStringSet(specBacktickTokens(codesList))
	for _, code := range localDockerReasonCodes {
		if !contractCodes[code] {
			t.Errorf("api-contract.md: не описан код причины %q", code)
		}
	}
	for code := range contractCodes {
		if !specContainsAny(strings.Join(localDockerReasonCodes, " "), code) {
			t.Errorf("api-contract.md: неизвестный код причины %q вне модели ошибок", code)
		}
	}

	spec := readChangeDoc(t, localDockerSpecDoc)
	design := readChangeDoc(t, "design.md")
	for _, status := range localDockerStatuses {
		specMustContain(t, localDockerSpecDoc, spec, status)
	}
	// Полный перечень кодов есть в контракте и дизайне; дельта спеки
	// ссылается на коды тех провалов, которые описывает сценариями.
	for _, code := range localDockerReasonCodes {
		specMustContain(t, "design.md", design, code)
	}
	for _, code := range []string{
		"recipe_not_detected", "build_failed", "healthcheck_failed",
		"loopback_not_bound", "docker_unavailable", "container_exited",
		"timeout", "agent_lost",
	} {
		specMustContain(t, localDockerSpecDoc, spec, code)
	}

	// Дельта deployment связывает local_docker с существующим конвейером.
	deployment := readChangeDoc(t, "specs/deployment/spec.md")
	specMustContain(t, "specs/deployment/spec.md", deployment,
		"## MODIFIED Requirements",
		"### Requirement: Типы окружений публикации",
		"local_docker",
		"### Requirement: Запуск публикации версии",
		"синонимом",
		"### Requirement: Итог публикации и эскалация",
	)
}

// TestLocalDockerPublishPublishedAPI — критерий 1: спека фиксирует набор
// операций публикации, права и форму статуса.
func TestLocalDockerPublishPublishedAPI(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	req := specRequirementByTitle(t, spec, "API-контракт публикации")
	specMustContain(t, "требование API-контракт публикации", req.body,
		"`publish`, `redeploy`, `restart`",
		"`stop`, `status`, `logs`",
		"`desired_state`, `generation`, `listen`",
		"`health_url`, `reason`",
		"`api-contract.md`",
		"участник проекта",
		"администратор",
	)
	scenario := specRequirementScenario(t, req, "Статус публикации")
	specMustContain(t, "сценарий Статус публикации", scenario,
		"GET /api/v1/environments/{id}/publication",
		"ответ 200",
		"`listen`",
	)

	publication := specRequirementByTitle(t, spec, "Публикация как объект с поколениями")
	specMustContain(t, "требование Публикация как объект с поколениями", publication.body,
		"durable",
		"Поколение увеличивается на каждой успешной публикации",
		"не больше одной активной публикации",
		"409",
		"POST /api/v1/environments/{id}/publish",
		"синоним",
	)
}

// TestLocalDockerPublishLoopbackOnly — критерий 2: спека явно требует bind
// только на 127.0.0.1 или ::1 и запрещает non-loopback listen.
func TestLocalDockerPublishLoopbackOnly(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	bind := specRequirementByTitle(t, spec, "Bind только на loopback")
	specMustContain(t, "требование Bind только на loopback", bind.body,
		"исключительно на `127.0.0.1` или `::1`",
		"Порт выбирается платформой",
		"20000–29999",
		"Публикация на `0.0.0.0`, `::`",
		"в любой интерфейс вне loopback ЗАПРЕЩЕНА",
		"`docker inspect`",
		"`HostIp` каждой опубликованной привязки — `127.0.0.1` или `::1`",
		"Пустой `HostIp` читается как `0.0.0.0`",
		"`loopback_not_bound`",
		"`host`, `macvlan`, `ipvlan` и `container:<id>` ЗАПРЕЩЕНЫ",
	)

	badScenario := specRequirementScenario(t, bind, "Демон Docker публикует на все адреса")
	specMustContain(t, "сценарий Демон Docker публикует на все адреса", badScenario,
		"`HostIp` привязки пустой или равен `0.0.0.0`",
		"контейнер останавливается и удаляется",
		"`failed` с кодом `loopback_not_bound`",
		"конфигурация окружения не меняется",
	)

	// Адрес публикации нельзя задать конфигурацией: полей port и host в модели нет.
	model := specRequirementByTitle(t, spec, "Тип окружения local_docker")
	fields := specStringSet(specTableFields(model.body))
	for _, want := range []string{
		"container_port", "health_path", "recipe", "build_context",
		"startup_timeout_s", "stop_timeout_s", "env",
	} {
		if !fields[want] {
			t.Errorf("модель LocalDockerEnvironmentConfig: нет поля %q", want)
		}
	}
	for _, forbidden := range []string{
		"port", "host", "deploy_cmd", "verify_cmd", "verify_url", "dockerfile",
	} {
		if fields[forbidden] {
			t.Errorf("модель LocalDockerEnvironmentConfig: поле %q должно отсутствовать", forbidden)
		}
	}
	specMustContain(t, "требование Тип окружения local_docker", model.body,
		"Поля `port` в модели нет: порт публикации выбирает платформа",
	)

	// Поля адреса и команд отклоняются проверкой конфигурации.
	validation := specRequirementByTitle(t, spec, "Проверка конфигурации и разделение типов")
	foreignField := specRequirementScenario(t, validation, "Поле чужого типа")
	specMustContain(t, "сценарий Поле чужого типа", foreignField,
		"`deploy_cmd`, `verify_cmd`, `verify_url` или `host`",
		"локальная Docker-публикация не принимает ручные команды и адреса",
		"422",
	)

	// Контракт подтверждает отсутствие полей адреса и loopback-адрес в статусе.
	contract := readChangeDoc(t, "api-contract.md")
	specMustContain(t, "api-contract.md", contract,
		"Поля `deploy_cmd`, `verify_cmd`, `verify_url`, `host`, `dockerfile`, `port` в конфигурации `local_docker` отсутствуют",
		`"listen": "127.0.0.1`,
	)

	// Ни в одном месте спека не разрешает публикацию на все интерфейсы.
	specMustNotContain(t, localDockerSpecDoc, spec,
		"-p 0.0.0.0",
		`listen: "0.0.0.0`,
		"--network host",
	)
}

// TestLocalDockerPublishNonLoopbackAlwaysProhibited — критерий 2: любое
// упоминание 0.0.0.0 в спеке и дизайне стоит в контексте запрета, нарушения
// или проверки отсутствия такого сокета, а не разрешения.
func TestLocalDockerPublishNonLoopbackAlwaysProhibited(t *testing.T) {
	for _, rel := range []string{localDockerSpecDoc, "design.md", "api-contract.md"} {
		text := readChangeDoc(t, rel)
		found := 0
		for _, paragraph := range strings.Split(text, "\n\n") {
			if !strings.Contains(paragraph, "0.0.0.0") {
				continue
			}
			found++
			allowed := specContainsAny(specNormalize(paragraph),
				"ЗАПРЕЩ", "нарушени", "loopback_not_bound",
				"останавливается", "loopback-only", "не содержит")
			if !allowed {
				t.Errorf("%s: 0.0.0.0 упомянут вне запрета: %q", rel, specNormalize(paragraph))
			}
		}
		if rel != "api-contract.md" && found == 0 {
			t.Errorf("%s: нет упоминания 0.0.0.0 — запрет non-loopback listen не зафиксирован", rel)
		}
	}
}

// TestLocalDockerPublishNoManualShellCommands — критерий 2: спека запрещает
// shell-команды пользователя и фиксирует закрытый набор операций runner'а.
func TestLocalDockerPublishNoManualShellCommands(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	req := specRequirementByTitle(t, spec, "Запрет ручных shell-команд")
	specMustContain(t, "требование Запрет ручных shell-команд", req.body,
		"НЕ ДОЛЖНА исполнять команды, заданные пользователем",
		"Полей `deploy_cmd`, `verify_cmd`, `monitor.check_cmd`, `monitor.logs_cmd` у `local_docker` нет",
		"ошибка 422",
		"фиксированный набор операций платформы",
		"`docker build`, `docker run`, `docker stop`, `docker rm`, `docker inspect`",
		"Аргументы этих операций собираются платформой",
		"без shell",
	)

	commandScenario := specRequirementScenario(t, req, "Команда в конфигурации")
	specMustContain(t, "сценарий Команда в конфигурации", commandScenario,
		"`deploy_cmd`",
		"ответ 422",
		"окружение не создаётся",
	)
	monitorScenario := specRequirementScenario(t, req, "Команда наблюдения")
	specMustContain(t, "сценарий Команда наблюдения", monitorScenario,
		"`check_cmd` или `logs_cmd`",
		"ответ 422",
	)

	// Наблюдение окружения тоже без команд.
	operations := readChangeDoc(t, "specs/operations/spec.md")
	specMustContain(t, "specs/operations/spec.md", operations,
		"`monitor.check_cmd`, `monitor.logs_cmd`",
		"ЗАПРЕЩЕНЫ: ответ 422",
		"Control plane ДОЛЖЕН продолжать отклонять проверки loopback и частных адресов",
	)

	// Дизайн фиксирует отсутствие команд как решение, а не как настройку.
	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"Команды в нём нет вообще",
		"`deploy_cmd` — произвольный shell",
		"код платформы, не текст от пользователя",
	)
}

// TestLocalDockerPublishNoManualDockerfileNginxToken — критерий 2: спека
// запрещает ручные Dockerfile, nginx-конфиги и токены.
func TestLocalDockerPublishNoManualDockerfileNginxToken(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	req := specRequirementByTitle(t, spec, "Запрет ручных Dockerfile, nginx-конфигов и токенов")
	specMustContain(t, "требование Запрет ручных Dockerfile, nginx-конфигов и токенов", req.body,
		"НЕ ДОЛЖЕН быть обязан",
		"НЕ ДОЛЖЕН иметь возможности задавать Dockerfile, конфигурацию reverse proxy и токены",
		"Dockerfile генерирует платформа по рецепту во временном каталоге runner'а",
		"файл в репозиторий не пишется, из конфигурации не принимается",
		"Reverse proxy не нужен",
		"nginx-конфиг не принимается и не генерируется",
		"Токены не нужны",
		"git-токен для локальной публикации не запрашиваются",
	)

	dockerfileScenario := specRequirementScenario(t, req, "Пользователь присылает Dockerfile")
	specMustContain(t, "сценарий Пользователь присылает Dockerfile", dockerfileScenario,
		"`dockerfile`",
		"ответ 422",
		"неизвестное поле dockerfile",
	)
	noDockerfileScenario := specRequirementScenario(t, req, "Репозиторий без Dockerfile")
	specMustContain(t, "сценарий Репозиторий без Dockerfile", noDockerfileScenario,
		"`go.mod`",
		"рецепту `go`",
		"`running`",
	)
	registryScenario := specRequirementScenario(t, req, "Публикация без registry и токена")
	specMustContain(t, "сценарий Публикация без registry и токена", registryScenario,
		"образ остаётся локальным",
		"токен registry не запрашивается",
	)

	// Рецепты сборки — код платформы: Dockerfile не приходит из конфигурации.
	recipes := specRequirementByTitle(t, spec, "Рецепты сборки образа")
	specMustContain(t, "требование Рецепты сборки образа", recipes.body,
		"`go`, `node`, `static`",
		"`go.mod` → `package.json` → `index.html`",
		"закреплены digest'ом",
		"Аргументы сборки, `build-args` и секреты в сборку не передаются",
		"`recipe_not_detected`",
	)

	// Веб-консоль не показывает полей Dockerfile, nginx и токенов.
	web := specRequirementByTitle(t, spec, "Веб-консоль локальной публикации")
	specMustContain(t, "требование Веб-консоль локальной публикации", web.body,
		"карточку публикации",
		"«Опубликовать»",
		"«Пересобрать»",
		"«Перезапустить»",
		"«Остановить»",
		"«Логи»",
		"не содержит полей Dockerfile, nginx, токенов и команд",
	)

	// Контракт: ручные поля отклоняются ответом 422.
	contract := readChangeDoc(t, "api-contract.md")
	specMustContain(t, "api-contract.md", contract,
		"в конфигурации `local_docker` отсутствуют: 422 `invalid` с указанием поля",
		"`monitor.check_cmd` и `monitor.logs_cmd` — 422",
	)

	// Дизайн: Dockerfile генерируется во временном каталоге и не попадает в репозиторий.
	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"Dockerfile генерируется во временном каталоге runner'а, в репозиторий не пишется",
	)
}

// TestLocalDockerPublishStates — критерий 3: спека описывает состояния
// публикации, разделение желаемого и наблюдаемого состояния и переходы.
func TestLocalDockerPublishStates(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	req := specRequirementByTitle(t, spec, "Состояния публикации и переходы")
	specMustContain(t, "требование Состояния публикации и переходы", req.body,
		localDockerStatuses...,
	)
	specMustContain(t, "требование Состояния публикации и переходы", req.body,
		"Раздельно хранятся `desired_state`",
		"`running` или `stopped`",
		"`queued → building`",
		"`building → starting`",
		"`starting → verifying`",
		"`verifying → running`",
		"`building | starting | verifying → failed`",
		"`running → stopping → stopped`",
		"`running → unhealthy`",
		"`unhealthy → running`",
		"`running | unhealthy → failed`",
		"`failed → queued`",
		"Переходы делаются CAS-ом по `publication_id` и `runner_id`",
		"устаревший результат отбрасывается и состояние не меняет",
	)

	happy := specRequirementScenario(t, req, "Успешный путь")
	specMustContain(t, "сценарий Успешный путь", happy,
		"`queued → building → starting → verifying → running`",
		"каждое действие оставляет событие в журнале",
	)
	stale := specRequirementScenario(t, req, "Устаревший результат runner'а")
	specMustContain(t, "сценарий Устаревший результат runner'а", stale,
		"уже финализирован",
		"статус не меняется",
	)

	// Состояния согласованы с контрактом и дизайном.
	contract := readChangeDoc(t, "api-contract.md")
	for _, status := range localDockerStatuses {
		specMustContain(t, "api-contract.md", contract, status)
	}
	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"desired_state",
		"status",
		"generation",
		"Расхождение лечится сверкой",
	)
}

// TestLocalDockerPublishDockerRestartRecovery — критерий 3: спека описывает
// восстановление после перезапуска Docker.
func TestLocalDockerPublishDockerRestartRecovery(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	req := specRequirementByTitle(t, spec, "Восстановление после перезапуска Docker")
	specMustContain(t, "требование Восстановление после перезапуска Docker", req.body,
		"`--restart unless-stopped`",
		"без `--rm`",
		"restart policy `no`",
		"найти контейнер по меткам",
		"проверить привязку `127.0.0.1`/`::1`",
		"выполнить healthcheck",
		"до трёх попыток с паузами 5, 15, 30 с",
		"`failed` с кодом `container_exited`",
	)

	restarted := specRequirementScenario(t, req, "Docker перезапустился")
	specMustContain(t, "сценарий Docker перезапустился", restarted,
		"демон Docker перезапущен",
		"`status: running`",
		"тот же `listen`",
	)
	notStarted := specRequirementScenario(t, req, "Контейнер не поднялся")
	specMustContain(t, "сценарий Контейнер не поднялся", notStarted,
		"после трёх попыток",
		"`status: failed`",
		"`container_exited`",
		"эскалация",
	)
	stopped := specRequirementScenario(t, req, "Остановленную публикацию не воскрешаем")
	specMustContain(t, "сценарий Остановленную публикацию не воскрешаем", stopped,
		"`stopped`",
		"контейнер не запускается",
	)

	// Дизайн фиксирует механику восстановления: метки, restart policy, сверка.
	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"Восстановление после перезапуска",
		"контейнер создаётся с `--restart unless-stopped` и без `--rm`",
		"паузы 5, 15, 30 с",
		"rivet.managed=true",
	)
}

// TestLocalDockerPublishSafeRedeploy — критерий 3: безопасный redeploy без
// простоя и с сохранением работающего поколения при провале.
func TestLocalDockerPublishSafeRedeploy(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	req := specRequirementByTitle(t, spec, "Безопасный redeploy")
	specMustContain(t, "требование Безопасный redeploy", req.body,
		"без простоя работающей версии",
		"новое поколение поднимается на свободном loopback-порту",
		"проходит healthcheck",
		"получает SIGTERM (SIGKILL через `stop_timeout_s`)",
		"Провал нового поколения оставляет работать предыдущее",
		"`status` и `listen` остаются от него",
		"Параметр `version` берёт версию из истории публикаций окружения",
		"`no_publication`",
		"последние два",
	)

	sameVersion := specRequirementScenario(t, req, "Redeploy той же версии")
	specMustContain(t, "сценарий Redeploy той же версии", sameVersion,
		"новом порту",
		"после успешного healthcheck",
		"простой не возникает",
	)
	broken := specRequirementScenario(t, req, "Redeploy сломанной версии")
	specMustContain(t, "сценарий Redeploy сломанной версии", broken,
		"работает прежнее поколение",
		"`status: running`",
		"`listen` не меняется",
		"`failed` с причиной",
	)
	outOfHistory := specRequirementScenario(t, req, "Версия вне истории")
	specMustContain(t, "сценарий Версия вне истории", outOfHistory,
		"ответ 422",
		"version: нет в истории публикаций окружения",
	)

	// Контракт: redeploy возвращает публикацию и не двигает поколение зря.
	contract := readChangeDoc(t, "api-contract.md")
	redeploy := specSection(contract, "### `POST /api/v1/environments/{id}/redeploy`")
	if redeploy == "" {
		t.Fatalf("api-contract.md: нет раздела redeploy")
	}
	specMustContain(t, "api-contract.md, redeploy", redeploy,
		"Пересобирает и публикует версию без простоя работающего поколения",
		"`generation` растёт только после успешного переключения",
		"409 `no_publication`",
		"422 `invalid`",
	)

	// Дизайн: blue-green на loopback как выбранное решение.
	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"Blue-green на loopback",
		"Новое поколение",
		"SIGTERM",
		"`stop_timeout_s`",
	)
}

// TestLocalDockerPublishErrorsAndHealthcheck — критерии 1 и 3: обработка
// ошибок сборки и проверки, healthcheck и логи публикации.
func TestLocalDockerPublishErrorsAndHealthcheck(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)

	errors := specRequirementByTitle(t, spec, "Обработка ошибок сборки и проверки")
	specMustContain(t, "требование Обработка ошибок сборки и проверки", errors.body,
		"завершать публикацию детерминированно",
		"финальный статус `failed`",
		"код и русский текст в `reason`",
		"ставить автопубликации окружения на паузу и создавать эскалацию",
		"Работающая предыдущая версия при провале НЕ ДОЛЖНА останавливаться",
	)
	buildFailed := specRequirementScenario(t, errors, "Ошибка сборки")
	specMustContain(t, "сценарий Ошибка сборки", buildFailed,
		"ненулевым кодом",
		"`build_failed`",
		"старый контейнер продолжает работать",
		"автопубликации на паузе",
	)
	dockerUnavailable := specRequirementScenario(t, errors, "Нет доступа к Docker")
	specMustContain(t, "сценарий Нет доступа к Docker", dockerUnavailable,
		"`docker_unavailable`",
		"не держит вечный «building»",
	)
	deadline := specRequirementScenario(t, errors, "Дедлайн джобы")
	specMustContain(t, "сценарий Дедлайн джобы", deadline, "`timeout`")

	health := specRequirementByTitle(t, spec, "Healthcheck публикации")
	specMustContain(t, "требование Healthcheck публикации", health.body,
		"`http://127.0.0.1:<port><health_path>`",
		"200–399",
		"Таймаут одного запроса 2 с",
		"не больше 4 КиБ",
		"redirect не отслеживается",
		"`startup_timeout_s`",
		"`healthcheck_failed`",
		"`monitor.check_url` платформа заполняет сама из `listen`",
	)
	notAnswering := specRequirementScenario(t, health, "Приложение не отвечает")
	specMustContain(t, "сценарий Приложение не отвечает", notAnswering,
		"новый контейнер удаляется",
		"`failed` с кодом `healthcheck_failed`",
	)

	logs := specRequirementByTitle(t, spec, "Логи публикации")
	specMustContain(t, "требование Логи публикации", logs.body,
		"`stream=builder`",
		"`stream=container`",
		"максимум 5000",
		"2 МиБ",
		"Логов нет — 404",
		"Секретов в логах нет",
	)
}

// TestLocalDockerPublishStatePersistence — критерий 3: состояние публикации
// durable, восстанавливается после перезапуска rivetd и сверяется с Docker.
func TestLocalDockerPublishStatePersistence(t *testing.T) {
	spec := readChangeDoc(t, localDockerSpecDoc)
	state := specRequirementByTitle(t, spec, "Сохранение состояния и сверка")
	specMustContain(t, "требование Сохранение состояния и сверка", state.body,
		"храниться в БД",
		"восстанавливаться после перезапуска rivetd",
		"`rivet.managed`, `rivet.project`, `rivet.environment`, `rivet.publication`",
		"`rivet.version`, `rivet.generation`",
		"приводить `status` к фактическому состоянию",
		"после грейса 10 минут",
		"удалять его контейнеры и образы",
	)
	rivetd := specRequirementScenario(t, state, "Перезапуск rivetd")
	specMustContain(t, "сценарий Перезапуск rivetd", rivetd,
		"`building|starting|verifying` с истёкшим дедлайном",
		"`failed` с кодом `agent_lost`",
		"`running` и `stopped` восстанавливаются сверкой без пересборки",
	)
	orphan := specRequirementScenario(t, state, "Осиротевший контейнер")
	specMustContain(t, "сценарий Осиротевший контейнер", orphan,
		"дольше 10 минут",
		"контейнер останавливается и удаляется",
	)

	design := readChangeDoc(t, "design.md")
	specMustContain(t, "design.md", design,
		"строки `building|starting|verifying` с истёкшим `deadline_at`",
		"`agent_lost`",
		"повторная доставка результата идемпотентна",
	)
}
