package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/infra"
	"github.com/PavluninVladimir/rivet/internal/infra/ruvds"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Действия людей с хостом окружения (api-contract add-environment-provisioning).
// Права (владелец проекта, сессия консоли) проверяет API; здесь: допустимые
// состояния и переходы. Подтверждение создания и удаления: единственные
// пути в approved и deleting у платного провайдера.

// Действия с хостом.
const (
	HostActRequest       = "request"
	HostActApprove       = "approve"
	HostActReject        = "reject"
	HostActDelete        = "delete"
	HostActConfirmDelete = "confirm-delete"
	HostActCancelDelete  = "cancel-delete"
	HostActRetry         = "retry"
	HostActRefresh       = "refresh"
)

// HostActionInput: действие человека.
type HostActionInput struct {
	Action        string
	Actor         string
	Amount        *float64
	RemoveVolumes bool
}

// RequestHostIfNeeded: привязанное окружение без живого хоста получает
// хост (сохранение привязки администратором).
func (e *Engine) RequestHostIfNeeded(ctx context.Context, env domain.Environment, actor string) error {
	if env.Config.Provision == nil {
		return nil
	}
	if _, live, err := e.St.LiveHost(ctx, env.ID); err != nil || live {
		return err
	}
	_, err := e.St.RequestHost(ctx, env, actor)
	if errors.Is(err, store.ErrHostLive) {
		return nil
	}
	return err
}

// HostAction выполняет действие человека и возвращает хост после него.
func (e *Engine) HostAction(ctx context.Context, env domain.Environment, in HostActionInput) (domain.EnvHost, error) {
	if in.Action == HostActRequest {
		if env.Config.Provision == nil {
			return domain.EnvHost{}, errors.Join(store.ErrInvalid, errors.New("окружение не привязано к провайдеру хоста"))
		}
		return e.St.RequestHost(ctx, env, in.Actor)
	}
	h, live, err := e.St.LiveHost(ctx, env.ID)
	if err != nil {
		return h, err
	}
	if !live {
		return h, &store.ErrHostState{State: "нет хоста"}
	}
	var c store.HostChange
	switch in.Action {
	case HostActApprove:
		if in.Amount == nil {
			return h, errors.Join(store.ErrInvalid, errors.New("нужна сумма, которую подтверждает человек"))
		}
		amount := fmt.Sprint(*in.Amount)
		if h.QuoteAmount != nil {
			amount = money(*h.QuoteAmount, h.QuoteCurrency)
		}
		c = store.HostChange{From: []string{domain.HostAwaitingApproval}, To: domain.HostApproved,
			Where: "abs(quote_amount - {1}) < 0.005", Args: []any{*in.Amount},
			Set: map[string]any{"approved_by": in.Actor, "approved_at": store.SQL("now()"),
				"approved_amount": store.SQL("quote_amount"), "next_attempt_at": store.SQL("now()"), "detail": ""},
			Resolve: []domain.AttentionReason{domain.AttHostCreate},
			Event:   "создание сервера подтверждено: " + amount}
	case HostActReject:
		c = store.HostChange{From: []string{domain.HostAwaitingApproval}, To: domain.HostRejected,
			Set:        map[string]any{"ended_at": store.SQL("now()")},
			ResolveAll: true, Event: "создание сервера отклонено"}
	case HostActDelete:
		c, err = e.deleteRequest(h, in)
		if err != nil {
			return h, err
		}
	case HostActConfirmDelete:
		c = store.HostChange{From: []string{domain.HostDeleteRequested}, To: domain.HostDeleting,
			Where: "delete_requested_by <> ''",
			Set: map[string]any{"delete_confirmed_by": in.Actor, "job_id": "", "job_kind": "", "job_runner": "",
				"job_started_at": nil, "needs_retry": false, "attempts": 0, "next_attempt_at": store.SQL("now()")},
			Resolve: []domain.AttentionReason{domain.AttHostDelete, domain.AttHostFailed},
			// Адрес и ключ уходят из конфигурации сразу: ни публикация, ни
			// проверка не пойдут на сервер, который удаляется.
			Env:   clearHostConfig,
			Event: "удаление подтверждено"}
	case HostActCancelDelete:
		prev := h.PrevState
		if prev == "" || domain.HostTerminal(prev) || prev == domain.HostDeleteRequested || prev == domain.HostDeleting {
			prev = domain.HostReady
		}
		c = store.HostChange{From: []string{domain.HostDeleteRequested}, To: prev,
			Set:     map[string]any{"prev_state": "", "delete_requested_by": "", "remove_volumes": false, "next_attempt_at": store.SQL("now()")},
			Resolve: []domain.AttentionReason{domain.AttHostDelete},
			Event:   "удаление отменено"}
	case HostActRetry:
		c = store.HostChange{From: []string{domain.HostBootstrapping, domain.HostDeleting}, Where: "needs_retry",
			Set: map[string]any{"needs_retry": false, "attempts": 0, "failure": "", "action_id": store.SQL(
				"CASE WHEN state = 'deleting' THEN '' ELSE action_id END"), "next_attempt_at": store.SQL("now()")},
			Resolve: []domain.AttentionReason{domain.AttHostFailed},
			Event:   "повтор запущен"}
	case HostActRefresh:
		if h.Provider == infra.KindLocal {
			return h, &store.ErrHostState{State: h.State}
		}
		c = store.HostChange{From: []string{domain.HostReady}, Set: map[string]any{"next_attempt_at": store.SQL("now()")}}
	default:
		return h, errors.Join(store.ErrInvalid, errors.New("неизвестное действие "+in.Action))
	}
	c.Action, c.Actor = in.Action, in.Actor
	out, ok, err := e.St.ChangeHost(ctx, h.ID, c)
	if err != nil {
		return out, err
	}
	if !ok {
		if in.Action == HostActApprove && out.State == domain.HostAwaitingApproval {
			return out, store.ErrPriceChanged
		}
		return out, &store.ErrHostState{State: out.State}
	}
	return out, nil
}

