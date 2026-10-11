package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Политика проекта из репозитория (change add-policy-git-provider):
// синхронизация превращает файл доверенной ветки в версию политики.

func seedGitPolicy(t *testing.T, st *store.Store, sc *fakeSCM, content string) (domain.Project, *Engine) {
	t.Helper()
	ctx := context.Background()
	e := New(st, sc, nil, &capture{}, 90*time.Second)
	owner := mustOwner(t, st)
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProjectPolicySource(ctx, p.ID, policy.SourceGit); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		sc.mu.Lock()
		sc.files = map[string]string{"o/r@main:" + policy.PolicyFile: content}
		sc.mu.Unlock()
	}
	p, err = st.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, e
}

// Файл политики становится версией; неизменный файл новых версий не даёт.
func TestGitPolicySync(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merge: true\nattempt_limit: 5\n")

	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	eff, err := st.EffectivePolicy(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Presets.AutoMerge || eff.Presets.AttemptLimit != 5 {
		t.Fatalf("политика из репозитория: %+v", eff.Presets)
	}
	if eff.Project == nil || !strings.HasPrefix(eff.Project.CreatedBy, "git") {
		t.Fatalf("автор версии: %+v", eff.Project)
	}

	// Тот же файл — новой версии нет.
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListPolicyVersions(ctx, store.PolicyScopeProject, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("неизменный файл не должен плодить версии: %d", len(versions))
	}

	// Файл изменился — новая версия.
	sc.mu.Lock()
	sc.files["o/r@main:"+policy.PolicyFile] = "auto_merge: false\nattempt_limit: 2\n"
	sc.fileID = "sha-2"
	sc.mu.Unlock()
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	eff, _ = st.EffectivePolicy(ctx, p.ID)
	if eff.Presets.AutoMerge || eff.Presets.AttemptLimit != 2 {
		t.Fatalf("новая версия политики: %+v", eff.Presets)
	}
}

// Другой идентификатор файла с тем же содержимым версии не создаёт:
// перенос ветки или второй инстанс rivetd не должны плодить историю.
func TestGitPolicySameContentNewFileID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merge: true\n")
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	sc.fileID = "sha-2" // содержимое то же, идентификатор другой
	sc.mu.Unlock()
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListPolicyVersions(ctx, store.PolicyScopeProject, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("то же содержимое не должно давать версию: %d", len(versions))
	}
	p, _ = st.GetProject(ctx, p.ID)
	if p.PolicyFileID != "sha-2" {
		t.Fatalf("идентификатор файла должен запомниться: %q", p.PolicyFileID)
	}
}

// Опечатка в имени ключа — видимая ошибка, а не проигнорированное поле.
func TestGitPolicyUnknownKey(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merges: true\n")
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Type: "policy.sync_failed", Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("опечатка должна давать событие: %v %d", err, len(evs))
	}
}

// Битый файл не меняет действующую политику, но виден человеку.
func TestGitPolicyBrokenFile(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merge: true\n")
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Невалидная политика: лимит попыток ниже единицы.
	sc.mu.Lock()
	sc.files["o/r@main:"+policy.PolicyFile] = "attempt_limit: 0\n"
	sc.fileID = "sha-broken"
	sc.mu.Unlock()
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	eff, _ := st.EffectivePolicy(ctx, p.ID)
	if !eff.Presets.AutoMerge {
		t.Fatalf("должна действовать последняя валидная версия: %+v", eff.Presets)
	}
	evs, err := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Type: "policy.sync_failed", Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("событие о битом файле: %v %d", err, len(evs))
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM attention WHERE reason=$1 AND status <> 'resolved'`,
		string(domain.AttPolicySource)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("эскалация о битой политике: %d", n)
	}

	// Файл починили — эскалация снимается.
	sc.mu.Lock()
	sc.files["o/r@main:"+policy.PolicyFile] = "attempt_limit: 4\n"
	sc.fileID = "sha-fixed"
	sc.mu.Unlock()
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM attention WHERE reason=$1 AND status <> 'resolved'`,
		string(domain.AttPolicySource)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("после починки эскалация должна закрыться: %d", n)
	}
}

