package stream

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/runnertls"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Идентичность runner'а (change add-runner-identity, спека runners
// «Токены регистрации runner'ов» и «Клиентские сертификаты runner'ов»):
// объявленный идентификатор должен совпадать с клиентским сертификатом
// соединения (mTLS) и входить в привязки предъявленного токена. Проверка
// идёт до любых действий с записью runner'а, его задачами и публикациями.

// identityDenied: отказ по токену наружу, без списка runner'ов токена.
func identityDenied(runnerID string) error {
	return status.Error(codes.PermissionDenied,
		"токен регистрации не выпущен для runner'а "+runnerID+
			": выпустите токен для него в консоли («Управление приложением» → «Runner'ы») или запустите runner с идентификатором, для которого выпущен токен")
}

// certDenied: отказ по клиентскому сертификату наружу.
func certDenied(runnerID string) error {
	return status.Error(codes.PermissionDenied,
		"клиентский сертификат выдан не для runner'а "+runnerID+
			": Common Name или DNS-имя из SAN сертификата должно совпадать с идентификатором runner'а")
}

// identityCheck: откуда проверка. Регистрация может привязать токен без
// привязок, канал нет.
type identityCheck struct {
	runnerID, host, agent string
	register              bool
}

// checkIdentity сверяет идентификатор runner'а с сертификатом соединения и
// привязками токена. Отказ пишет runner.rejected (с окном частоты) и
// возвращает PERMISSION_DENIED.
func (s *Server) checkIdentity(ctx context.Context, c identityCheck) error {
	// Недопустимый идентификатор (пустой, длиннее предела, с пробелами или
	// управляющими символами) отклоняется до сверки, события и окна
	// отказов: иначе токен без привязки закрепился бы за мусорной строкой,
	// а перебор многомегабайтных идентификаторов раздул бы память, журнал и
	// event log. Правило то же, что при выпуске токена в консоли.
	if p := domain.RunnerIDProblem(c.runnerID); p != "" {
		s.logInvalidRequest(ctx, c.runnerID, p)
		return status.Error(codes.InvalidArgument, p+": задайте -id по правилам идентификатора runner'а")
	}
	// Имя хоста и агент попадают в payload события отказа и в запись
	// runner'а: без предела держатель токена раздувал бы event log
	// многомегабайтными полями (ревью круг 2, P2-A).
	if p := requestFieldProblem(c.host, c.agent); p != "" {
		s.logInvalidRequest(ctx, c.runnerID, p)
		return status.Error(codes.InvalidArgument, p)
	}
	token := tokenFromContext(ctx)
	if cert := runnertls.PeerCertificate(ctx); cert != nil || s.ClientCertRequired {
		if !runnertls.Matches(cert, c.runnerID) {
			s.rejectIdentity(ctx, c, token, "клиентский сертификат выдан не для этого runner'а", cert)
			return certDenied(c.runnerID)
		}
	}
	if token.ID == "" {
		// Без интерцептора аутентификации (тесты пакета): в rivetd токен
		// есть всегда, без него запрос не доходит до обработчика.
		return nil
	}
	bound, err := s.St.AuthorizeRunner(ctx, token.ID, c.runnerID, c.register)
	var ie *store.IdentityError
	switch {
	case errors.As(err, &ie):
		s.rejectIdentity(ctx, c, token, ie.Reason, nil)
		return identityDenied(c.runnerID)
	case errors.Is(err, store.ErrNotFound):
		// Токен отозвали или он истёк между интерцептором и привязкой:
		// тот же единый отказ, что и для недействительного токена.
		return errUnauthenticated
	case err != nil:
		return err
	}
	if bound {
		slog.Info("токен регистрации привязан при первой регистрации", "runner", c.runnerID, "token", token.Name)
	}
	return nil
}

// Пределы полей регистрации, которые идут в запись runner'а и в событие
// отказа. os.Hostname не возвращает больше 255 байт, агент runner'а: это
// rivet, пусто у runner'а выкатки или короткое имя стороннего CLI.
const (
	maxHostLen  = 255
	maxAgentLen = 64
)

// requestFieldProblem: что не так с именем хоста или агентом из запроса
// регистрации, или пустая строка. Значения в текст не попадают.
func requestFieldProblem(host, agent string) string {
	bad := func(v string) bool {
		return !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0
	}
	switch {
	case len(host) > maxHostLen:
		return fmt.Sprintf("имя хоста runner'а длиннее %d байт", maxHostLen)
	case len(agent) > maxAgentLen:
		return fmt.Sprintf("агент runner'а длиннее %d байт", maxAgentLen)
	case bad(host) || bad(agent):
		return "имя хоста или агент runner'а содержит управляющие символы или не в UTF-8"
	}
	return ""
}

// invalidLogEvery: не чаще одной строки журнала rivetd об отклонённом
// недопустимом запросе регистрации на всю установку: след для
// администратора без пути для раздувания журнала.
const invalidLogEvery = time.Minute

// logInvalidRequest пишет в журнал rivetd недопустимый запрос регистрации
// не чаще раза в invalidLogEvery. Идентификатор обрезан и в кавычках, чтобы
// были видны пробелы и управляющие символы (\r из env-файла).
func (s *Server) logInvalidRequest(ctx context.Context, runnerID, problem string) {
	now := time.Now()
	s.rejMu.Lock()
	if now.Sub(s.invalidLogAt) < invalidLogEvery {
		s.rejMu.Unlock()
		return
	}
	s.invalidLogAt = now
	s.rejMu.Unlock()
	addr := ""
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
	}
	slog.Warn("отклонён запрос runner'а с недопустимым полем (следующая такая строка не раньше чем через минуту)",
		"причина", problem, "runner", fmt.Sprintf("%q", clip(runnerID, 64)), "peer", addr)
}

// rejectIdentity пишет отказ в event log и журнал rivetd не чаще раза в
// окно на runner и причину: runner прежней версии переподключается каждые
// несколько секунд.
func (s *Server) rejectIdentity(ctx context.Context, c identityCheck, token domain.RunnerToken, reason string, cert *x509.Certificate) {
	write, suppressed := s.noteRejection(c.runnerID, reason, time.Now())
	if !write {
		return
	}
	what := "канал"
	if c.register {
		what = "регистрация"
	}
	slog.Warn("отказ runner'у по идентичности", "runner", c.runnerID, "rpc", what, "token", token.Name, "reason", reason)
	payload := map[string]any{
		"runner_id": c.runnerID, "host": c.host, "agent": c.agent,
		"token_id": token.ID, "token_name": token.Name, "reason": reason, "kind": "identity",
	}
	if cert != nil {
		payload["cert_cn"] = cert.Subject.CommonName
	}
	if suppressed > 0 {
		payload["repeated"] = suppressed
	}
	text := "регистрация runner'а " + c.runnerID + " отклонена: " + reason
	if !c.register {
		text = "канал runner'а " + c.runnerID + " отклонён: " + reason
	}
	if token.Name != "" {
		text += " (токен «" + token.Name + "»)"
	}
	if _, err := s.St.AppendEvent(context.WithoutCancel(ctx), store.EventInput{
		ActorKind: domain.ActorRunner, ActorID: c.runnerID, Type: "runner.rejected", Text: text, Payload: payload,
	}); err != nil {
		slog.Error("событие отказа по идентичности", "err", err)
	}
}
