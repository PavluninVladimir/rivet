package architecture

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var applyDate = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// snapshot — содержимое всех файлов каталога.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compareDirs(t *testing.T, want, got string) {
	t.Helper()
	w, g := snapshot(t, want), snapshot(t, got)
	for p, content := range w {
		if g[p] != content {
			t.Errorf("%s отличается от golden:\n--- ожидали\n%s\n--- получили\n%s", p, content, g[p])
		}
	}
	for p := range g {
		if _, ok := w[p]; !ok {
			t.Errorf("лишний файл %s", p)
		}
	}
}

// Сценарий «Изменение применено» и golden-дифф файлов.
func TestApplyRefunds(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, shopDir, dir)
	ch, ds, err := LoadChange(os.DirFS(dir), "refunds")
	if err != nil || ds.HasErrors() {
		t.Fatalf("изменение не загрузилось: %v\n%s", err, dump(ds))
	}

	res, err := Apply(dir, "refunds", applyDate)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Archive != "changes/archive/2026-10-13-refunds" || len(res.Defects) != 0 {
		t.Fatalf("результат: %+v", res)
	}

	golden := "testdata/golden/apply-refunds"
	if *update {
		if err := os.RemoveAll(golden); err != nil {
			t.Fatal(err)
		}
		copyDir(t, dir, golden)
	}
	compareDirs(t, golden, dir)

	m, ds, err := Load(os.DirFS(dir))
	if err != nil || len(ds) != 0 {
		t.Fatalf("описание после применения: %v\n%s", err, dump(ds))
	}
	if !reflect.DeepEqual(m.Elements(), ch.Target.Elements()) {
		t.Errorf("модель после применения не совпадает с целевой моделью изменения")
	}
	if ds, err := CheckViews(os.DirFS(dir)); err != nil || len(ds) != 0 {
		t.Errorf("представления после применения: %v\n%s", err, dump(ds))
	}
	if _, err := os.Stat(filepath.Join(dir, "changes/refunds")); !os.IsNotExist(err) {
		t.Error("папка изменения должна уехать в архив")
	}

	// Правка узлов не трогает нетронутые записи и комментарии.
	before, _ := os.ReadFile(filepath.Join(shopDir, "model/business.yaml"))
	after, _ := os.ReadFile(filepath.Join(dir, "model/business.yaml"))
	if !strings.HasPrefix(string(after), strings.Split(string(before), "  - id: biz.manager")[0]) ||
		!strings.Contains(string(after), "# Процессы описаны в processes/<id>.bpmn.") {
		t.Errorf("business.yaml переписан шире, чем нужно:\n%s", after)
	}
}

// Сценарий «Текущая модель ушла вперёд»: при конфликте не пишется ни
// один файл.
func TestApplyConflict(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, shopDir, dir)
	app := filepath.Join(dir, "model/application.yaml")
	raw, _ := os.ReadFile(app)
	raw = append(raw, "  - {id: app.refunds, type: component, name: Возвраты}\n"...)
	if err := os.WriteFile(app, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	res, err := Apply(dir, "refunds", applyDate)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("изменение с конфликтом применено")
	}
	find(t, res.Defects, "delta.added-exists", "changes/refunds/delta/application.yaml", "app.refunds")
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Error("при конфликте файлы изменились")
	}
}

// Удаление процесса убирает его BPMN, добавление в пустой слой пишет
// массив блоком.
func TestApplyRemovesProcessAndFillsEmptyLayer(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "Тест"); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"model/business.yaml":                 "elements:\n  - {id: biz.old, type: process, name: Старый процесс}\n",
		"processes/biz.old.bpmn":              bpmnDoc(`<bpmn:startEvent id="s"/><bpmn:endEvent id="e"/><bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>`),
		"changes/cleanup/change.yaml":         "id: cleanup\ntitle: Уборка\n",
		"changes/cleanup/vision.md":           "Убрать старый процесс.\n",
		"changes/cleanup/delta/business.yaml": "removed:\n  - {id: biz.old, reason: Не используется}\n",
		"changes/cleanup/delta/data.yaml":     "added:\n  - {id: data.log, type: data-object, name: Журнал}\n",
		"changes/cleanup/gap.yaml":            "gaps:\n  - {id: gap.cleanup, title: Уборка, kind: remove, description: x, elements: [biz.old, data.log]}\n",
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteViews(dir); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(dir, "cleanup", applyDate)
	if err != nil || !res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(dir, "processes/biz.old.bpmn")); !os.IsNotExist(err) {
		t.Error("BPMN удалённого процесса остался")
	}
	if _, err := os.Stat(filepath.Join(dir, "views/processes/biz.old.md")); !os.IsNotExist(err) {
		t.Error("представление удалённого процесса осталось")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "model/data.yaml"))
	if string(data) != "elements:\n  - {id: data.log, type: data-object, name: Журнал}\n" {
		t.Errorf("data.yaml:\n%s", data)
	}
	business, _ := os.ReadFile(filepath.Join(dir, "model/business.yaml"))
	if string(business) != "elements: []\n" {
		t.Errorf("business.yaml:\n%s", business)
	}
}

