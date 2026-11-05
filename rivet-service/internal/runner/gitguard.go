package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Защита git runner'а от рабочей копии (review add-native-agent, P1-1).
// Агент и проверки проекта исполняют код в клоне, который runner потом
// использует сам: commit, push, fetch следующей стадии. Через .git/config
// (core.fsmonitor, фильтры, credential.helper, url.insteadOf, include) и
// .git/hooks такой код получил бы исполнение в git runner'а с токеном
// хостинга. Поэтому:
//
//   - git runner'а работает с core.hooksPath=/dev/null, core.fsmonitor=false
//     и --no-verify, а окружение собирает из короткого списка, а не из
//     os.Environ();
//   - служебные файлы git снимаются до сессии агента (и до проверок) и
//     сверяются после. Расхождение — отказ стадии с причиной, файлы
//     возвращаются к снимку, а если вернуть нельзя, клон удаляется и
//     следующая стадия клонирует заново;
//   - перед каждой стадией, до fetch и до снимка, .git приводится к виду
//     свежего клона (prepareWorkspaceGit): так в работу не попадает то,
//     что сверка не поймала, потому что runner упал между сессией и
//     сверкой. Эта же подготовка защищает проверку архитектуры
//     (add-architecture-c4) от подменённой истории и адреса origin.

// errGitTampered — служебные файлы git изменились за время сессии.
var errGitTampered = errors.New("изменены служебные файлы git")

// gitTamperError — что изменилось и кто менял.
type gitTamperError struct {
	who     string
	changed []string
	// removed — вернуть файлы не удалось, клон удалён.
	removed bool
}

func (e *gitTamperError) Error() string {
	tail := "файлы возвращены к состоянию до сессии"
	if e.removed {
		tail = "клон удалён, следующая стадия клонирует репозиторий заново"
	}
	return fmt.Sprintf("%s изменил служебные файлы git: %s; %s", e.who, strings.Join(e.changed, ", "), tail)
}

func (e *gitTamperError) Unwrap() error { return errGitTampered }

// gitFile — содержимое служебного файла: режим и данные (у симлинка —
// цель ссылки, у каталога — пусто).
type gitFile struct {
	mode fs.FileMode
	data []byte
}

// gitSnapshot — снимок служебных файлов .git рабочей копии.
type gitSnapshot struct {
	ws    string
	files map[string]gitFile
	// replace — слепок ссылок refs/replace (объекты-подмены дерева): их нет
	// среди файлов config и hooks, а git runner'а их игнорирует
	// (GIT_NO_REPLACE_OBJECTS), но подмена всё равно считается
	// вмешательством и валит стадию.
	replace string
}

// gitSnapshotLimit — предел объёма снимка: служебные файлы маленькие,
// больший объём — признак подмены.
const gitSnapshotLimit = 16 << 20

// gitTopFiles — файлы .git, которые меняют поведение git: конфигурация,
// общий каталог, атрибуты, grafts и shallow, которые меняют видимую
// историю, и alternates — сторонние хранилища объектов.
var gitTopFiles = []string{"config", "config.worktree", "commondir", "info/attributes", "info/grafts", "shallow",
	gitAlternates, gitHTTPAlternates}

// gitAlternates и gitHTTPAlternates — указатели на сторонние хранилища
// объектов (локальное и для dumb HTTP). git clone runner'а их не пишет.
const (
	gitAlternates     = "objects/info/alternates"
	gitHTTPAlternates = "objects/info/http-alternates"
)

// gitSkipDirs — большие каталоги данных подмодулей, где служебных файлов нет.
var gitSkipDirs = map[string]bool{"objects": true, "refs": true, "logs": true, "lfs": true}

func snapshotGit(ws string) (*gitSnapshot, error) {
	files, err := collectGit(ws)
	if err != nil {
		return nil, fmt.Errorf("снимок служебных файлов git: %w", err)
	}
	replace, err := replaceRefsDigest(ws)
	if err != nil {
		return nil, fmt.Errorf("снимок refs/replace: %w", err)
	}
	return &gitSnapshot{ws: ws, files: files, replace: replace}, nil
}