// deleteRequest: запрос удаления: решение HOST_DELETE, а у локального хоста
// без удаления томов сразу снятие (денег и данных это не касается).
func (e *Engine) deleteRequest(h domain.EnvHost, in HostActionInput) (store.HostChange, error) {
	local := h.Provider == infra.KindLocal
	from := []string{domain.HostReady, domain.HostBootstrapping}
	if !local && h.State == domain.HostCreating {
		if h.ServerID == "" {
			return store.HostChange{}, &store.ErrHostState{State: h.State}
		}
		from = append(from, domain.HostCreating)
	}
	if local && !in.RemoveVolumes {
		return store.HostChange{From: from, To: domain.HostDeleting,
			Set: map[string]any{"prev_state": store.SQL("state"), "delete_requested_by": in.Actor,
				"delete_confirmed_by": in.Actor, "remove_volumes": false, "job_id": "", "job_kind": "",
				"job_runner": "", "job_started_at": nil, "needs_retry": false, "attempts": 0,
				"next_attempt_at": store.SQL("now()")},
			Resolve: []domain.AttentionReason{domain.AttHostFailed},
			Env:     clearHostConfig,
			Event:   "снятие локального окружения: контейнеры останавливаются, тома сохраняются"}, nil
	}
	var msg string
	if local {
		msg = fmt.Sprintf("снять локальное окружение и удалить тома compose-проекта %s. Данные в томах пропадут безвозвратно.",
			domain.ComposeProject(h.ProjectID, h.EnvID))
	} else {
		msg = fmt.Sprintf("удалить сервер RUVDS %s", h.ServerID)
		if h.Address != "" {
			msg += " (" + h.Address + ")"
		}
		msg += ". Удаление необратимо: сервер и данные на нём пропадут."
		if h.PaidTill != nil {
			msg += " Оплачено до " + h.PaidTill.In(time.UTC).Format("02.01.2006") + "."
		}
		if h.CostAmount != nil {
			msg += fmt.Sprintf(" Стоимость продления %s за %s.", money(*h.CostAmount, h.CostCurrency), ruvds.PeriodLabel(h.CostPeriod))
		}
	}
	return store.HostChange{From: from, To: domain.HostDeleteRequested,
		Set: map[string]any{"prev_state": store.SQL("state"), "delete_requested_by": in.Actor,
			"remove_volumes": in.RemoveVolumes},
		Open:  []store.HostAttention{{Reason: domain.AttHostDelete, Message: msg}},
		Event: "запрошено удаление: ждёт подтверждения"}, nil
}

// clearHostConfig: адрес и ключ хоста больше не действуют.
func clearHostConfig(cfg *domain.EnvConfig) {
	cfg.Host, cfg.HostKey = "", ""
}
