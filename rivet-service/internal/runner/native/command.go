package native

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Команды вне агентского цикла (ревью интеграции add-native-agent, P2-5).
// Проверки проекта на стадии TESTING исполняют код из ветки задачи, а его
// написала модель. Поэтому они идут тем же исполнителем, что и инструмент
// run: окружение из белого списка, временный HOME, песочница без сети с
// закрытыми $HOME runner'а и файлом токена, кеши зависимостей только на
// чтение, без фоновых процессов после выхода.

// ErrNoSandbox — песочница команд недоступна, а запуск без неё запрещён
// (RequireSandbox): команда не запускалась.
var ErrNoSandbox = errors.New("песочница команд недоступна")

// CommandSpec — команда в песочнице агента.
type CommandSpec struct {
	// Dir — рабочий каталог команды (рабочая копия задачи).
	Dir     string
	Command string
	// Timeout — предел времени команды; 0 — без предела, команду
	// останавливает только ctx (как проверки проекта до этого change).
	Timeout time.Duration
	// Home — HOME и TMPDIR команды (временный каталог вызывающего, общий
	// для нескольких команд: кеш сборки переживает их); пусто — свой
	// временный каталог на время команды.
	Home string
	// Path — PATH команды; пусто — PATH runner'а.
	Path string
	// Sandbox — выполнять в песочнице, если она доступна: без сети (если
	// не задан Network), со скрытыми HidePaths и кешами только на чтение.
	Sandbox bool
	// RequireSandbox — без песочницы не запускать: ErrNoSandbox вместо
	// отката в режим без изоляции. Включает Sandbox.
	RequireSandbox bool
	// Network — в песочнице оставить сеть хоста (без net namespace),
	// остальная изоляция та же. Только проверки проекта у runner'а с
	// -checks-network.
	Network bool
	// ReadOnlyCaches, HidePaths, ExtraEnv — как в ExecConfig.
	ReadOnlyCaches []CacheDir
	HidePaths      []string
	ExtraEnv       []string
	// MaxOutput — потолок вывода в результате (начало и конец); 0 — 32 КиБ.
	MaxOutput int
	// KillGrace — пауза между SIGTERM и SIGKILL; 0 — 5 с.
	KillGrace time.Duration
	// OnOutput — вывод команды по мере поступления (транскрипт стадии).
	// Вызывается последовательно из одной горутины и не после возврата
	// RunCommand.
	OnOutput func([]byte)
}

// CommandResult — итог команды.
type CommandResult struct {
	// Output — вывод с потолком MaxOutput.
	Output   string
	ExitCode int
	TimedOut bool
	// Leftovers — сколько фоновых процессов команды завершено после её выхода.
	Leftovers int
	// Isolated — команда шла в песочнице без сети.
	Isolated bool
	// Sandboxed — команда шла в песочнице (с сетью или без): $HOME runner'а
	// и файлы секретов ей не видны.
	Sandboxed bool
}

// RunCommand исполняет команду в песочнице агента. Ошибка — команда не
// запускалась или не завершилась сама: нет песочницы при RequireSandbox,
// сбой подготовки песочницы, отмена ctx. Ненулевой код выхода ошибкой не
// считается.
func RunCommand(ctx context.Context, spec CommandSpec) (CommandResult, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return CommandResult{}, errors.New("пустая команда")
	}
	// Помощник песочницы работает в рабочей копии и сохраняет её поверх
	// скрытого $HOME по пути: путь нужен абсолютный и без симлинков.
	dir, err := filepath.Abs(spec.Dir)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return CommandResult{}, fmt.Errorf("рабочий каталог команды: %w", err)
	}
	home := spec.Home
	if home == "" {
		h, err := os.MkdirTemp("", "rivet-command-home-*")
		if err != nil {
			return CommandResult{}, fmt.Errorf("временный HOME: %w", err)
		}
		defer RemoveTree(h)
		home = h
	}
	maxOut := spec.MaxOutput
	if maxOut <= 0 {
		maxOut = 32 << 10
	}
	grace := spec.KillGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	res := runCommand(ctx, commandSpec{
		dir: dir, command: spec.Command, timeout: spec.Timeout,
		home: home, path: spec.Path, isolate: spec.Sandbox || spec.RequireSandbox, network: spec.Network,
		maxOut: maxOut, killGrace: grace,
		readOnly: spec.ReadOnlyCaches, hidePaths: spec.HidePaths, extraEnv: spec.ExtraEnv,
		requireSandbox: spec.RequireSandbox, onOutput: spec.OnOutput,
	})
	return CommandResult{Output: res.output, ExitCode: res.exitCode, TimedOut: res.timedOut,
		Leftovers: res.leftovers, Isolated: res.isolated, Sandboxed: res.sandboxed}, res.err
}

// RemoveTree удаляет временный каталог команд целиком. Каталоги только
// для чтения (кеш модулей Go, скачанный во временный HOME) сначала
// открываются на запись, иначе os.RemoveAll оставил бы их в /tmp.
func RemoveTree(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
