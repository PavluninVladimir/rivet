// Package archcli — подкоманда «rivet-runner architecture» (add-architecture-c4):
// проверка, генерация представлений и гейт шага test в рабочей копии
// задачи. Тот же код, что CLI rivet, но в бинаре runner'а: проверка
// приходит runner'у командой "$RIVET_RUNNER_BIN" architecture gate.
package archcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
)

// Коды выхода: 0 — успех, 1 — дефекты или проваленный гейт, 2 — неверные
// аргументы или сбой.
const (
	ExitOK      = 0
	ExitDefects = 1
	ExitUsage   = 2
)

const usage = "usage: rivet-runner architecture <init|validate|views|gate> [-dir " + architecture.DefaultDir + "] " +
	"[-system имя] [-check] [-base origin/main] [-force] [-only-docs]"

// Main выполняет подкоманду и возвращает код выхода.
func Main(args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(out, usage)
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("architecture "+sub, flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("dir", architecture.DefaultDir, "каталог описания архитектуры")
	system := fs.String("system", "", "название системы для init")
	check := fs.Bool("check", false, "views: проверить актуальность, не записывая")
	base := fs.String("base", "", "gate: базовая версия, например origin/main")
	force := fs.Bool("force", false, "gate: проверять описание, даже если ветка его не меняла")
	onlyDocs := fs.Bool("only-docs", false, "gate: ветка не должна менять файлы вне каталога описания")
	origin := fs.String("origin", "", "gate: адрес репозитория из конфигурации проекта (а не из .git/config рабочей копии)")
	if err := fs.Parse(rest); err != nil {
		return ExitUsage
	}
	switch sub {
	case "init":
		name := *system
		if name == "" {
			wd, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(out, err)
				return ExitUsage
			}
			name = filepath.Base(wd)
		}
		if err := architecture.Init(*dir, name); err != nil {
			fmt.Fprintln(out, err)
			return ExitUsage
		}
		fmt.Fprintf(out, "описание архитектуры создано в %s\n", *dir)
		return ExitOK
	case "validate":
		ds, err := validate(*dir)
		return report(out, ds, err)
	case "views":
		if *check {
			ds, err := checkViews(*dir)
			return report(out, ds, err)
		}
		ds, err := architecture.WriteViews(*dir)
		return report(out, ds, err)
	case "gate":
		if *base == "" {
			fmt.Fprintln(out, "gate: нужен -base")
			return ExitUsage
		}
		return GateOrigin(out, ".", *dir, *base, *origin, *force, *onlyDocs)
	}
	fmt.Fprintf(out, "неизвестная подкоманда %q\n%s\n", sub, usage)
	return ExitUsage
}

func validate(dir string) (architecture.Defects, error) {
	fsys, closer, err := architecture.OpenDir(dir)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return architecture.Validate(fsys)
}

func checkViews(dir string) (architecture.Defects, error) {
	fsys, closer, err := architecture.OpenDir(dir)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return architecture.CheckViews(fsys)
}

// report печатает дефекты и превращает ошибки в код выхода. Отсутствие
// описания ошибкой не считается: описание у проекта необязательно.
func report(out io.Writer, ds architecture.Defects, err error) int {
	if errors.Is(err, architecture.ErrAbsent) {
		fmt.Fprintln(out, "описания архитектуры нет")
		return ExitOK
	}
	if err != nil {
		fmt.Fprintln(out, err)
		return ExitUsage
	}
	for _, d := range ds {
		fmt.Fprintln(out, d.String())
	}
	errs, warns := ds.Count()
	fmt.Fprintf(out, "ошибок: %d, предупреждений: %d\n", errs, warns)
	if ds.HasErrors() {
		return ExitDefects
	}
	return ExitOK
}

// Gate — проверка архитектуры шага test (спека project-architecture
// «Проверка архитектуры на шаге test»). Без force проверка срабатывает,
// только если ветка меняет описание. onlyDocs проваливает ветку, которая
// меняет файлы вне каталога описания. repo — корень рабочей копии.
func Gate(out io.Writer, repo, dir, base string, force, onlyDocs bool) int {
	return GateOrigin(out, repo, dir, base, "", force, onlyDocs)
}