func TestApplyArchiveExists(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, shopDir, dir)
	if err := os.MkdirAll(filepath.Join(dir, "changes/archive/2026-10-13-refunds"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Apply(dir, "refunds", applyDate)
	if err != nil || res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	find(t, res.Defects, "apply.archive-exists", "changes/archive/2026-10-13-refunds", "")
}

func TestInitDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(dir, "model/business.yaml")
	if err := os.MkdirAll(filepath.Dir(model), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("elements: [{id: biz.a, type: actor, name: A}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, "Тест"); err == nil || !strings.Contains(err.Error(), "model/business.yaml") {
		t.Fatalf("init поверх чужих файлов: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); !os.IsNotExist(err) {
		t.Error("при отказе init ничего не пишет")
	}
}

// Симлинк наружу не читается: описание из клона не должно утянуть чужой
// файл с машины runner'а.
func TestOpenDirRejectsEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "Тест"); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(secret, []byte("elements: [{id: app.secret, type: component, name: TOKEN}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, "model/application.yaml")
	if err := os.Remove(app); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, app); err != nil {
		t.Skip("симлинки недоступны:", err)
	}
	fsys, closer, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if _, err := Validate(fsys); err == nil {
		t.Fatal("файл за пределами каталога описания прочитан")
	}
	if _, err := WriteViews(dir); err == nil {
		t.Fatal("представления построены по файлу за пределами каталога")
	}
	if _, _, err := OpenDir(filepath.Join(dir, "nope")); err != ErrAbsent {
		t.Errorf("несуществующий каталог: %v", err)
	}
}

// Сбой записи посреди применения откатывает уже записанные файлы.
func TestApplyRollback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("под root права каталога не мешают записи")
	}
	dir := t.TempDir()
	copyDir(t, shopDir, dir)
	procs := filepath.Join(dir, "processes")
	before := snapshot(t, dir)
	if err := os.Chmod(procs, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(procs, 0o755) })
	res, err := Apply(dir, "refunds", applyDate)
	if err == nil || res.Applied {
		t.Fatalf("применение должно упасть на записи процесса: %v %+v", err, res)
	}
	if err := os.Chmod(procs, 0o755); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		for p := range after {
			if before[p] != after[p] {
				t.Errorf("файл %s не откатился", p)
			}
		}
	}
}

func TestApplyArchiveNotDir(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, shopDir, dir)
	if err := os.WriteFile(filepath.Join(dir, "changes/archive"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	res, err := Apply(dir, "refunds", applyDate)
	if err != nil || res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	find(t, res.Defects, "apply.archive-dir", "changes/archive", "")
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Error("описание изменилось")
	}
}

// Симлинк в views/ наружу не перезаписывается.
func TestWriteViewsDoesNotFollowEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "Тест"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "important.md")
	if err := os.WriteFile(outside, []byte("важное"), 0o644); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(dir, "views/application.md")
	if err := os.Remove(view); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, view); err != nil {
		t.Skip("симлинки недоступны:", err)
	}
	if _, err := WriteViews(dir); err == nil {
		t.Error("запись через симлинк наружу должна отказать")
	}
	if raw, _ := os.ReadFile(outside); string(raw) != "важное" {
		t.Errorf("внешний файл перезаписан: %s", raw)
	}
}

func writeMapFS(t *testing.T, dir string, fsys fstest.MapFS) {
	t.Helper()
	for p, f := range fsys {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestApplyItemsThroughArray(t *testing.T) {
	dir := t.TempDir()
	writeMapFS(t, dir, riskFS(map[string]string{
		"changes/add/change.yaml":     "id: add\ntitle: Риск\n",
		"changes/add/vision.md":       "Добавить риск.\n",
		"changes/add/delta/risk.yaml": "added:\n  - {id: risk.b, title: B}\nremoved:\n  - {id: risk.a, reason: Устарел}\n",
		"changes/add/gap.yaml":        "gaps:\n  - {id: gap.risk, title: Риск, kind: change, description: x, elements: [risk.a, risk.b]}\n",
	}))
	res, err := Apply(dir, "add", applyDate)
	if err != nil || !res.Applied {
		t.Fatalf("%v %+v", err, res)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "model/risk.yaml"))
	want := "owner: app.old\ngroups:\n  - name: Первая\n    risks: []\n  - name: Вторая\n    risks:\n      - {id: risk.b, title: B}\n"
	if string(raw) != want {
		t.Errorf("model/risk.yaml:\n%s", raw)
	}
}

// Симлинк внутри описания тоже не даёт писать: иначе генератор затёр бы
// модель Markdown'ом.
func TestWriteViewsRejectsInternalSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, "Тест"); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model/application.yaml")
	before, _ := os.ReadFile(model)
	view := filepath.Join(dir, "views/application.md")
	if err := os.Remove(view); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../model/application.yaml", view); err != nil {
		t.Skip("симлинки недоступны:", err)
	}
	if _, err := WriteViews(dir); err == nil || !strings.Contains(err.Error(), "симлинк") {
		t.Errorf("запись через внутренний симлинк: %v", err)
	}
	if after, _ := os.ReadFile(model); string(after) != string(before) {
		t.Errorf("модель затёрта:\n%s", after)
	}
}

// Симлинк на промежуточном каталоге тоже не даёт писать.
func TestWriteFileRejectsSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "model"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "views"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../model", filepath.Join(dir, "views/processes")); err != nil {
		t.Skip("симлинки недоступны:", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeFile(root, "views/processes/x.md", []byte("x")); err == nil || !strings.Contains(err.Error(), "views/processes") {
		t.Errorf("запись через симлинк каталога: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "model/x.md")); !os.IsNotExist(err) {
		t.Error("файл записан в model/ через симлинк")
	}
}