// replaceRefsDigest — детерминированный слепок ссылок refs/replace: имена
// и цели, из loose-ссылок и packed-refs. Пусто — подмен нет (обычный клон).
func replaceRefsDigest(ws string) (string, error) {
	lines := map[string]bool{}
	root := filepath.Join(ws, ".git", "refs", "replace")
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(filepath.Join(ws, ".git"), p)
		lines[filepath.ToSlash(rel)+" "+strings.TrimSpace(string(b))] = true
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if b, err := os.ReadFile(filepath.Join(ws, ".git", "packed-refs")); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.Contains(ln, " refs/replace/") {
				lines[strings.TrimSpace(ln)] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	out := make([]string, 0, len(lines))
	for l := range lines {
		out = append(out, l)
	}
	sort.Strings(out)
	return strings.Join(out, "\n"), nil
}

// cleanReplaceRefs удаляет все ссылки refs/replace: loose-каталог и строки
// refs/replace в packed-refs. Используется при подготовке рабочей копии и
// при откате вмешательства.
func cleanReplaceRefs(ws string) error {
	if err := os.RemoveAll(filepath.Join(ws, ".git", "refs", "replace")); err != nil {
		return err
	}
	packed := filepath.Join(ws, ".git", "packed-refs")
	b, err := os.ReadFile(packed)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept []string
	changed := false
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.Contains(ln, " refs/replace/") {
			changed = true
			continue
		}
		kept = append(kept, ln)
	}
	if !changed {
		return nil
	}
	return os.WriteFile(packed, []byte(strings.Join(kept, "\n")), 0o644)
}

