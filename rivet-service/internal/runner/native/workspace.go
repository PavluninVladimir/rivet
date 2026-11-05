// Package native — агентский цикл Rivet (change add-native-agent, спека
// backend/native-agent): runner сам ведёт диалог с моделью провайдера и
// исполняет её вызовы инструментов в рабочей копии задачи. Внешние
// агентские программы не нужны; commit, push и проверки проекта остаются
// за стадией runner'а, модель их не выполняет.
package native

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Границы рабочей копии (design, решение 4). Ввод-вывод идёт через os.Root:
// ядро не даёт выйти за корень ни через «..», ни через симлинк, в том числе
// висячий или подменённый между проверкой и записью. Поверх этого:
//   - каталог .git (в любом регистре и на любом уровне) только для чтения,
//     включая пути, которые ведут в него через симлинки;
//   - запись через символическую ссылку запрещена целиком.
// Проверка делается перед каждым обращением: модель может создать симлинк
// командой run между вызовами.

// ErrOutside — путь вне рабочей копии.
var ErrOutside = errors.New("путь вне рабочей копии")

// ErrGitWrite — запись внутрь .git.
var ErrGitWrite = errors.New("каталог .git только для чтения: git делает runner")

// ErrSymlinkWrite — запись через символическую ссылку.
var ErrSymlinkWrite = errors.New("запись через символическую ссылку запрещена")

// workspace — корень рабочей копии задачи.
type workspace struct {
	root string   // абсолютный путь корня как передан
	real string   // корень с раскрытыми симлинками
	r    *os.Root // ввод-вывод только через него
	git  os.FileInfo
}

func newWorkspace(dir string) (*workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("рабочая копия: %w", err)
	}
	r, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("рабочая копия: %w", err)
	}
	w := &workspace{root: abs, real: real, r: r}
	if fi, err := os.Lstat(filepath.Join(real, ".git")); err == nil {
		w.git = fi
	}
	return w, nil
}

func (w *workspace) close() { _ = w.r.Close() }

// within — лежит ли путь хоста (с раскрытыми симлинками) внутри корня.
func (w *workspace) within(real string) bool {
	rel, err := filepath.Rel(w.real, real)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolve — путь от корня (через «/», «.» для корня) для обращения через
// os.Root. write — путь для записи: запрещены .git и симлинк в конце пути.
func (w *workspace) resolve(p string, write bool) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		p = "."
	}
	var joined string
	if filepath.IsAbs(p) {
		joined = filepath.Clean(p)
	} else {
		joined = filepath.Join(w.root, p)
	}
	// Абсолютный путь мог прийти в форме реального корня (/private/tmp на
	// macOS): сверяем и с исходным, и с раскрытым корнем.
	rel, err := filepath.Rel(w.root, joined)
	if err != nil || escapes(rel) {
		r2, e2 := filepath.Rel(w.real, joined)
		if e2 != nil || escapes(r2) {
			return "", fmt.Errorf("%w: %s", ErrOutside, p)
		}
		rel = r2
	}
	rel = filepath.ToSlash(rel)
	// Раскрытие симлинков нужно для проверок: сам ввод-вывод держит os.Root.
	realPath, err := evalExisting(filepath.Join(w.real, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if !w.within(realPath) {
		return "", fmt.Errorf("%w: %s", ErrOutside, p)
	}
	if !write {
		return rel, nil
	}
	realRel, _ := filepath.Rel(w.real, realPath)
	if inGit(rel) || inGit(filepath.ToSlash(realRel)) || w.underGit(realPath) {
		return "", fmt.Errorf("%w: %s", ErrGitWrite, p)
	}
	if rel != "." {
		if fi, err := w.r.Lstat(rel); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s", ErrSymlinkWrite, p)
		}
	}
	return rel, nil
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// underGit — какой-либо существующий предок пути — тот же каталог, что
// .git корня (ловит регистр, Unicode-формы имени и симлинки в .git).
func (w *workspace) underGit(realPath string) bool {
	if w.git == nil {
		return false
	}
	for cur := realPath; w.within(cur); cur = filepath.Dir(cur) {
		if fi, err := os.Stat(cur); err == nil && os.SameFile(fi, w.git) {
			return true
		}
		if cur == w.real {
			break
		}
	}
	return false
}

// evalExisting раскрывает симлинки пути, а для несуществующего — его
// ближайшего существующего предка с хвостом как есть.
func evalExisting(p string) (string, error) {
	tail := ""
	cur := p
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if tail == "" {
				return real, nil
			}
			return filepath.Join(real, tail), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		if tail == "" {
			tail = filepath.Base(cur)
		} else {
			tail = filepath.Join(filepath.Base(cur), tail)
		}
		cur = parent
	}
}

// inGit — путь от корня указывает в каталог .git на любом уровне и в любом
// регистре (у подмодулей свой .git; macOS и Windows не различают регистр).
func inGit(rel string) bool {
	for _, part := range strings.Split(path.Clean(filepath.ToSlash(rel)), "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}
