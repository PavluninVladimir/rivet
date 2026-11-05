package scm

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
)

// readStub — хостинг, который умеет только ReadFile: остальные методы
// Adapter в RepoFS не нужны.
type readStub struct {
	Adapter
	files map[string]string // "ref:путь" -> содержимое
	calls []string
	fail  error // сбой хостинга на любом запросе
}

func (s *readStub) ReadFile(_ context.Context, repo, ref, path string) (FileContent, error) {
	s.calls = append(s.calls, ref+":"+path)
	if s.fail != nil {
		return FileContent{}, s.fail
	}
	c, ok := s.files[ref+":"+path]
	if !ok {
		return FileContent{}, ErrFileNotFound
	}
	return FileContent{Content: c, FileID: "id"}, nil
}

func TestRepoFSReadsByRefFromRoot(t *testing.T) {
	st := &readStub{files: map[string]string{
		"abc123:docs/architecture/architecture.yaml":   "version: v1\n",
		"abc123:docs/architecture/model/business.yaml": "elements: []\n",
	}}
	fsys := NewRepoFS(context.Background(), st, "own/proj", "abc123", "/docs/architecture/", FSLimits{})

	raw, err := fs.ReadFile(fsys, "architecture.yaml")
	if err != nil || string(raw) != "version: v1\n" {
		t.Fatalf("ReadFile: %q %v", raw, err)
	}
	f, err := fsys.Open("model/business.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(f)
	fi, _ := f.Stat()
	if string(body) != "elements: []\n" || fi.Size() != int64(len(body)) || fi.Name() != "business.yaml" || fi.IsDir() {
		t.Fatalf("Open: %q %+v", body, fi)
	}
	if fi, err := fs.Stat(fsys, "architecture.yaml"); err != nil || fi.Size() != 12 {
		t.Fatalf("Stat: %+v %v", fi, err)
	}
	// Повторное чтение берётся из кэша: запрос к хостингу один на путь.
	if len(st.calls) != 2 {
		t.Fatalf("запросы к хостингу: %v", st.calls)
	}
	// Изменение возвращённого среза не портит кэш.
	raw[0] = 'X'
	if again, _ := fs.ReadFile(fsys, "architecture.yaml"); string(again) != "version: v1\n" {
		t.Fatalf("кэш испорчен: %q", again)
	}
}

func TestRepoFSMissingAndListing(t *testing.T) {
	st := &readStub{files: map[string]string{}}
	fsys := NewRepoFS(context.Background(), st, "own/proj", "abc", "docs/architecture", FSLimits{})

	_, err := fs.ReadFile(fsys, "processes/order.bpmn")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("отсутствующий файл должен давать fs.ErrNotExist: %v", err)
	}
	// Отсутствие кэшируется: второй вопрос не тратит запрос.
	_, _ = fs.Stat(fsys, "processes/order.bpmn")
	if len(st.calls) != 1 {
		t.Fatalf("запросы: %v", st.calls)
	}

	_, err = fs.ReadDir(fsys, "model")
	if !errors.Is(err, ErrNoListing) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("листинг: %v", err)
	}
	if _, err := fsys.Open("."); !errors.Is(err, ErrNoListing) {
		t.Fatalf("открытие корня: %v", err)
	}
	for _, bad := range []string{"../secret", "/abs", "a//b", ""} {
		if _, err := fs.ReadFile(fsys, bad); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("путь %q: %v", bad, err)
		}
	}
	if len(st.calls) != 1 {
		t.Fatalf("некорректные пути не должны доходить до хостинга: %v", st.calls)
	}
}