// collectGit — служебные файлы: .git (тип), файлы gitTopFiles, всё в
// hooks/, а у подмодулей (.git/modules/**) — их config и hooks.
func collectGit(ws string) (map[string]gitFile, error) {
	files := map[string]gitFile{}
	total := 0
	add := func(rel string) error {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		f := gitFile{mode: st.Mode()}
		switch {
		case st.Mode().IsRegular():
			if total += int(st.Size()); total > gitSnapshotLimit {
				return fmt.Errorf("служебные файлы больше %d байт", gitSnapshotLimit)
			}
			if f.data, err = os.ReadFile(p); err != nil {
				return err
			}
		case st.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			f.data = []byte(target)
		}
		files[rel] = f
		return nil
	}
	if err := add(".git"); err != nil {
		return nil, err
	}
	if g, ok := files[".git"]; !ok || !g.mode.IsDir() {
		// .git — файл или ссылка: дальше не обходим, сверка увидит подмену.
		return files, nil
	}
	walk := func(root string, keep func(rel string, d fs.DirEntry) bool) error {
		base := filepath.Join(ws, filepath.FromSlash(root))
		if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(ws, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() && p != base && gitSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			if keep(rel, d) {
				return add(rel)
			}
			return nil
		})
	}
	for _, f := range gitTopFiles {
		if err := add(".git/" + f); err != nil {
			return nil, err
		}
	}
	if err := walk(".git/hooks", func(string, fs.DirEntry) bool { return true }); err != nil {
		return nil, err
	}
	err := walk(".git/modules", func(rel string, d fs.DirEntry) bool {
		if strings.Contains(rel, "/hooks/") || strings.HasSuffix(rel, "/hooks") {
			return true
		}
		switch d.Name() {
		case "config", "config.worktree", "commondir":
			return !d.IsDir()
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// diff — пути, которые отличаются от снимка (изменены, удалены, добавлены).
func (s *gitSnapshot) diff() ([]string, map[string]gitFile, error) {
	now, err := collectGit(s.ws)
	if err != nil {
		return nil, nil, err
	}
	var changed []string
	for p, f := range s.files {
		g, ok := now[p]
		if !ok || g.mode != f.mode || !bytes.Equal(g.data, f.data) {
			changed = append(changed, p)
		}
	}
	for p := range now {
		if _, ok := s.files[p]; !ok {
			changed = append(changed, p)
		}
	}
	replace, err := replaceRefsDigest(s.ws)
	if err != nil {
		return nil, nil, err
	}
	if replace != s.replace {
		changed = append(changed, "refs/replace/*")
	}
	sort.Strings(changed)
	return changed, now, nil
}

// verify сверяет служебные файлы со снимком. При расхождении файлы
// возвращаются к снимку (а если нельзя — клон удаляется) и возвращается
// gitTamperError. who — кто работал в клоне: «агент», «проверка».
func (s *gitSnapshot) verify(who string) error {
	changed, now, err := s.diff()
	if err == nil && len(changed) == 0 {
		return nil
	}
	if err != nil {
		changed = []string{err.Error()}
	}
	terr := &gitTamperError{who: who, changed: changed}
	if err == nil && s.restore(changed, now) == nil {
		if again, _, err := s.diff(); err == nil && len(again) == 0 {
			return terr
		}
	}
	// Клон в неизвестном состоянии: следующая стадия клонирует заново.
	_ = os.RemoveAll(s.ws)
	terr.removed = true
	return terr
}

// restore возвращает изменённые пути к снимку: лишние удаляются,
// изменённые и удалённые записываются заново (каталоги — раньше файлов).
func (s *gitSnapshot) restore(changed []string, now map[string]gitFile) error {
	if f, ok := s.files[".git"]; !ok || !f.mode.IsDir() {
		return errors.New(".git не каталог")
	}
	if g, ok := now[".git"]; !ok || !g.mode.IsDir() {
		return errors.New(".git подменён")
	}
	// Ссылки refs/replace убираем целиком: в обычном клоне их нет, а
	// воссоздать снятые невозможно (клон тогда удаляется выше по diff).
	if s.replace == "" {
		if err := cleanReplaceRefs(s.ws); err != nil {
			return err
		}
	}
	// Сначала удаляем лишнее и изменённое (с конца: файлы раньше каталогов).
	// Каталог, который остался каталогом (сменились только права), не
	// удаляется: у .git это стёрло бы весь репозиторий.
	sort.Sort(sort.Reverse(sort.StringSlice(changed)))
	for _, p := range changed {
		g, existed := now[p]
		f, inSnap := s.files[p]
		if !existed || (inSnap && f.mode.IsDir() && g.mode.IsDir()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.ws, filepath.FromSlash(p))); err != nil {
			return err
		}
	}
	sort.Strings(changed)
	for _, p := range changed {
		f, ok := s.files[p]
		if !ok {
			continue
		}
		dst := filepath.Join(s.ws, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		switch {
		case f.mode.IsDir():
			if err := os.MkdirAll(dst, f.mode.Perm()); err != nil {
				return err
			}
			if err := os.Chmod(dst, f.mode.Perm()); err != nil {
				return err
			}
		case f.mode&fs.ModeSymlink != 0:
			if err := os.Symlink(string(f.data), dst); err != nil {
				return err
			}
		default:
			if err := os.WriteFile(dst, f.data, f.mode.Perm()); err != nil {
				return err
			}
			if err := os.Chmod(dst, f.mode.Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

// gitEnvKeep — переменные окружения runner'а, которые нужны git: пути,
// локаль, ssh-агент, прокси и корневые сертификаты. Ключи и токены сюда
// не входят: токен хостинга приходит только через askpass назначения.
var gitEnvKeep = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR",
	"SSH_AUTH_SOCK", "GIT_SSH_COMMAND", "GIT_SSH",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO",
}

// gitSafeConfig — настройки поверх конфигурации рабочей копии (как -c):
// хуки и fsmonitor клона не исполняются, помощники учётных данных из
// конфигурации клона сброшены (пустое значение очищает их список), и
// токен назначения получает только askpass runner'а.
var gitSafeConfig = [][2]string{
	{"core.hooksPath", "/dev/null"},
	{"core.fsmonitor", "false"},
	{"credential.helper", ""},
}

// gitEnv — окружение git runner'а: разрешённые переменные, безопасные
// настройки и переменные askpass назначения (creds). Глобальный и
// системный конфиг игнорируются: драйверы filter/clean/smudge и
// diff.textconv из ~/.gitconfig или /etc/gitconfig исполнились бы в git
// runner'а при add/clone/checkout с токеном хостинга в окружении, а снимок
// их не ловит (они вне клона). Объекты-подмены refs/replace тоже не
// применяются (GIT_NO_REPLACE_OBJECTS). info/grafts удаляет подготовка
// рабочей копии, а появление файла за сессию ловит снимок: GIT_GRAFT_FILE
// здесь не ставится, git печатал бы на нём предупреждение в каждый вывод.
func gitEnv(creds []string) []string {
	var env []string
	for _, k := range gitEnvKeep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(gitSafeConfig)),
	)
	for i, kv := range gitSafeConfig {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return append(env, creds...)
}

// standardFetch — refspec origin, который ставит git clone.
const standardFetch = "+refs/heads/*:refs/remotes/origin/*"

// gitConfigReadTimeout — предел чтения .git/config при подготовке рабочей
// копии: у свежего клона это доли секунды.
const gitConfigReadTimeout = 10 * time.Second

// errUnsafeClone — .git рабочей копии нельзя привести к виду свежего
// клона: клон удаляется и клонируется заново.
var errUnsafeClone = errors.New("служебные файлы git в неизвестном состоянии")

// prepareWorkspaceGit приводит .git рабочей копии к виду свежего клона до
// fetch и до снимка служебных файлов. Одна подготовка на обе защиты:
// gitguard add-native-agent и чистку рабочей копии add-architecture-c4.
//
//   - .git/config собирается заново из ключей, которые пишут clone и
//     checkout (keepGitConfig), адрес origin — из назначения, refspec
//     стандартный. Всё остальное исчезает: url.*.insteadOf, include и
//     includeIf, pushurl, credential.*, filter.*, diff.*, merge.*,
//     core.fsmonitor, core.hooksPath, core.worktree, extensions.worktreeConfig.
//     Список разрешённых ключей, а не опасных: новый способ исполнить код
//     через конфигурацию его не обойдёт;
//   - config.worktree и info/grafts удаляются, refs/replace чистится;
//   - .git не каталог, есть commondir, shallow или alternates, .git/config
//     не обычный файл (ссылка, FIFO, устройство) или не прочитан за
//     gitConfigReadTimeout: errUnsafeClone, рабочая копия клонируется заново.
//     Alternates не удаляются, а ведут к новому клону: объекты, которые
//     клон брал из стороннего хранилища, пропали бы вместе с указателем,
//     и стадия упала бы на fetch или checkout испорченного клона.
//
// git здесь идёт без shell и без учётных данных: ключи и значения пришли
// из конфигурации, которую мог писать агент.
func prepareWorkspaceGit(ctx context.Context, dir, origin string) error {
	gitDir := filepath.Join(dir, ".git")
	if st, err := os.Lstat(gitDir); err != nil || !st.IsDir() {
		return fmt.Errorf("%w: .git не каталог", errUnsafeClone)
	}
	for _, f := range []string{"commondir", "shallow", gitAlternates, gitHTTPAlternates} {
		if _, err := os.Lstat(filepath.Join(gitDir, filepath.FromSlash(f))); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: .git/%s", errUnsafeClone, f)
		}
	}
	gitIn := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv(nil)
		return cmd.Output()
	}
	git := func(args ...string) ([]byte, error) { return gitIn(ctx, args...) }
	cfgPath := filepath.Join(gitDir, "config")
	// Конфигурация читается, только если это обычный файл: ссылка на
	// неисчерпаемый спецфайл (/dev/zero) или FIFO, оставленные упавшей
	// сессией, подвесили бы чтение. Новый клон надёжнее правки на месте.
	if st, err := os.Lstat(cfgPath); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%w: .git/config не обычный файл", errUnsafeClone)
	}
	// --file читает один файл без include: видны только ключи самого
	// .git/config. Время чтения ограничено и для обычного файла.
	readCtx, cancel := context.WithTimeout(ctx, gitConfigReadTimeout)
	raw, err := gitIn(readCtx, "config", "--file", cfgPath, "--null", "--list")
	cancel()
	if err != nil {
		return fmt.Errorf("%w: .git/config не читается: %v", errUnsafeClone, err)
	}
	tmp, err := os.CreateTemp(gitDir, "config.rivet-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()
	set := func(key, value string) error {
		if out, err := git("config", "--file", tmpPath, "--add", key, value); err != nil {
			return fmt.Errorf("%s: %v %s", key, err, out)
		}
		return nil
	}
	for _, entry := range strings.Split(string(raw), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		if key != "" && keepGitConfig(key, value) {
			if err := set(key, value); err != nil {
				return err
			}
		}
	}
	if err := set("remote.origin.url", origin); err != nil {
		return err
	}
	if err := set("remote.origin.fetch", standardFetch); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	// rename заменяет и ссылку на месте .git/config, не следуя за ней.
	if err := os.Rename(tmpPath, cfgPath); err != nil {
		return err
	}
	for _, f := range []string{"config.worktree", filepath.Join("info", "grafts")} {
		if err := os.Remove(filepath.Join(gitDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return cleanReplaceRefs(dir)
}

// keepGitConfig — ключ .git/config, который переживает подготовку: то,
// что пишут git clone и checkout с отслеживанием ветки. Имена секций и
// ключей git отдаёт в нижнем регистре.
func keepGitConfig(key, value string) bool {
	switch key {
	case "core.repositoryformatversion", "core.filemode", "core.logallrefupdates",
		"core.ignorecase", "core.precomposeunicode", "core.symlinks",
		"extensions.objectformat", "extensions.refstorage":
		return true
	case "core.bare":
		return value == "false"
	}
	if rest, ok := strings.CutPrefix(key, "branch."); ok {
		switch {
		case strings.HasSuffix(rest, ".remote"):
			return value == "origin"
		case strings.HasSuffix(rest, ".merge"):
			return strings.HasPrefix(value, "refs/heads/")
		}
	}
	return false
}