// GateOrigin — Gate с адресом репозитория из конфигурации проекта: база
// забирается с него, а не с remote origin из .git/config рабочей копии,
// который мог переписать агент (ревью Opus, круг 3).
func GateOrigin(out io.Writer, repo, dir, base, origin string, force, onlyDocs bool) int {
	changed, err := changedFiles(repo, base, origin)
	if err != nil {
		fmt.Fprintln(out, "gate: изменения ветки не получены:", err)
		return ExitUsage
	}
	prefix := strings.TrimSuffix(filepath.ToSlash(dir), "/") + "/"
	var docs, outside []string
	for _, f := range changed {
		if strings.HasPrefix(f, prefix) {
			docs = append(docs, f)
		} else {
			outside = append(outside, f)
		}
	}
	if onlyDocs && len(outside) > 0 {
		fmt.Fprintf(out, "задача описания меняет файлы вне %s:\n", strings.TrimSuffix(prefix, "/"))
		for _, f := range outside {
			fmt.Fprintln(out, "  "+f)
		}
		return ExitDefects
	}
	if !force && len(docs) == 0 {
		fmt.Fprintln(out, "описание архитектуры в ветке не менялось: проверка не нужна")
		return ExitOK
	}
	full := filepath.Join(repo, dir)
	// Символическая ссылка внутри каталога описания: локально она
	// читается, а снимок через API хостинга её не принимает, и описание
	// после merge стало бы некорректным (ревью Opus, финальная проверка).
	if links := symlinks(full); len(links) > 0 {
		fmt.Fprintln(out, "в каталоге описания символические ссылки, замените их файлами:")
		for _, l := range links {
			fmt.Fprintln(out, "  file.symlink "+filepath.ToSlash(filepath.Join(dir, l)))
		}
		return ExitDefects
	}
	ds, err := validate(full)
	if errors.Is(err, architecture.ErrAbsent) {
		fmt.Fprintf(out, "описания архитектуры нет в %s: создайте его командой \"$RIVET_RUNNER_BIN\" architecture init\n", dir)
		return ExitDefects
	}
	if code := report(out, ds, err); code != ExitOK {
		return code
	}
	stale, err := checkViews(full)
	if err != nil {
		fmt.Fprintln(out, err)
		return ExitUsage
	}
	if stale.HasErrors() {
		fmt.Fprintln(out, "представления устарели, перегенерируйте их: \"$RIVET_RUNNER_BIN\" architecture views")
		for _, d := range stale {
			fmt.Fprintln(out, d.String())
		}
		return ExitDefects
	}
	fmt.Fprintln(out, "описание архитектуры корректно, представления актуальны")
	return ExitOK
}

