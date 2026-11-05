package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/runner/native"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Проверки проекта на стадии TESTING (ревью интеграции add-native-agent,
// P2-5). Они исполняют код из ветки задачи, а его написала модель
// (разработчик или тестировщик). Поэтому проверки идут тем же
// исполнителем, что и команды агента (native.RunCommand):
//
//   - окружение из белого списка инструмента run: PATH, LANG=C.UTF-8,
//     TERM=dumb, CI=1, NO_COLOR=1, GIT_TERMINAL_PROMPT=0, временные HOME и
//     TMPDIR (общие для проверок стадии), GOCACHE в нём, GOMODCACHE и
//     npm_config_cache на кеши runner'а. Других переменных runner'а у
//     проверок нет;
//   - в песочнице без сети, $HOME runner'а и файл токена скрыты, кеши
//     зависимостей и каталоги -sandbox-ro только на чтение: зависимости
//     готовятся до стадии. С -checks-network проверки получают сеть хоста,
//     остальная изоляция та же;
//   - фоновые процессы проверки завершаются после её выхода;
//   - предела времени у одной проверки нет, их ограничивает стадия (как
//     до этого change).
//
// Без песочницы проверки идут только у runner'а, запущенного с
// -unsandboxed (тот же флаг, что у агента); иначе стадия неуспешна с
// причиной, а проверки не запускаются.
//
// Проверка, упавшая в песочнице из-за окружения (код 127: инструмента нет
// в песочнице; типовые сетевые ошибки при песочнице без сети), — сбой
// инфраструктуры, а не вердикт по коду: разработчик в той же песочнице
// окружение не починит. Стадия отдаёт вердикт error, шаг повторяется без
// расхода попытки, повторный сбой эскалирует человеку с подсказкой.

// sandboxAvailable — есть ли песочница команд (подмена в тестах).
var sandboxAvailable = native.NetworkIsolationAvailable

// checkReportTail — сколько вывода каждой проверки попадает в отчёт стадии.
const checkReportTail = 4000

// checkTranscriptCap — сколько вывода одной проверки уходит в транскрипт:
// зациклившаяся проверка не забьёт outbox runner'а.
const checkTranscriptCap = 8 << 20

// errChecksEnvironment — проверка упала из-за окружения песочницы, а не
// из-за кода: сбой инфраструктуры стадии.
var errChecksEnvironment = errors.New("проверке нужна сеть или инструмент вне рабочей копии, см. README: подготовка кешей и флаги runner'а")

// networkFailureMarkers — типовые сетевые ошибки в выводе (в нижнем
// регистре): DNS, недоступная сеть, отказ соединения у клиентов Go, curl,
// git, npm, pip, JVM.
var networkFailureMarkers = []string{
	"network is unreachable",
	"network unreachable",
	"enetunreach",
	"could not resolve host",
	"could not resolve hostname",
	"temporary failure in name resolution",
	"name or service not known",
	"no such host",
	"enotfound",
	"eai_again",
	"getaddrinfo",
	"dial tcp",
	"dial udp",
	"failed to establish a new connection",
	"unknownhostexception",
}

// checkEnvFailure — причина, по которой упавшая проверка считается сбоем
// окружения песочницы (пусто — вердикт по коду). Код 127 в песочнице —
// команда не найдена: инструмент под скрытым $HOME runner'а. Сетевая
// ошибка — только при песочнице без сети.
func checkEnvFailure(res native.CommandResult) string {
	if res.ExitCode == 0 || !res.Sandboxed {
		return ""
	}
	if res.ExitCode == 127 {
		return "код выхода 127: команда не найдена в песочнице (инструмент под $HOME runner'а скрыт)"
	}
	if res.Isolated {
		out := strings.ToLower(res.Output)
		for _, m := range networkFailureMarkers {
			if strings.Contains(out, m) {
				return fmt.Sprintf("сетевая ошибка в песочнице без сети (%q)", m)
			}
		}
	}
	return ""
}

// runChecks прогоняет проверки проекта в песочнице агента. ok — все
// проверки прошли, report — отчёт для стадии. err — проверки не
// запускались, прерваны или упали из-за окружения песочницы (нет
// песочницы без -unsandboxed, сбой песочницы, отмена стадии,
// errChecksEnvironment): это сбой инфраструктуры, а не вердикт по коду.
// transcript получает вывод потоком.
func (a *agent) runChecks(ctx context.Context, dir string, checks []*pb.Check, step func(string), transcript func([]byte)) (ok bool, report string, err error) {
	if len(checks) == 0 {
		return true, "нет настроенных проверок", nil
	}
	sandbox := sandboxAvailable()
	if !sandbox && !a.cfg.Unsandboxed {
		return false, "", fmt.Errorf("проверки проекта исполняют код из ветки задачи, а песочница команд недоступна (%s): "+
			"без неё они пошли бы с правами runner'а; запусти runner на Linux с user namespaces, в отдельной VM или контейнере "+
			"либо, если это осознанно, с -unsandboxed", native.SandboxUnavailableReason())
	}
	switch {
	case sandbox && a.cfg.ChecksNetwork:
		step("проверки проекта в песочнице с сетью (-checks-network)")
	case sandbox:
		step("проверки проекта в песочнице без сети")
	default:
		step("проверки проекта без песочницы (-unsandboxed): с правами runner'а и сетью")
	}
	home, err := os.MkdirTemp("", "rivet-checks-home-*")
	if err != nil {
		return false, "", fmt.Errorf("временный HOME проверок: %w", err)
	}
	defer native.RemoveTree(home)
	var b strings.Builder
	ok = true
	for _, c := range checks {
		step("проверка: " + c.Name)
		res, err := native.RunCommand(ctx, native.CommandSpec{
			Dir: dir, Command: c.Cmd, Home: home,
			Sandbox: true, RequireSandbox: !a.cfg.Unsandboxed, Network: a.cfg.ChecksNetwork,
			ReadOnlyCaches: a.cfg.sandboxReadOnly(), HidePaths: a.cfg.sandboxHidePaths(),
			MaxOutput: 2 * checkReportTail, OnOutput: cappedOutput(transcript, checkTranscriptCap),
		})
		fmt.Fprintf(&b, "== %s ==\n%s\n", c.Name, tail(res.Output, checkReportTail))
		if err != nil {
			fmt.Fprintf(&b, "проверка не выполнена: %v\n", err)
			return false, b.String(), fmt.Errorf("проверка %s: %w", c.Name, err)
		}
		// Причина и подсказка — в ошибке: стадия добавит её к отчёту.
		if cause := checkEnvFailure(res); cause != "" {
			return false, b.String(), fmt.Errorf("проверка %s: %s: %w", c.Name, cause, errChecksEnvironment)
		}
		if res.ExitCode != 0 {
			ok = false
			fmt.Fprintf(&b, "FAIL: код выхода %d\n", res.ExitCode)
		}
		if res.Leftovers > 0 {
			fmt.Fprintf(&b, "фоновых процессов проверки завершено после выхода: %d\n", res.Leftovers)
		}
	}
	return ok, b.String(), nil
}