func TestRepoFSFileLimit(t *testing.T) {
	st := &readStub{files: map[string]string{"r:a": "1", "r:b": "2", "r:c": "3"}}
	fsys := NewRepoFS(context.Background(), st, "own/proj", "r", "", FSLimits{MaxFiles: 2})

	if _, err := fs.ReadFile(fsys, "a"); err != nil {
		t.Fatal(err)
	}
	// Отсутствующий файл тоже расходует запрос.
	if _, err := fs.ReadFile(fsys, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	_, err := fs.ReadFile(fsys, "b")
	if !errors.Is(err, ErrFSLimit) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("третий файл сверх лимита: %v", err)
	}
	if !errors.Is(fsys.Err(), ErrFSLimit) {
		t.Fatalf("Err: %v", fsys.Err())
	}
	// Прочитанное раньше по-прежнему доступно, новое нет.
	if raw, err := fs.ReadFile(fsys, "a"); err != nil || string(raw) != "1" {
		t.Fatalf("кэш после лимита: %q %v", raw, err)
	}
	if _, err := fs.ReadFile(fsys, "c"); !errors.Is(err, ErrFSLimit) {
		t.Fatalf("после превышения: %v", err)
	}
	if files, _ := fsys.Usage(); files != 2 || len(st.calls) != 2 {
		t.Fatalf("запросы: %d %v", files, st.calls)
	}
}

func TestRepoFSByteLimit(t *testing.T) {
	st := &readStub{files: map[string]string{"r:a": strings.Repeat("x", 6), "r:b": strings.Repeat("y", 6)}}
	fsys := NewRepoFS(context.Background(), st, "own/proj", "r", "", FSLimits{MaxBytes: 10})
	if _, err := fs.ReadFile(fsys, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(fsys, "b"); !errors.Is(err, ErrFSLimit) {
		t.Fatalf("объём сверх лимита: %v", err)
	}
	if _, bytes := fsys.Usage(); bytes != 12 {
		t.Fatalf("объём: %d", bytes)
	}
}

// Сбой хостинга липкий на всё время жизни RepoFS и виден через Err():
// иначе загрузчик, превративший ошибку в дефект, дал бы снимку ложный
// invalid вместо error, а удачный повтор скрыл бы это. Повтор того же пути
// отдаёт запомненный сбой без запроса. Новая попытка — новый RepoFS.
func TestRepoFSHostFailure(t *testing.T) {
	boom := errors.New("502 bad gateway")
	st := &readStub{files: map[string]string{"r:a": "ok", "r:b": "ok"}, fail: boom}
	fsys := NewRepoFS(context.Background(), st, "own/proj", "r", "", FSLimits{})
	_, err := fs.ReadFile(fsys, "a")
	if !errors.Is(err, boom) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("сбой хостинга: %v", err)
	}
	if err := fsys.Err(); !errors.Is(err, boom) || errors.Is(err, ErrFSLimit) {
		t.Fatalf("Err после сбоя: %v", err)
	}

	// Хостинг ожил, но эта попытка снимка уже испорчена.
	st.fail = nil
	if _, err := fs.ReadFile(fsys, "a"); !errors.Is(err, boom) {
		t.Fatalf("повтор того же пути: %v", err)
	}
	if raw, err := fs.ReadFile(fsys, "b"); err != nil || string(raw) != "ok" {
		t.Fatalf("другой путь читается: %q %v", raw, err)
	}
	if err := fsys.Err(); !errors.Is(err, boom) {
		t.Fatalf("сбой остаётся до конца жизни RepoFS: %v", err)
	}
	if len(st.calls) != 2 {
		t.Fatalf("путь со сбоем не перезапрашивается: %v", st.calls)
	}

	// Новая попытка снимка — новый RepoFS, без старого сбоя.
	again := NewRepoFS(context.Background(), st, "own/proj", "r", "", FSLimits{})
	if raw, err := fs.ReadFile(again, "a"); err != nil || string(raw) != "ok" || again.Err() != nil {
		t.Fatalf("новая попытка: %q %v %v", raw, err, again.Err())
	}
}

// Первая проблема снимка остаётся первой: лимит после сбоя не подменяет
// причину.
func TestRepoFSFirstProblemWins(t *testing.T) {
	boom := errors.New("502 bad gateway")
	st := &readStub{files: map[string]string{"r:a": "1", "r:b": "2"}, fail: boom}
	fsys := NewRepoFS(context.Background(), st, "own/proj", "r", "", FSLimits{MaxFiles: 1})
	_, _ = fs.ReadFile(fsys, "a")
	st.fail = nil
	if _, err := fs.ReadFile(fsys, "b"); !errors.Is(err, ErrFSLimit) {
		t.Fatalf("лимит: %v", err)
	}
	if err := fsys.Err(); !errors.Is(err, boom) {
		t.Fatalf("первая проблема: %v", err)
	}
}
