package stream

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/runnertls"
	"github.com/PavluninVladimir/rivet/internal/runnertls/tlstest"
	"github.com/PavluninVladimir/rivet/internal/store"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Идентичность runner'а (add-runner-identity, спека runners «Токены
// регистрации runner'ов» и «Клиентские сертификаты runner'ов»).

func identityAdmin(t *testing.T, st *store.Store) domain.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), "root", "", "pw-root-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func registerReq(id string) *pb.RegisterRequest {
	return &pb.RegisterRequest{RunnerId: id, Agent: "rivet", Host: "h-" + id, Capabilities: []string{"coding"},
		ProtocolVersion: protocolVersion, Adapter: "native", Depth: "full", Features: []string{"native"}, BootId: "boot-" + id}
}

func bearer(ctx context.Context, secret string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+secret)
}

func identityEvents(t *testing.T, st *store.Store, runnerID string) []domain.Event {
	t.Helper()
	evs, err := st.Events(context.Background(), store.EventFilter{Installation: true, Type: "runner.rejected"})
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Event
	for _, e := range evs {
		if e.ActorID == runnerID && e.Payload["kind"] == "identity" {
			out = append(out, e)
		}
	}
	return out
}

func mustDenied(t *testing.T, what string, err error, contains string) {
	t.Helper()
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), contains) {
		t.Fatalf("%s: ожидался PermissionDenied «…%s…», получено %v", what, contains, err)
	}
}

// channelRecv открывает канал, шлёт Hello и ждёт ответа сервера.
func channelRecv(ctx context.Context, client pb.RunnerServiceClient, runnerID string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := client.Channel(ctx)
	if err != nil {
		return err
	}
	if err := s.Send(&pb.RunnerMsg{MsgId: "hello-" + runnerID, Kind: &pb.RunnerMsg_Hello{Hello: &pb.Hello{RunnerId: runnerID}}}); err != nil {
		return err
	}
	_, err = s.Recv()
	return err
}