// cappedOutput — поток вывода с пределом: после limit байт в транскрипт
// уходит одна пометка, остальное отбрасывается.
func cappedOutput(out func([]byte), limit int) func([]byte) {
	if out == nil {
		return nil
	}
	n := 0
	return func(b []byte) {
		if n >= limit {
			return
		}
		if n+len(b) > limit {
			out(b[:limit-n])
			out([]byte("\n… [вывод проверки в транскрипте обрезан]\n"))
			n = limit
			return
		}
		n += len(b)
		out(b)
	}
}

// sandboxHidePaths — что скрыть от команд агента и проверок в песочнице:
// домашний каталог runner'а (под тем же uid команда видела бы его файлы) и
// файлы секретов (файл токена).
func (c Config) sandboxHidePaths() []string {
	var out []string
	add := func(p string) {
		// Помощник песочницы работает в рабочей копии: относительный путь
		// он искал бы там, поэтому все пути абсолютные.
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if p != "" && p != "/" {
			out = append(out, p)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(home)
	}
	for _, f := range c.SecretFiles {
		if f != "" {
			add(f)
		}
	}
	return out
}

// sandboxReadOnly — что в песочнице видно только на чтение: кеши
// зависимостей runner'а (через их переменные), каталоги -sandbox-ro (без
// переменной: путь остаётся прежним, обычно он уже в PATH) и бинарь
// runner'а в RIVET_RUNNER_BIN (add-architecture-c4). Бинарь монтируется
// одним файлом: соседние файлы под скрытым $HOME команда не видит.
func (c Config) sandboxReadOnly() []native.CacheDir {
	out := dependencyCaches()
	for _, p := range c.SandboxReadOnly {
		out = append(out, native.CacheDir{Dir: p})
	}
	if c.RunnerBin != "" {
		out = append(out, native.CacheDir{Env: "RIVET_RUNNER_BIN", Dir: c.RunnerBin})
	}
	return out
}

// CheckSandboxReadOnly проверяет каталоги -sandbox-ro при старте runner'а
// и возвращает их абсолютными путями. Каталог открывается командам модели,
// поэтому нельзя:
//   - $HOME runner'а и каталоги выше него: песочница перестала бы скрывать
//     его файлы;
//   - файл секретов (файл токена), его каталог и каталоги выше;
//   - рабочий каталог runner'а, каталоги выше и внутри него: там рабочие
//     копии других задач и outbox.
//
// Пути сравниваются и как заданы, и после раскрытия символических ссылок.
func CheckSandboxReadOnly(paths []string, home, workdir string, secretFiles []string) ([]string, error) {
	type guard struct {
		path, what string
		inside     bool // запрещены и каталоги внутри
	}
	var guards []guard
	addGuard := func(p, what string, inside bool) {
		if p == "" {
			return
		}
		for _, v := range pathVariants(p) {
			guards = append(guards, guard{v, what, inside})
		}
	}
	addGuard(home, "домашний каталог runner'а", false)
	for _, f := range secretFiles {
		addGuard(f, "файл секретов runner'а", false)
		if f != "" {
			addGuard(filepath.Dir(f), "каталог файла секретов runner'а", false)
		}
	}
	addGuard(workdir, "рабочий каталог runner'а", true)
	var out []string
	seen := map[string]bool{}
	for _, raw := range paths {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		abs, err := filepath.Abs(raw)
		if err != nil {
			return nil, fmt.Errorf("-sandbox-ro %s: %w", raw, err)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("-sandbox-ro %s: %w", raw, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("-sandbox-ro %s: нужен каталог", raw)
		}
		for _, v := range pathVariants(abs) {
			for _, g := range guards {
				if v == g.path || isUnder(g.path, v) || (g.inside && isUnder(v, g.path)) {
					return nil, fmt.Errorf("-sandbox-ro %s: каталог открыл бы песочнице %s (%s); укажи каталог с инструментами или кешем, а не выше него", raw, g.what, g.path)
				}
			}
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out, nil
}

// pathVariants — абсолютный путь как задан и после раскрытия символических
// ссылок (если он существует).
func pathVariants(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	out := []string{abs}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		out = append(out, real)
	}
	return out
}

// isUnder — path лежит внутри dir (не совпадает с ним).
func isUnder(path, dir string) bool {
	if dir == string(filepath.Separator) {
		return path != dir
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}