// Файла политики нет в доверенной ветке — это поломка источника, а не
// «политика по умолчанию».
func TestGitPolicyMissingFile(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{}
	p, e := seedGitPolicy(t, st, sc, "")
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Type: "policy.sync_failed", Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("событие о пропавшем файле: %v %d", err, len(evs))
	}
}

// Политика читается только из доверенной ветки: файл в ветке задачи на
// решения не влияет.
func TestGitPolicyTrustedRefOnly(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merge: false\n")
	// В ветке задачи лежит «ослабленная» политика.
	sc.mu.Lock()
	sc.files["o/r@agent/task-1:"+policy.PolicyFile] = "auto_merge: true\n"
	sc.mu.Unlock()
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	eff, _ := st.EffectivePolicy(ctx, p.ID)
	if eff.Presets.AutoMerge {
		t.Fatal("политика из ветки задачи не должна применяться")
	}
}

// Сломанный файл не даёт событие каждую минуту: одно на причину, новая
// причина — новое событие, починка сбрасывает память (ретро-ревью).
func TestGitPolicyBrokenFileNoEventSpam(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-broken"}
	p, e := seedGitPolicy(t, st, sc, "attempt_limit: 0\n")
	for i := 0; i < 3; i++ {
		if err := e.syncProjectPolicy(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		evs, err := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Type: "policy.sync_failed", Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	if n := count(); n != 1 {
		t.Fatalf("одна причина — одно событие, получили %d", n)
	}
	// Другая причина поломки — новое событие.
	sc.mu.Lock()
	sc.files["o/r@main:"+policy.PolicyFile] = "auto_merges: true\n"
	sc.mu.Unlock()
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("новая причина — новое событие, получили %d", n)
	}
	// Починили, потом сломали так же — событие снова пишется.
	sc.mu.Lock()
	sc.files["o/r@main:"+policy.PolicyFile] = "attempt_limit: 3\n"
	sc.fileID = "sha-ok"
	sc.mu.Unlock()
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	sc.files["o/r@main:"+policy.PolicyFile] = "auto_merges: true\n"
	sc.fileID = "sha-broken-2"
	sc.mu.Unlock()
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("после починки поломка снова даёт событие, получили %d", n)
	}
}

// Источник переключили обратно, пока файл читался: версия из файла не
// создаётся (ретро-ревью: гонка синхронизации и переключения).
func TestGitPolicySourceSwitchedDuringSync(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merge: true\n")
	// p ещё помнит источник git, а в базе он уже store.
	if err := st.SetProjectPolicySource(ctx, p.ID, policy.SourceStore); err != nil {
		t.Fatal(err)
	}
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	versions, err := st.ListPolicyVersions(ctx, store.PolicyScopeProject, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 0 {
		t.Fatalf("после переключения на store версия из файла не нужна: %d", len(versions))
	}
}

// Неизменный файл всё равно снимает застрявшую эскалацию (ретро-ревью).
func TestGitPolicyUnchangedFileResolvesEscalation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-1"}
	p, e := seedGitPolicy(t, st, sc, "auto_merge: true\n")
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Эскалация осталась от прошлого прохода (снятие тогда не удалось).
	if err := st.EscalateProjectOnce(ctx, p.ID, domain.AttPolicySource, "застряла"); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetProject(ctx, p.ID)
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM attention WHERE reason=$1 AND status <> 'resolved'`,
		string(domain.AttPolicySource)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("неизменный файл должен снять застрявшую эскалацию: %d", n)
	}
}

// Сбой записи события о поломке не теряет его: следующий проход пишет
// событие, потому что причина запоминается только после записи
// (третий круг ревью).
func TestGitPolicyBrokenEventSurvivesWriteFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sc := &fakeSCM{fileID: "sha-broken"}
	p, e := seedGitPolicy(t, st, sc, "attempt_limit: 0\n")
	// Отменённый контекст валит запись события.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.syncProjectPolicy(dead, p); err == nil {
		t.Fatal("запись с отменённым контекстом должна упасть")
	}
	if err := e.syncProjectPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	evs, err := st.Events(ctx, store.EventFilter{ProjectID: p.ID, Type: "policy.sync_failed", Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("событие должно записаться после сбоя: %v %d", err, len(evs))
	}
}
