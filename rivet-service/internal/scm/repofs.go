package scm

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path"
	"sync"
	"time"
)

// RepoFS — файловая система только для чтения поверх ReadFile хостинга на
// одной версии репозитория (снимок архитектуры проекта, change
// add-architecture-c4). Каждый файл читается отдельным запросом к API,
// поэтому листинга каталогов нет: читатель обязан знать пути заранее.
// Запросы ограничены по числу файлов и объёму, чтобы описание-переросток
// или ошибка в нём не выжгли лимит запросов хостинга.
//
// fs.FS не принимает context, поэтому контекст запросов задаётся при
// создании и действует на всё время жизни RepoFS.
//
// RepoFS — одна попытка снимка. Каждый путь запрашивается не больше одного
// раза: ответ, отсутствие файла и сбой хостинга запоминаются до конца
// жизни RepoFS. Иначе первый читатель мог бы превратить сбой в дефект
// описания, а удачный повтор того же пути скрыл бы, что дефект ложный.
// Новая попытка — новый RepoFS.
type RepoFS struct {
	ctx   context.Context
	a     Adapter
	repo  string
	ref   string
	root  string
	limit FSLimits

	mu       sync.Mutex
	cache    map[string]repoEntry // по полному пути: ответ, отсутствие или сбой
	files    int
	bytes    int64
	overflow error // превышение лимита: новые пути больше не запрашиваются
	first    error // первая проблема чтения (сбой хостинга или лимит)
	symlinks []string
}

// Symlinks — пути (от корня RepoFS), где лежали символические ссылки: они
// читались как отсутствующие файлы, вызывающий пишет о них дефект.
func (r *RepoFS) Symlinks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.symlinks...)
}

type repoEntry struct {
	data    []byte
	missing bool
	err     error // сбой хостинга на этом пути
}

// FSLimits — лимиты чтения RepoFS: число запрошенных файлов (включая
// отсутствующие, запрос к API всё равно сделан) и суммарный объём.
type FSLimits struct {
	MaxFiles int
	MaxBytes int64
}

// DefaultFSLimits — лимиты снимка архитектуры: 600 файлов и 8 МиБ
// (add-architecture-erd-sequence-adr подняла их с 200 файлов и 4 МиБ ради
// сценариев и журнала решений).
var DefaultFSLimits = FSLimits{MaxFiles: 600, MaxBytes: 8 << 20}

// ErrFSLimit — чтение превысило лимиты RepoFS.
var ErrFSLimit = errors.New("превышен лимит чтения файлов репозитория")

// ErrNoListing — RepoFS не умеет листать каталоги.
var ErrNoListing = errors.New("листинг каталогов недоступен: файлы читаются по известным путям")

// NewRepoFS — RepoFS репозитория repo на версии ref (SHA или ветка) с
// корнем root внутри репозитория (например "docs/architecture"; пусто —
// корень репозитория). Нулевые поля лимитов означают значения по умолчанию.
func NewRepoFS(ctx context.Context, a Adapter, repo, ref, root string, limit FSLimits) *RepoFS {
	if limit.MaxFiles <= 0 {
		limit.MaxFiles = DefaultFSLimits.MaxFiles
	}
	if limit.MaxBytes <= 0 {
		limit.MaxBytes = DefaultFSLimits.MaxBytes
	}
	root = path.Clean("/" + root)[1:]
	return &RepoFS{ctx: ctx, a: a, repo: repo, ref: ref, root: root, limit: limit, cache: map[string]repoEntry{}}
}

// Err — первая причина, по которой прочитанное нельзя считать описанием
// репозитория: сбой хостинга или превышение лимита. Однажды появившись,
// она остаётся до конца жизни RepoFS. Загрузчик описания мог превратить
// ошибку чтения в дефект, а снимку нужен статус error, а не invalid.
func (r *RepoFS) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first
}

// Usage — сколько файлов запрошено и сколько байт прочитано.
func (r *RepoFS) Usage() (files int, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files, r.bytes
}