// Чужой идентификатор при действующем токене: отказ до любых действий с
// записью runner'а и его публикацией, событие с именем токена.
func TestRegisterForeignRunnerID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin := identityAdmin(t, st)
	_, own, err := st.CreateRunnerToken(ctx, "office-2", nil, []string{"office-2"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	_, stolen, err := st.CreateRunnerToken(ctx, "office-1", nil, []string{"office-1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthServer(t, st)
	if resp, err := client.Register(bearer(ctx, own), registerReq("office-2")); err != nil || !resp.Accepted {
		t.Fatalf("законная регистрация office-2: %v %+v", err, resp)
	}
	// У office-2 идёт публикация. Сервер теста без Engine: если бы
	// проверка идентичности шла после FailDeploymentNow, обработчик упал
	// бы на nil, а не вернул отказ.
	p, err := st.CreateProject(ctx, "demo", "o/r", nil, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "staging", ExecType: "ssh", Trigger: "manual",
		Config: domain.EnvConfig{DeployCmd: "true", VerifyCmd: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.EnqueueDeployment(ctx, env.ID, "sha-1", "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE deployments SET status='deploying', runner_id='office-2' WHERE id=$1`, dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE runners SET deployment_id=$1 WHERE id='office-2'`, dep.ID); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		req := registerReq("office-2")
		req.Host, req.BootId = "intruder", "boot-intruder"
		_, err = client.Register(bearer(ctx, stolen), req)
		mustDenied(t, "регистрация под чужим идентификатором", err, "токен регистрации не выпущен для runner'а office-2")
		if strings.Contains(status.Convert(err).Message(), "office-1") {
			t.Fatalf("отказ раскрывает runner'ов токена: %v", err)
		}
	}
	rn, err := st.GetRunner(ctx, "office-2")
	if err != nil || rn.Host != "h-office-2" {
		t.Fatalf("запись office-2 изменена: %v %+v", err, rn)
	}
	var depStatus, bootID string
	if err := st.Pool.QueryRow(ctx, `SELECT d.status, r.boot_id FROM deployments d, runners r WHERE d.id=$1 AND r.id='office-2'`, dep.ID).Scan(&depStatus, &bootID); err != nil {
		t.Fatal(err)
	}
	if depStatus != "deploying" || bootID != "boot-office-2" {
		t.Fatalf("публикация или boot_id office-2 тронуты: %s %s", depStatus, bootID)
	}
	// Событие одно за окно, с именем токена и причиной.
	evs := identityEvents(t, st, "office-2")
	if len(evs) != 1 || evs[0].Payload["token_name"] != "office-1" || evs[0].Payload["host"] != "intruder" ||
		evs[0].Payload["reason"] != "токен выпущен для других runner'ов" || !strings.Contains(evs[0].Text, "регистрация runner'а office-2 отклонена") {
		t.Fatalf("событие отказа: %+v", evs)
	}
	if regs, _ := st.Events(ctx, store.EventFilter{Installation: true, Type: "runner.registered"}); len(regs) != 1 {
		t.Fatalf("лишняя регистрация в журнале: %d", len(regs))
	}
}

// Канал верит не Hello, а привязке токена: чужой идентификатор не
// подключается к очереди назначений, токен без привязки канал не открывает.
func TestChannelForeignRunnerID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin := identityAdmin(t, st)
	_, secret, err := st.CreateRunnerToken(ctx, "office-1", nil, []string{"office-1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	_, free, err := st.CreateRunnerToken(ctx, "free", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	rs := &Server{St: st, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second}
	client := dialServer(t, rs)
	if resp, err := client.Register(bearer(ctx, secret), registerReq("office-1")); err != nil || !resp.Accepted {
		t.Fatalf("регистрация office-1: %v %+v", err, resp)
	}
	err = channelRecv(bearer(ctx, secret), client, "office-2")
	mustDenied(t, "канал под чужим идентификатором", err, "office-2")
	if rs.Reg.Connected("office-2") {
		t.Fatal("канал чужого идентификатора подключён к очереди назначений")
	}
	if evs := identityEvents(t, st, "office-2"); len(evs) != 1 || !strings.Contains(evs[0].Text, "канал runner'а office-2 отклонён") {
		t.Fatalf("событие отказа канала: %+v", evs)
	}
	err = channelRecv(bearer(ctx, free), client, "build-7")
	mustDenied(t, "канал по токену без привязки", err, "build-7")
	// Пустой идентификатор не забирает свободный токен.
	if _, err := client.Register(bearer(ctx, free), registerReq("")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("пустой идентификатор: %v", err)
	}

	// Первая регистрация привязывает свободный токен, второму runner'у отказ,
	// и runner текущей версии на нём завершается (код отличается от
	// недействительного токена).
	if resp, err := client.Register(bearer(ctx, free), registerReq("build-7")); err != nil || !resp.Accepted {
		t.Fatalf("первая регистрация build-7: %v %+v", err, resp)
	}
	_, err = client.Register(bearer(ctx, free), registerReq("build-8"))
	mustDenied(t, "второй runner по привязанному токену", err, "build-8")
	if bound, _ := st.Events(ctx, store.EventFilter{Installation: true, Type: "runner_token.bound"}); len(bound) != 1 || bound[0].ActorID != "build-7" {
		t.Fatalf("событие привязки: %+v", bound)
	}
}

// dialServer: bufconn-клиент к серверу с интерцепторами аутентификации.
func dialServer(t *testing.T, rs *Server) pb.RunnerServiceClient {
	t.Helper()
	return dialServerCreds(t, rs, nil, nil)
}

// dialServerCreds: настоящий TCP на loopback, если заданы TLS-настройки
// сервера и клиента, иначе bufconn без TLS.
func dialServerCreds(t *testing.T, rs *Server, server, client credentials.TransportCredentials) pb.RunnerServiceClient {
	t.Helper()
	if server == nil {
		return dialAuthServerWith(t, rs)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(server), grpc.UnaryInterceptor(rs.UnaryAuth()), grpc.StreamInterceptor(rs.StreamAuth()))
	pb.RegisterRunnerServiceServer(srv, rs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewRunnerServiceClient(conn)
}

// mTLS с настоящими сертификатами: свой CN и DNS SAN проходят, чужой даёт
// отказ с событием, без сертификата, с чужим корнем и просроченным
// сертификатом соединение не проходит рукопожатие.
func TestMutualTLSIdentity(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin := identityAdmin(t, st)
	_, secret, err := st.CreateRunnerToken(ctx, "office", nil, []string{"office-1", "office-2", "office-3"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	ca := tlstest.NewCA(t, "runners")
	srvCert, srvKey := ca.Server(t)
	serverCfg, err := runnertls.ServerConfig(srvCert, srvKey, ca.File)
	if err != nil {
		t.Fatal(err)
	}
	rs := &Server{St: st, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second, ClientCertRequired: true}
	serverCreds := runnertls.LogHandshakeFailures(credentials.NewTLS(serverCfg))
	dial := func(certFile, keyFile string) pb.RunnerServiceClient {
		t.Helper()
		cfg, _, err := runnertls.ClientConfig(ca.File, certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		return dialServerCreds(t, rs, serverCreds, credentials.NewTLS(cfg))
	}
	authed := bearer(ctx, secret)

	// Свой Common Name: регистрация, канал защищён.
	c1, k1 := ca.Issue(t, "office-1", tlstest.Leaf{CN: "office-1"})
	office1 := dial(c1, k1)
	resp, err := office1.Register(authed, registerReq("office-1"))
	if err != nil || !resp.Accepted || !resp.Secure {
		t.Fatalf("office-1 со своим сертификатом: %v %+v", err, resp)
	}
	// Сертификат office-1 не годится для office-2, даже если токен общий.
	_, err = office1.Register(authed, registerReq("office-2"))
	mustDenied(t, "сертификат другого runner'а", err, "клиентский сертификат выдан не для runner'а office-2")
	evs := identityEvents(t, st, "office-2")
	if len(evs) != 1 || evs[0].Payload["cert_cn"] != "office-1" || !strings.Contains(evs[0].Text, "клиентский сертификат") {
		t.Fatalf("событие отказа по сертификату: %+v", evs)
	}
	if _, err := st.GetRunner(ctx, "office-2"); err == nil {
		t.Fatal("office-2 зарегистрирован чужим сертификатом")
	}
	err = channelRecv(authed, office1, "office-2")
	mustDenied(t, "канал с сертификатом другого runner'а", err, "office-2")

	// DNS-имена из SAN: один сертификат на хост с двумя runner'ами.
	cs, ks := ca.Issue(t, "host-a", tlstest.Leaf{CN: "host-a", DNS: []string{"office-2", "office-3"}})
	hostA := dial(cs, ks)
	for _, id := range []string{"office-2", "office-3"} {
		if resp, err := hostA.Register(authed, registerReq(id)); err != nil || !resp.Accepted {
			t.Fatalf("%s по DNS SAN: %v %+v", id, err, resp)
		}
	}

	// Без сертификата, с чужим корнем, просроченный: рукопожатие не
	// проходит, до регистрации дело не доходит.
	other := tlstest.NewCA(t, "other")
	co, ko := other.Issue(t, "office-4", tlstest.Leaf{CN: "office-4"})
	ce, ke := ca.Issue(t, "expired", tlstest.Leaf{CN: "office-4", NotAfter: time.Now().Add(-time.Minute)})
	noCertCfg, _, err := runnertls.ClientConfig(ca.File, "", "")
	if err != nil {
		t.Fatal(err)
	}
	foreignCfg, _, err := runnertls.ClientConfig(ca.File, co, ko)
	if err != nil {
		t.Fatal(err)
	}
	expiredCfg, _, err := runnertls.ClientConfig(ca.File, ce, ke)
	if err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]credentials.TransportCredentials{
		"без сертификата": credentials.NewTLS(noCertCfg),
		"чужой корень":    credentials.NewTLS(foreignCfg),
		"просрочен":       credentials.NewTLS(expiredCfg),
	} {
		c := dialServerCreds(t, rs, serverCreds, cfg)
		cctx, cancel := context.WithTimeout(authed, 5*time.Second)
		_, err := c.Register(cctx, registerReq("office-4"))
		cancel()
		if err == nil || status.Code(err) == codes.PermissionDenied {
			t.Fatalf("%s: ожидался отказ рукопожатия, получено %v", name, err)
		}
		t.Logf("%s: %v", name, err)
	}
	if _, err := st.GetRunner(ctx, "office-4"); err == nil {
		t.Fatal("office-4 зарегистрирован без годного сертификата")
	}
}

// Сервер с обязательным mTLS, до которого дошло соединение без
// сертификата (ошибка сборки опций), отказывает в аутентификации.
func TestClientCertRequiredWithoutTLS(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin := identityAdmin(t, st)
	_, secret, err := st.CreateRunnerToken(ctx, "office", nil, []string{"office-1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	rs := &Server{St: st, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second, ClientCertRequired: true}
	client := dialServer(t, rs)
	_, err = client.Register(bearer(ctx, secret), registerReq("office-1"))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("без сертификата при обязательном mTLS: %v", err)
	}
	if err := channelRecv(bearer(ctx, secret), client, "office-1"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("канал без сертификата при обязательном mTLS: %v", err)
	}
}

// Потолок карты окна событий: перебор идентификаторов держателем токена
// не раздувает ни карту, ни event log.
func TestRejectionKeysCap(t *testing.T) {
	var s Server
	now := time.Now()
	for i := range rejectedKeysMax {
		if write, _ := s.noteRejection(fmt.Sprintf("r%d", i), "reason", now); !write {
			t.Fatalf("отказ %d до потолка не записан", i)
		}
	}
	if write, _ := s.noteRejection("one-more", "reason", now); write {
		t.Fatal("сверх потолка событие записано")
	}
	if len(s.rejSeen) != rejectedKeysMax {
		t.Fatalf("карта выросла сверх потолка: %d", len(s.rejSeen))
	}
	// О переполнении предупреждают один раз: момент освобождения места
	// запомнен и повторные упоры его не сдвигают.
	until := s.rejFullUntil
	if !until.Equal(now.Add(rejectedEventWindow)) {
		t.Fatalf("переполнение не отмечено: %v", until)
	}
	if write, _ := s.noteRejection("and-more", "reason", now.Add(time.Minute)); write || !s.rejFullUntil.Equal(until) {
		t.Fatalf("повторный упор: %v %v", write, s.rejFullUntil)
	}
	// После окна старые ключи вычищаются и место освобождается.
	if write, _ := s.noteRejection("one-more", "reason", now.Add(rejectedEventWindow)); !write {
		t.Fatal("после окна новый отказ не записан")
	}
}

// Недопустимый идентификатор (длиннее предела, с управляющими символами,
// многомегабайтный) отклоняется INVALID_ARGUMENT до сверки: без события,
// без записи в окно отказов и без привязки свободного токена (ревью
// add-runner-identity, P2-1).
func TestInvalidRunnerIDRejectedEarly(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin := identityAdmin(t, st)
	_, secret, err := st.CreateRunnerToken(ctx, "office-1", nil, []string{"office-1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	_, free, err := st.CreateRunnerToken(ctx, "free", nil, nil, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	rs := &Server{St: st, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second}
	client := dialServer(t, rs)
	huge := strings.Repeat("x", 1<<20)
	for name, id := range map[string]string{
		"129 символов": strings.Repeat("r", 129), "перевод строки": "office-2\n", "пробел": "office 2", "мегабайт": huge,
	} {
		for _, sec := range []string{secret, free} {
			_, err := client.Register(bearer(ctx, sec), registerReq(id))
			if status.Code(err) != codes.InvalidArgument || strings.Contains(status.Convert(err).Message(), "xxxx") {
				t.Fatalf("регистрация, %s: %v", name, clip(fmt.Sprint(err), 300))
			}
		}
		if err := channelRecv(bearer(ctx, secret), client, id); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("канал, %s: %v", name, clip(fmt.Sprint(err), 300))
		}
	}
	if evs, _ := st.Events(ctx, store.EventFilter{Installation: true, Type: "runner.rejected"}); len(evs) != 0 {
		t.Fatalf("события отказа по недопустимому идентификатору: %d", len(evs))
	}
	rs.rejMu.Lock()
	n := len(rs.rejSeen)
	rs.rejMu.Unlock()
	if n != 0 {
		t.Fatalf("недопустимый идентификатор попал в окно отказов: %d", n)
	}
	list, err := st.ListRunnerTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if tok.Name == "free" && len(tok.Runners) != 0 {
			t.Fatalf("свободный токен привязан к недопустимому идентификатору: %+v", tok.Runners)
		}
	}
}

// Имя хоста и агент из запроса ограничены до сверки: держатель токена не
// раздувает event log многомегабайтными полями (ревью круг 2, P2-A).
func TestOversizedRegisterFieldsRejectedEarly(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	admin := identityAdmin(t, st)
	_, secret, err := st.CreateRunnerToken(ctx, "office-1", nil, []string{"office-1"}, admin.ID, admin.Login)
	if err != nil {
		t.Fatal(err)
	}
	rs := &Server{St: st, Reg: NewRegistry(), Hub: NewHub(), HeartbeatInterval: 30 * time.Second}
	client := dialServer(t, rs)
	for i, mut := range []func(*pb.RegisterRequest){
		func(r *pb.RegisterRequest) { r.Host = strings.Repeat("h", 1<<20) },
		func(r *pb.RegisterRequest) { r.Host = strings.Repeat("h", maxHostLen+1) },
		func(r *pb.RegisterRequest) { r.Agent = strings.Repeat("a", maxAgentLen+1) },
		func(r *pb.RegisterRequest) { r.Host = "mac\x1b[31m" },
		func(r *pb.RegisterRequest) { r.Agent = "rivet\n" },
	} {
		// Чужой идентификатор: без предела дошло бы до события отказа.
		req := registerReq(fmt.Sprintf("x-%d", i))
		mut(req)
		_, err := client.Register(bearer(ctx, secret), req)
		if status.Code(err) != codes.InvalidArgument || strings.Contains(status.Convert(err).Message(), "hhhh") {
			t.Fatalf("случай %d: %v", i, clip(fmt.Sprint(err), 300))
		}
	}
	if evs, _ := st.Events(ctx, store.EventFilter{Installation: true, Type: "runner.rejected"}); len(evs) != 0 {
		t.Fatalf("событие с недопустимым полем: %d", len(evs))
	}
	// На пределе поля проходят к сверке: чужой идентификатор получает
	// обычный отказ по токену, а payload события остаётся маленьким.
	req := registerReq("x-edge")
	req.Host, req.Agent = strings.Repeat("h", maxHostLen), strings.Repeat("a", maxAgentLen)
	_, err = client.Register(bearer(ctx, secret), req)
	mustDenied(t, "поля на пределе", err, "x-edge")
	evs := identityEvents(t, st, "x-edge")
	if len(evs) != 1 || len(fmt.Sprint(evs[0].Payload)) > 2048 {
		t.Fatalf("событие на пределе: %d", len(evs))
	}
}

// Затяжной перебор: слоты освобождаются по одному, а предупреждение о
// переполнении пишется одно на окно (ревью круг 2, P3-A).
func TestRejectionOverflowWarnsOncePerWindow(t *testing.T) {
	var s Server
	t0 := time.Now()
	for i := range rejectedKeysMax {
		if write, _ := s.noteRejection(fmt.Sprintf("r%d", i), "reason", t0.Add(time.Duration(i)*time.Second)); !write {
			t.Fatalf("отказ %d до потолка не записан", i)
		}
	}
	// Истекла первая запись: один новый отказ проходит, следующий
	// упирается в потолок и предупреждает.
	at := t0.Add(rejectedEventWindow + 500*time.Millisecond)
	if write, _ := s.noteRejection("n0", "reason", at); !write {
		t.Fatal("освободившийся слот не занят")
	}
	if write, _ := s.noteRejection("n1", "reason", at); write {
		t.Fatal("сверх потолка записано")
	}
	until := s.rejFullUntil
	if !until.Equal(at.Add(rejectedEventWindow)) {
		t.Fatalf("момент повторного предупреждения: %v", until)
	}
	// Дальше слоты освобождаются каждую секунду, перебор продолжается:
	// предупреждение не повторяется.
	for k := 1; k <= 60; k++ {
		now := at.Add(time.Duration(k) * time.Second)
		s.noteRejection(fmt.Sprintf("a%d", k), "reason", now)
		s.noteRejection(fmt.Sprintf("b%d", k), "reason", now)
		if !s.rejFullUntil.Equal(until) {
			t.Fatalf("предупреждение повторилось на шаге %d: %v", k, s.rejFullUntil)
		}
	}
}