// changedFiles — файлы, изменённые в ветке относительно общего предка с
// базой, включая незакоммиченные и неотслеживаемые. Пути от корня
// репозитория, отсортированы. Переименование даёт оба пути: иначе перенос
// кода в каталог описания прошёл бы -only-docs, а перенос описания наружу
// выглядел бы как «описание не менялось».
func changedFiles(repo, base, origin string) ([]string, error) {
	base, err := trustedBase(repo, base, origin)
	if err != nil {
		return nil, err
	}
	mb, err := git(repo, "merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	diff, err := git(repo, "diff", "--no-renames", "--name-only", "-z", strings.TrimSpace(mb))
	if err != nil {
		return nil, err
	}
	untracked, err := git(repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Split(diff+untracked, "\x00") {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out, nil
}

// symlinks — символические ссылки в каталоге (пути от него, отсортированы).
func symlinks(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if rel, rerr := filepath.Rel(root, path); rerr == nil {
				out = append(out, rel)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// gateBaseRef — ref, в который гейт сам забирает базовую ветку.
const gateBaseRef = "refs/rivet/gate-base"

// standardFetch — refspec remote.origin.fetch, который ставит git clone.
const standardFetch = "+refs/heads/*:refs/remotes/origin/*"

// trustedBase — база, которой можно верить, хотя .git рабочей копии мог
// оставить агент (ревью Opus, круги 2 и 3). База origin/<ветка> забирается
// явным refspec в собственный ref гейта: с адреса из конфигурации проекта
// (origin), а без него с remote origin. Подменённый refspec её не сдвинет,
// хуки и подмены объектов отключены (см. git). Переписывание адресов
// url.*.insteadOf в конфигурации рабочей копии — отказ. Без сети
// остаётся origin/<ветка>, но только при стандартном refspec и адресе
// remote origin, совпадающем с адресом проекта. Другие базы (локальные
// проверки) идут как есть.
func trustedBase(repo, base, origin string) (string, error) {
	branch, ok := strings.CutPrefix(base, "origin/")
	if !ok {
		return base, nil
	}
	if strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " \t\n:^~?*[\\") {
		return "", fmt.Errorf("недопустимая базовая ветка %q", branch)
	}
	if strings.HasPrefix(origin, "-") || strings.ContainsAny(origin, " \t\n") {
		return "", fmt.Errorf("недопустимый адрес репозитория %q", origin)
	}
	// --includes: переписывание адресов можно спрятать в файл, подключённый
	// через include.path (ревью Opus, финальная проверка, вариант G).
	// Глобальная и системная конфигурация отключены окружением гейта.
	if rewrites, _ := git(repo, "config", "--includes", "--get-regexp", `^url\.`); strings.TrimSpace(rewrites) != "" {
		return "", errors.New("конфигурация рабочей копии переписывает адреса репозиториев (url.*.insteadOf): база не надёжна")
	}
	remote := origin
	if remote == "" {
		remote = "origin"
	}
	if _, err := git(repo, "fetch", "--quiet", "--no-tags", remote, "+refs/heads/"+branch+":"+gateBaseRef); err == nil {
		return gateBaseRef, nil
	}
	refspecs, err := git(repo, "config", "--local", "--get-all", "remote.origin.fetch")
	if err != nil {
		return "", fmt.Errorf("refspec origin не прочитан: %w", err)
	}
	if strings.TrimSpace(refspecs) != standardFetch {
		return "", fmt.Errorf("нестандартный remote.origin.fetch (%s): база %s не надёжна", strings.TrimSpace(refspecs), base)
	}
	if origin != "" {
		url, _ := git(repo, "config", "--local", "--get", "remote.origin.url")
		if strings.TrimSpace(url) != origin {
			return "", fmt.Errorf("адрес remote origin рабочей копии (%s) не совпадает с адресом проекта: база %s не надёжна",
				strings.TrimSpace(url), base)
		}
	}
	return base, nil
}

// gitEnv — git без подмен объектов и без чужой конфигурации: replace-ссылки
// и grafts из .git рабочей копии не меняют историю, которую видит гейт,
// глобальная и системная конфигурация не читаются. Без запроса пароля:
// fetch без учётных данных падает, а не ждёт ввода.
var gitEnv = []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE=/dev/null", "GIT_TERMINAL_PROMPT=0",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// gitArgs — параметры каждой команды git гейта: хуки и fsmonitor рабочей
// копии не запускаются, зависший fetch обрывается по скорости.
var gitArgs = []string{"-c", "core.quotePath=false", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
	"-c", "http.lowSpeedLimit=1000", "-c", "http.lowSpeedTime=30"}

// gitTimeout — потолок одной команды git гейта (fetch базы с хостинга).
const gitTimeout = 2 * time.Minute

func git(repo string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, gitArgs...), args...)...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), gitEnv...)
	// Дочерние процессы git (git-remote-https) держат унаследованные
	// дескрипторы: без WaitDelay Output ждал бы их и после таймаута.
	cmd.WaitDelay = 5 * time.Second
	b, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(b), nil
}