// ReadFile — fs.ReadFileFS: содержимое файла name от корня RepoFS.
func (r *RepoFS) ReadFile(name string) ([]byte, error) {
	data, err := r.read("readfile", name)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(data), nil
}

// Open — fs.FS: открыть файл. Каталог открыть нельзя (нет листинга).
func (r *RepoFS) Open(name string) (fs.File, error) {
	if name == "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: ErrNoListing}
	}
	data, err := r.read("open", name)
	if err != nil {
		return nil, err
	}
	return &repoFile{Reader: bytes.NewReader(data), info: repoInfo{name: path.Base(name), size: int64(len(data))}}, nil
}

// Stat — fs.StatFS: сведения о файле. Каталог неотличим от отсутствующего
// файла (GitLab), поэтому Stat годится только для файлов.
func (r *RepoFS) Stat(name string) (fs.FileInfo, error) {
	data, err := r.read("stat", name)
	if err != nil {
		return nil, err
	}
	return repoInfo{name: path.Base(name), size: int64(len(data))}, nil
}

// ReadDir — fs.ReadDirFS: листинга нет. Ошибка не fs.ErrNotExist, чтобы
// читатель не принял каталог за пустой.
func (r *RepoFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: ErrNoListing}
}

// read — общий путь чтения: кэш, лимиты, запрос к хостингу. Каждый путь
// запрашивается не больше одного раза (см. RepoFS): повтор отдаёт
// запомненный ответ, отсутствие файла или сбой.
func (r *RepoFS) read(op, name string) ([]byte, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	full := name
	if r.root != "" {
		full = r.root + "/" + name
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.cache[full]; ok {
		switch {
		case e.err != nil:
			return nil, &fs.PathError{Op: op, Path: name, Err: e.err}
		case e.missing:
			return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
		return e.data, nil
	}
	if r.overflow == nil && r.files >= r.limit.MaxFiles {
		r.fail(ErrFSLimit)
	}
	if r.overflow != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: r.overflow}
	}
	r.files++
	fc, err := r.a.ReadFile(r.ctx, r.repo, r.ref, full)
	if errors.Is(err, ErrFileNotFound) {
		r.cache[full] = repoEntry{missing: true}
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	if errors.Is(err, ErrSymlink) {
		r.cache[full] = repoEntry{missing: true}
		r.symlinks = append(r.symlinks, name)
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	if err != nil {
		r.cache[full] = repoEntry{err: err}
		if r.first == nil {
			r.first = &fs.PathError{Op: op, Path: name, Err: err}
		}
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	r.bytes += int64(len(fc.Content))
	if r.bytes > r.limit.MaxBytes {
		r.fail(ErrFSLimit)
		return nil, &fs.PathError{Op: op, Path: name, Err: r.overflow}
	}
	data := []byte(fc.Content)
	r.cache[full] = repoEntry{data: data}
	return data, nil
}

// fail — превышение лимита: новые пути больше не запрашиваются, и это
// проблема снимка, если до неё других не было. Вызывается под замком.
func (r *RepoFS) fail(err error) {
	r.overflow = err
	if r.first == nil {
		r.first = err
	}
}

// repoFile — открытый файл RepoFS: содержимое уже в памяти.
type repoFile struct {
	*bytes.Reader
	info repoInfo
}

func (f *repoFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *repoFile) Close() error               { return nil }

// repoInfo — сведения о файле RepoFS: только имя и размер.
type repoInfo struct {
	name string
	size int64
}

func (i repoInfo) Name() string       { return i.name }
func (i repoInfo) Size() int64        { return i.size }
func (i repoInfo) Mode() fs.FileMode  { return 0o444 }
func (i repoInfo) ModTime() time.Time { return time.Time{} }
func (i repoInfo) IsDir() bool        { return false }
func (i repoInfo) Sys() any           { return nil }

var (
	_ fs.ReadFileFS = (*RepoFS)(nil)
	_ fs.ReadDirFS  = (*RepoFS)(nil)
	_ fs.StatFS     = (*RepoFS)(nil)
)
