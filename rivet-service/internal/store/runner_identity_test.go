package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Привязка токенов регистрации к runner'ам (add-runner-identity, спека
// runners «Токены регистрации runner'ов»).

func tokenByName(t *testing.T, s *Store, name string) domain.RunnerToken {
	t.Helper()
	list, err := s.ListRunnerTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.Name == name {
			return tok
		}
	}
	t.Fatalf("токена %q нет в списке", name)
	return domain.RunnerToken{}
}

func boundIDs(tok domain.RunnerToken) []string {
	var out []string
	for _, b := range tok.Runners {
		out = append(out, b.RunnerID+":"+string(b.Source))
	}
	slices.Sort(out)
	return out
}

func mustIdentityErr(t *testing.T, err error, reason string) {
	t.Helper()
	var ie *IdentityError
	if !errors.Is(err, ErrRunnerIdentity) || !errors.As(err, &ie) || !strings.Contains(ie.Reason, reason) {
		t.Fatalf("ожидался отказ по идентичности «%s», получено %v", reason, err)
	}
}

func TestRunnerTokenBindings(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)

	// Выпуск для двух runner'ов хоста: повтор в списке схлопывается, событие
	// выпуска называет runner'ов.
	host, _, err := s.CreateRunnerToken(ctx, "host", nil, []string{"w1", "w2", "w1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	if got := boundIDs(host); !slices.Equal(got, []string{"w1:issue", "w2:issue"}) {
		t.Fatalf("привязки при выпуске: %v", got)
	}
	evs, err := s.Events(ctx, EventFilter{Installation: true, Type: "runner_token.created"})
	if err != nil || len(evs) != 1 || fmt.Sprint(evs[0].Payload["runner_ids"]) != "[w1 w2]" || !strings.Contains(evs[0].Text, "для w1, w2") {
		t.Fatalf("событие выпуска: %v %+v", err, evs)
	}
	for _, id := range []string{"w1", "w2"} {
		if bound, err := s.AuthorizeRunner(ctx, host.ID, id, true); err != nil || bound {
			t.Fatalf("%s по токену хоста: %v %v", id, bound, err)
		}
	}
	_, err = s.AuthorizeRunner(ctx, host.ID, "w3", true)
	mustIdentityErr(t, err, "выпущен для других runner'ов")

	// Токен без привязки: канал до регистрации не открывается, чужой
	// привязанный runner не забирается, токен остаётся свободным.
	free, _, err := s.CreateRunnerToken(ctx, "free", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AuthorizeRunner(ctx, free.ID, "b7", false)
	mustIdentityErr(t, err, "ещё не привязан")
	_, err = s.AuthorizeRunner(ctx, free.ID, "w1", true)
	mustIdentityErr(t, err, "привязан к другому действующему токену «host»")
	if got := boundIDs(tokenByName(t, s, "free")); len(got) != 0 {
		t.Fatalf("токен привязан после отказа: %v", got)
	}

	// Первая регистрация привязывает токен с событием, второму runner'у отказ.
	if bound, err := s.AuthorizeRunner(ctx, free.ID, "b7", true); err != nil || !bound {
		t.Fatalf("первая регистрация b7: %v %v", bound, err)
	}
	_, err = s.AuthorizeRunner(ctx, free.ID, "b8", true)
	mustIdentityErr(t, err, "выпущен для других runner'ов")
	if bound, err := s.AuthorizeRunner(ctx, free.ID, "b7", false); err != nil || bound {
		t.Fatalf("канал b7 после привязки: %v %v", bound, err)
	}
	if got := boundIDs(tokenByName(t, s, "free")); !slices.Equal(got, []string{"b7:first_use"}) {
		t.Fatalf("привязка при первой регистрации: %v", got)
	}
	evs, err = s.Events(ctx, EventFilter{Installation: true, Type: "runner_token.bound"})
	if err != nil || len(evs) != 1 || evs[0].ActorID != "b7" || evs[0].Payload["token_name"] != "free" {
		t.Fatalf("событие привязки: %v %+v", err, evs)
	}

	// Токен без привязки, отозванный после проверки секрета, под
	// блокировкой не привязывается (ревью P3-1).
	gone, _, err := s.CreateRunnerToken(ctx, "gone", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeRunnerToken(ctx, gone.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	if bound, err := s.AuthorizeRunner(ctx, gone.ID, "g1", true); !errors.Is(err, ErrNotFound) || bound {
		t.Fatalf("отозванный свободный токен: %v %v", bound, err)
	}
	if got := tokenByName(t, s, "gone").Runners; len(got) != 0 {
		t.Fatalf("отозванный токен привязан: %v", got)
	}

	// Отозванный и просроченный токены runner'а не держат: идентификатор
	// свободен для привязки при первой регистрации.
	if err := s.RevokeRunnerToken(ctx, host.ID, admin.Login); err != nil {
		t.Fatal(err)
	}
	next, _, err := s.CreateRunnerToken(ctx, "next", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := s.AuthorizeRunner(ctx, next.ID, "w1", true); err != nil || !bound {
		t.Fatalf("w1 после отзыва прежнего токена: %v %v", bound, err)
	}
	old, _, err := s.CreateRunnerToken(ctx, "old", nil, []string{"y"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runner_tokens SET expires_at = now() - interval '1 minute' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	later, _, err := s.CreateRunnerToken(ctx, "later", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := s.AuthorizeRunner(ctx, later.ID, "y", true); err != nil || !bound {
		t.Fatalf("y после истечения прежнего токена: %v %v", bound, err)
	}

	// Ротация: новый токен на тот же runner при действующем старом.
	rot, _, err := s.CreateRunnerToken(ctx, "rot", nil, []string{"b7"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeRunner(ctx, rot.ID, "b7", true); err != nil {
		t.Fatalf("ротация токена b7: %v", err)
	}
	// Отозванный токен в списке сохраняет привязки для аудита.
	if got := boundIDs(tokenByName(t, s, "host")); !slices.Equal(got, []string{"w1:issue", "w2:issue"}) {
		t.Fatalf("привязки отозванного токена: %v", got)
	}
}

// Параллельные первые регистрации: токен привязывается ровно к одному
// runner'у, а один идентификатор ровно к одному свободному токену.
func TestFirstUseBindingRace(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	admin := mustOwner(t, s)

	free, _, err := s.CreateRunnerToken(ctx, "free", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	race := func(n int, call func(i int) (bool, error)) (won int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		start := make(chan struct{})
		for i := range n {
			wg.Go(func() {
				<-start
				bound, err := call(i)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil && bound:
					won++
				case errors.Is(err, ErrRunnerIdentity):
				default:
					t.Errorf("гонка %d: %v %v", i, bound, err)
				}
			})
		}
		close(start)
		wg.Wait()
		return won
	}
	if won := race(8, func(i int) (bool, error) {
		return s.AuthorizeRunner(ctx, free.ID, fmt.Sprintf("r%d", i), true)
	}); won != 1 {
		t.Fatalf("токен привязан %d раз", won)
	}
	if got := tokenByName(t, s, "free").Runners; len(got) != 1 {
		t.Fatalf("привязок у токена: %v", got)
	}

	var tokens []string
	for i := range 6 {
		tok, _, err := s.CreateRunnerToken(ctx, fmt.Sprintf("t%d", i), nil, nil, admin.ID, admin.Login)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, tok.ID)
	}
	if won := race(len(tokens), func(i int) (bool, error) {
		return s.AuthorizeRunner(ctx, tokens[i], "shared", true)
	}); won != 1 {
		t.Fatalf("идентификатор shared привязан к %d токенам", won)
	}
}

// Миграция 0037: общий токен привязывается к runner'ам, которые уже
// регистрировались с ним, неиспользованный остаётся свободным, runner без
// токена не привязывается.
func TestMigration0037RunnerBindings(t *testing.T) {
	ctx := context.Background()
	url := emptyDB(t)
	if err := migrateFiles(ctx, url, migrationsBefore(t, "0037")); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	admin := mustOwner(t, s)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// Токены до change'а: без привязок (вставка в обход нового кода).
	var shared, unused, fleet string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO runner_tokens (name, prefix, token_hash, created_by)
		VALUES ('local-host', 'rrt_aaaaaaaa', 'h1', $1) RETURNING id`, admin.ID).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO runner_tokens (name, prefix, token_hash, created_by)
		VALUES ('old-fleet', 'rrt_cccccccc', 'h3', $1) RETURNING id`, admin.ID).Scan(&fleet); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO runner_tokens (name, prefix, token_hash, created_by)
		VALUES ('unused', 'rrt_bbbbbbbb', 'h2', $1) RETURNING id`, admin.ID).Scan(&unused); err != nil {
		t.Fatal(err)
	}
	// local-worker сначала ходил по старому токену флота, потом его
	// перевели на local-host, а старый токен не отозвали: в runners.token_id
	// остался только local-host, прежний токен виден в журнале регистраций.
	if err := s.RegisterRunner(ctx, domain.Runner{ID: "local-worker", Agent: "rivet", Host: "mac", Capabilities: []string{"coding"},
		Depth: domain.DepthFull, Protocol: "12"}, domain.RunnerToken{ID: fleet, Name: "old-fleet"}); err != nil {
		t.Fatal(err)
	}
	tok := domain.RunnerToken{ID: shared, Name: "local-host"}
	for _, id := range []string{"local-worker", "local-reviewer"} {
		if err := s.RegisterRunner(ctx, domain.Runner{ID: id, Agent: "rivet", Host: "mac", Capabilities: []string{"coding"},
			Depth: domain.DepthFull, Protocol: "12"}, tok); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO runners (id, agent, host) VALUES ('ancient', 'rivet', 'old')`)

	if err := migrateFiles(ctx, url, migrationsBefore(t, "9999")); err != nil {
		t.Fatal(err)
	}
	if got := boundIDs(tokenByName(t, s, "local-host")); !slices.Equal(got, []string{"local-reviewer:migration", "local-worker:migration"}) {
		t.Fatalf("перенос общего токена: %v", got)
	}
	if got := boundIDs(tokenByName(t, s, "old-fleet")); !slices.Equal(got, []string{"local-worker:migration"}) {
		t.Fatalf("перенос прежнего токена из журнала: %v", got)
	}
	if _, err := s.AuthorizeRunner(ctx, fleet, "intruder", true); !errors.Is(err, ErrRunnerIdentity) {
		t.Fatalf("прежний токен флота не должен стать свободным: %v", err)
	}
	if got := tokenByName(t, s, "unused").Runners; len(got) != 0 {
		t.Fatalf("у неиспользованного токена привязки: %v", got)
	}
	for _, id := range []string{"local-worker", "local-reviewer"} {
		if _, err := s.AuthorizeRunner(ctx, shared, id, true); err != nil {
			t.Fatalf("%s прежней командой: %v", id, err)
		}
	}
	_, err = s.AuthorizeRunner(ctx, shared, "local-tester", true)
	mustIdentityErr(t, err, "выпущен для других runner'ов")
	if bound, err := s.AuthorizeRunner(ctx, unused, "ancient", true); err != nil || !bound {
		t.Fatalf("неиспользованный токен привязывается при первой регистрации: %v %v", bound, err)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runner_token_bindings WHERE runner_id='ancient' AND source='migration'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("runner без токена перенесён: %d %v", n, err)
	}
}
