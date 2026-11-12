package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func (s *Server) getEpicAssessment(w http.ResponseWriter, r *http.Request) {
	epic, err := s.St.EpicForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.St.EpicAssessment(r.Context(), epic.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
func (s *Server) retryEpicAssessment(w http.ResponseWriter, r *http.Request) {
	epic, err := s.St.EpicForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err = s.St.RetryEpicAssessment(r.Context(), epic.ID); err != nil {
		writeErr(w, err)
		return
	}
	v, err := s.St.EpicAssessment(r.Context(), epic.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

// The same durable controller handles recovery and automatic continuation.
// Successful architecture never starts coding; it only releases a requested plan.
func (s *Server) RunEpicAssessments(ctx context.Context) {
	if s.Engine == nil {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		if err := s.ReconcileEpicAssessments(ctx); err != nil && ctx.Err() == nil {
			slog.Error("очередь архитектурных оценок недоступна")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) ReconcileEpicAssessments(ctx context.Context) error {
	due, err := s.St.DueEpicAssessments(ctx)
	if err != nil {
		return err
	}
	for _, a := range due {
		s.Engine.EnqueueOps(ctx, orchestrator.OpsJob{Kind: orchestrator.JobEpicAssessment, RefID: a.EpicID, ProjectID: a.ProjectID})
	}
	ids, err := s.St.DueAssessmentPlans(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		epic, err := s.St.GetEpic(ctx, id)
		if err != nil {
			continue
		}
		if budget, err := s.St.ProjectBudget(ctx, epic.ProjectID, time.Now()); err != nil || budget.PausedUntil != nil {
			if err = s.St.PostponeAssessmentPlan(ctx, id, "планирование ждёт восстановления дневного бюджета"); err != nil {
				return err
			}
			continue
		}
		pl, state := s.plannerStatus()
		if pl == nil || state.State == "invalid" {
			if err = s.St.PostponeAssessmentPlan(ctx, id, "планирование ждёт доступной модели Тимлида"); err != nil {
				return err
			}
			continue
		}
		token, err := s.St.ClaimAssessmentPlan(ctx, id)
		if errors.Is(err, store.ErrAssessmentStale) {
			continue
		}
		if err != nil {
			return err
		}
		go func() {
			planCtx, cancel := context.WithTimeout(ctx, backlogDecomposeTimeout)
			defer cancel()
			finished := make(chan struct{})
			defer close(finished)
			go func() {
				renew := time.NewTicker(30 * time.Second)
				defer renew.Stop()
				for {
					select {
					case <-finished:
						return
					case <-planCtx.Done():
						return
					case <-renew.C:
						ok, err := s.St.RenewAssessmentPlan(planCtx, id, token)
						if err != nil || !ok {
							cancel()
							return
						}
					}
				}
			}()
			_, err := s.decomposeEpic(planCtx, epic)
			success := err == nil || errors.Is(err, errEpicHasTasks)
			finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancelFinish()
			if !success && !store.IsArchitectureRequired(err) {
				s.markDecompose(finish, id, "failed", "план не построен; ограниченный повтор или повторный запрос декомпозиции")
			}
			if err := s.St.FinishAssessmentPlan(finish, id, token, success); err != nil {
				slog.Error("не удалось сохранить продолжение планирования", "epic", id)
			}
		}()
	}
	return nil
}
