package stream

import (
	"context"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

type sinkSender struct{}

func (sinkSender) Send(string, *pb.PlaneMsg) bool { return true }

// Приватная сессия (спека team-visibility «Видимость по умолчанию и
// приватность»): шаги не пишутся в event log и не публикуются в Hub,
// live-вывод не транслируется, пересечения не считаются; last_step и файлы
// копятся на сессии для автора.
func TestPrivateSessionFiltering(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	owner, err := st.CreateUser(ctx, "alice", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := st.CreateProject(ctx, "p", "o/r", nil, owner.ID)
	epic, _ := st.CreateEpic(ctx, p.ID, "E", "")
	taskA, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "Приватная"})
	taskB, _ := st.CreateTask(ctx, epic.ID, store.NewTask{Title: "Обычная"})

	mk := func(taskID, prompt string, private bool) string {
		id, err := st.CreateSession(ctx, domain.Session{TaskID: taskID, Attempt: 1,
			DriverKind: "user", DriverID: "alice", Agent: "claude-code",
			Depth: domain.DepthFull, Scope: "FIXING", Prompt: prompt, Private: private})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	priv := mk(taskA.ID, "секретный промпт", true)
	pub := mk(taskB.ID, "обычный промпт", false)

	hub := NewHub()
	logs, unsub := hub.Subscribe(p.ID)
	defer unsub()
	eng := orchestrator.New(st, nil, (*blob.Store)(nil), sinkSender{}, 0)
	srv := &Server{St: st, Engine: eng, Hub: hub}

	// Шаг приватной сессии: событие не пишется, файлы и last_step — да.
	if err := srv.handle(ctx, "r1", &pb.RunnerMsg{Kind: &pb.RunnerMsg_Event{Event: &pb.AgentEvent{
		TaskId: taskA.ID, SessionId: priv, Kind: "tool", Tool: "Edit",
		Detail: "shared.go", Files: []string{"shared.go"}, Ok: true, Text: "Edit shared.go"}}}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := st.Events(ctx, store.EventFilter{TaskID: taskA.ID, Type: "session.step", Limit: 10}); len(evs) != 0 {
		t.Fatalf("шаги приватной сессии не должны попадать в event log: %+v", evs)
	}
	// Шаг обычной сессии с тем же файлом: событие есть, но пересечения с
	// приватной не возникает.
	if err := srv.handle(ctx, "r2", &pb.RunnerMsg{Kind: &pb.RunnerMsg_Event{Event: &pb.AgentEvent{
		TaskId: taskB.ID, SessionId: pub, Kind: "tool", Tool: "Edit",
		Detail: "shared.go", Files: []string{"shared.go"}, Ok: true, Text: "Edit shared.go"}}}); err != nil {
		t.Fatal(err)
	}
	if evs, _ := st.Events(ctx, store.EventFilter{TaskID: taskB.ID, Type: "session.step", Limit: 10}); len(evs) != 1 {
		t.Fatal("шаг обычной сессии должен писаться")
	}
	if evs, _ := st.Events(ctx, store.EventFilter{Type: "session.overlap", Limit: 10}); len(evs) != 0 {
		t.Fatalf("приватная сессия не участвует в пересечениях: %+v", evs)
	}

	// Live-вывод: приватная не публикуется в Hub, обычная — публикуется.
	srv.emitTranscript(ctx, taskA.ID, priv, []byte("тайное"))
	srv.emitTranscript(ctx, taskB.ID, pub, []byte("открытое"))
	select {
	case c := <-logs:
		if c.TaskID != taskB.ID || !strings.Contains(string(c.Data), "открытое") {
			t.Fatalf("в Hub должен попасть только вывод обычной сессии: %+v", c)
		}
	default:
		t.Fatal("вывод обычной сессии должен публиковаться")
	}
	select {
	case c := <-logs:
		t.Fatalf("лишний чанк в Hub: %+v", c)
	default:
	}

	// Чтение: автор видит содержимое, чужой участник — факт.
	if _, err := st.CreateUser(ctx, "bob", "", "pw-testpass", false); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, p.ID, "bob", "member"); err != nil {
		t.Fatal(err)
	}
	own, _ := st.ListTaskSessions(ctx, taskA.ID, "alice")
	if own[0].Prompt == "" || own[0].LastStep == "" || own[0].Files == nil {
		t.Fatalf("автор должен видеть содержимое: %+v", own[0])
	}
	other, _ := st.ListTaskSessions(ctx, taskA.ID, "bob")
	if other[0].Prompt != "" || other[0].LastStep != "" || other[0].Files != nil || !other[0].Private {
		t.Fatalf("чужая приватная сессия должна быть фактом: %+v", other[0])
	}
	// Реестр: маскировка для bob, у alice всё видно; поиск чужих приватных пуст.
	regBob, _ := st.ActiveProjectSessions(ctx, p.ID, "bob")
	for _, e := range regBob {
		if e.ID == priv && (e.Prompt != "" || e.LastStep != "" || e.Files != nil) {
			t.Fatalf("реестр не должен раскрывать приватную: %+v", e)
		}
	}
	if found, _ := st.SearchProjectSessions(ctx, p.ID, "секретный", "bob", 0); len(found) != 0 {
		t.Fatalf("поиск не должен находить чужую приватную: %+v", found)
	}
	if found, _ := st.SearchProjectSessions(ctx, p.ID, "секретный", "alice", 0); len(found) != 1 {
		t.Fatalf("автор ищет свою приватную: %+v", found)
	}

	// Транскрипт приватной — только автору, остальным 404 (неотличимо от
	// отсутствия).
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET transcript_ref='s3://x' WHERE id=$1`, priv); err != nil {
		t.Fatal(err)
	}
	bob, err := st.Authenticate(ctx, "bob", "pw-testpass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionTranscriptForViewer(ctx, priv, bob.ID); err == nil {
		t.Fatal("чужой участник не должен получать транскрипт приватной")
	}
	if ref, err := st.SessionTranscriptForViewer(ctx, priv, owner.ID); err != nil || ref == "" {
		t.Fatalf("автор должен получать транскрипт: %v %q", err, ref)
	}
}
