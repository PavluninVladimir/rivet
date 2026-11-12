package infra

import "context"

// Local: провайдер «машина deploy-runner'а» (design, решение 10): сервер
// не создаётся, денег и решения нет, хост готов сразу. Доставка идёт
// существующим путём с пустым host, снятие: командами compose.
type Local struct{}

func (Local) Kind() string { return KindLocal }
func (Local) Remote() bool { return false }
func (Local) Paid() bool   { return false }

func (Local) Quote(context.Context, Spec) (Quote, error) { return Quote{}, nil }

func (Local) Balance(context.Context) (*Money, error) { return nil, nil }

func (Local) Find(context.Context, string) (*Server, error) { return nil, nil }

func (Local) Create(context.Context, Spec, Labels) (Server, error) {
	return Server{ID: KindLocal, State: ServerActive, Progress: 100}, nil
}

func (Local) Poll(_ context.Context, srv Server) (Server, error) {
	if srv.State == ServerDeleted {
		return srv, nil
	}
	return Server{ID: KindLocal, State: ServerActive, Progress: 100}, nil
}

func (Local) Delete(context.Context, string) (Server, error) {
	return Server{ID: KindLocal, State: ServerDeleted}, nil
}

func (Local) Billing(context.Context, string) (Billing, error) { return Billing{}, nil }

// LocalBootstrap: проверка, что на машине runner'а есть docker compose.
const LocalBootstrap = "docker compose version"

func (Local) BootstrapScript(s Spec) string {
	if s.Bootstrap != "" {
		return s.Bootstrap
	}
	return LocalBootstrap
}

// TeardownScript снимает compose-проект окружения: имя приходит в
// COMPOSE_PROJECT_NAME, тома удаляются только по решению человека.
func (Local) TeardownScript(_ Spec, removeVolumes bool) string {
	cmd := `docker compose -p "$COMPOSE_PROJECT_NAME" down --remove-orphans`
	if removeVolumes {
		cmd += " --volumes"
	}
	return cmd
}
