package stream

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/runnertls"
)

// Аутентификация runner'ов токеном регистрации (спека runners «Токены
// регистрации runner'ов», design add-operations-management): секрет в
// метаданных authorization на Register и Channel. Отказ единый — без
// различения «нет», «неизвестен», «отозван», «просрочен». Привязку токена
// к идентификатору runner'а проверяют сами Register и Channel
// (identity.go): идентификатор приходит в теле запроса и в Hello.

type tokenKey struct{}

// errUnauthenticated — единый отказ для всех причин.
var errUnauthenticated = status.Error(codes.Unauthenticated, "токен регистрации runner'а не принят")

// errNoClientCert: установка требует mTLS, а у соединения нет
// проверенного клиентского сертификата. При верной сборке TLS сервера
// такое соединение не проходит рукопожатие: это защита от ошибки опций.
var errNoClientCert = status.Error(codes.Unauthenticated, "нужен клиентский сертификат runner'а")

// authenticate проверяет секрет из метаданных и возвращает токен.
func (s *Server) authenticate(ctx context.Context) (domain.RunnerToken, error) {
	if s.ClientCertRequired && runnertls.PeerCertificate(ctx) == nil {
		return domain.RunnerToken{}, errNoClientCert
	}
	md, _ := metadata.FromIncomingContext(ctx)
	var secret string
	for _, v := range md.Get("authorization") {
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			secret = strings.TrimSpace(v[len("bearer "):])
		}
	}
	if secret == "" {
		return domain.RunnerToken{}, errUnauthenticated
	}
	t, err := s.St.RunnerTokenBySecret(ctx, secret)
	if err != nil {
		return domain.RunnerToken{}, errUnauthenticated
	}
	return t, nil
}

// UnaryAuth — интерцептор Register: токен кладётся в контекст для записи
// регистрации.
func (s *Server) UnaryAuth() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		t, err := s.authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, tokenKey{}, t), req)
	}
}

// StreamAuth — интерцептор Channel: Hello верить нельзя, канал без токена
// не открывается. Токен кладётся в контекст потока: Channel сверяет с ним
// идентификатор из Hello.
func (s *Server) StreamAuth() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		t, err := s.authenticate(ss.Context())
		if err != nil {
			return err
		}
		return next(srv, &authedStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), tokenKey{}, t)})
	}
}

// authedStream: поток канала с токеном в контексте.
type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a *authedStream) Context() context.Context { return a.ctx }

// tokenFromContext — токен, положенный UnaryAuth; пустой без интерцептора
// (тесты без аутентификации).
func tokenFromContext(ctx context.Context) domain.RunnerToken {
	t, _ := ctx.Value(tokenKey{}).(domain.RunnerToken)
	return t
}
