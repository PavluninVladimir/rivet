package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

func (s *Server) getIssueIntake(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	if !pathIDs(w, pid) || !s.requireMember(w, r, pid) {
		return
	}
	out, err := s.St.IssueIntake(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) patchIssueIntake(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	if !pathIDs(w, pid) || !s.requireOwner(w, r, pid) {
		return
	}
	var c store.IssueIntakeConfig
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := decodeJSONLimit(r, &c); err != nil {
		discErr(w, http.StatusBadRequest, "invalid", "неверные настройки наблюдения Issues", "", nil)
		return
	}
	if err := s.St.SetIssueIntake(r.Context(), pid, user(r), c); err != nil {
		writeDiscoveryErr(w, err)
		return
	}
	out, err := s.St.IssueIntake(r.Context(), pid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RunIssueIntake only dispatches jobs; the existing per-role SRE queue owns
// execution and fairness. No public webhook or manual sync is required.
func (s *Server) RunIssueIntake(ctx context.Context) {
	if s.Engine == nil {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		ids, err := s.St.DueIssueIntakes(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("очередь наблюдения Issues недоступна")
		}
		for _, id := range ids {
			s.Engine.EnqueueOps(ctx, orchestrator.OpsJob{Kind: orchestrator.JobGitHubIssues, RefID: id, ProjectID: id})
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// PollProjectIssues is called by the real SRE worker. Every write validates its
// durable lease, including the final research start after network I/O.
func (s *Server) PollProjectIssues(ctx context.Context, pid, agentID string) error {
	l, err := s.St.ClaimIssueIntake(ctx, pid)
	if errors.Is(err, store.ErrIssueLease) {
		return nil
	}
	if err != nil {
		return errors.New("не удалось получить задание наблюдения Issues")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	page, etag, detail, delay := l.Page, l.ETag, "", time.Minute
	defer func() {
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.St.FinishIssueIntake(finish, l, page, etag, detail, delay); err != nil {
			slog.Error("не удалось сохранить проверку Issues", "project", pid)
		}
	}()
	role, err := s.St.GetRole(ctx, domain.RoleSRE)
	if err != nil || !role.Enabled {
		detail = "наблюдение ждёт включения роли сопровождения"
		return nil
	}
	p, err := s.St.GetProject(ctx, pid)
	if err != nil || p.Provider != "github" {
		detail = "проект GitHub недоступен"
		return errors.New(detail)
	}
	a, err := s.Engine.SCMFor(ctx, p)
	if err != nil {
		detail = "проверьте подключение GitHub проекта"
		return errors.New(detail)
	}
	reader, ok := a.(scm.IssueReader)
	if !ok {
		detail = "адаптер проекта не поддерживает GitHub Issues"
		return errors.New(detail)
	}
	for n := 0; n < 3; n++ {
		sendETag := ""
		if page == 1 {
			sendETag = etag
		}
		result, err := reader.ListIssues(ctx, p.Repo(), l.Label, page, sendETag)
		if err != nil {
			detail = "GitHub Issues недоступны; проверьте подключение и права Issues: read"
			if errors.Is(err, scm.ErrRateLimited) {
				detail = "GitHub ограничил запросы; проверка будет повторена позже"
				delay = 2 * time.Minute
				var rate *scm.RateLimitError
				if errors.As(err, &rate) {
					delay = max(delay, min(rate.After, 24*time.Hour))
				}
			}
			return errors.New(detail)
		}
		if result.NotModified {
			break
		}
		for _, issue := range result.Issues {
			if err := s.St.ImportIssue(ctx, l, p, issue, agentID); err != nil {
				if errors.Is(err, store.ErrIssueLease) {
					return nil
				}
				detail = "не удалось сохранить Issue как материал исследования"
				if errors.Is(err, store.ErrFeedbackLimit) {
					detail = "достигнут лимит 500 отзывов исследования"
				}
				return errors.New(detail)
			}
		}
		// Cache validators only when the entire scan fit on page 1. A 304
		// must never hide updates on later pages of a paginated scan.
		if result.NextPage == 0 {
			if page == 1 {
				etag = result.ETag
			} else {
				etag = ""
			}
			page = 1
			break
		}
		page, etag = result.NextPage, ""
	}
	if !l.AutoResearch {
		return nil
	}
	pending, err := s.St.IssueResearchPending(ctx, l)
	if err != nil {
		detail = "не удалось проверить передачу в исследование"
		return errors.New(detail)
	}
	if !pending {
		return nil
	}
	if b, err := s.St.ProjectBudget(ctx, pid, time.Now()); err != nil {
		detail = "не удалось проверить бюджет исследования"
		return errors.New(detail)
	} else if b.PausedUntil != nil {
		detail = "исследование ждёт восстановления дневного бюджета"
		return nil
	}
	setup, err := s.resolveAnalyst(ctx)
	if err != nil {
		detail = "исследование ждёт настройки доступной модели Аналитика"
		if errors.Is(err, errAnalystDisabled) {
			detail = "исследование ждёт включения роли Аналитика"
		}
		return nil
	}
	run, texts, err := s.St.StartIssueResearch(ctx, l, store.NewRun{AgentID: setup.agentID, AgentName: setup.who.Name, RoleTitle: setup.who.Title,
		Model: setup.model.Model, ConnectionID: setup.model.ConnectionID, InstructionSHA: setup.who.InstructionSHA()})
	if err != nil {
		var busy *store.AnalysisRunningError
		switch {
		case errors.As(err, &busy):
			detail = "передача ждёт завершения текущего исследования"
		case errors.Is(err, store.ErrIssueLease):
			return nil
		default:
			detail = "не удалось начать исследование; будет повторная попытка"
		}
		return nil
	}
	s.launchAnalysis(ctx, run, texts, setup, domain.ActorSystem, agentID)
	return nil
}
