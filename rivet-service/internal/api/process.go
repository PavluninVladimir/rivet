package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Участники-люди в процессе (change add-process-humans, api-contract):
// очередь «мои шаги» и вердикт человека по запуску.

// stepRunView — запуск участника в DTO задачи (api-contract StepRun).
type stepRunView struct {
	ID          int64      `json:"id"`
	Participant string     `json:"participant"`
	Kind        string     `json:"kind"`
	Agent       string     `json:"agent"`
	User        string     `json:"user"`
	Runner      string     `json:"runner"`
	Verdict     string     `json:"verdict"`
	Detail      string     `json:"detail"`
	By          string     `json:"by"`
	FinishedAt  *time.Time `json:"finished_at"`
	// Role — роль участника, Model — модель запуска, AgentID и AgentName —
	// агент команды запуска (add-agent-roles).
	Role      string `json:"role"`
	Model     string `json:"model,omitempty"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
}

func runView(r store.StepRun) stepRunView {
	v := stepRunView{ID: r.ID, Participant: r.Participant, Kind: "agent", Runner: r.RunnerID,
		Verdict: r.Verdict, Detail: r.Detail, By: r.VerdictBy, FinishedAt: r.FinishedAt,
		Role: r.Role, Model: r.DisplayModel(), AgentID: r.AgentID, AgentName: r.AgentName}
	if m := r.DisplayModel(); r.AgentKind != "" || m != "" {
		v.Agent = r.AgentKind + "/" + m
		if m == "" {
			v.Agent = r.AgentKind
		}
	}
	if r.IsUser() {
		v.Kind, v.User = "user", r.UserLogin
		if r.UserRole != "" {
			v.User = "role:" + r.UserRole
		}
	}
	return v
}

func runViews(runs []store.StepRun) []stepRunView {
	out := make([]stepRunView, 0, len(runs))
	for _, r := range runs {
		out = append(out, runView(r))
	}
	return out
}

// maxStepHistory: сколько последних переходов отдаёт граф процесса.
const maxStepHistory = 500

// getTaskProcess отдаёт процесс задачи для графа в деталке (api-contract
// fix-web-console-tails): снимок процесса, по которому идёт задача (null без
// снимка), последние переходы по шагам и текущие шаг и статус задачи.
// Отдельно от GET /tasks/{id}: деталка перечитывает задачу на каждое
// событие проекта, а процесс только на переходы и смену статуса этой
// задачи. История читается раньше задачи: если между запросами закоммитится
// новый переход, позиция задачи окажется впереди истории, а не позади.
// Граф принимает такое расхождение за вход на шаг без события; обратное
// он показал бы ложной эскалацией.
func (s *Server) getTaskProcess(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireTaskMember(w, r, id) {
		return
	}
	history, err := s.St.TaskStepHistory(r.Context(), id, maxStepHistory)
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.St.GetTask(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var process any
	if len(t.Process) > 0 {
		process = t.Process
	}
	writeJSON(w, http.StatusOK, map[string]any{"process": process, "step_history": history,
		"step": t.StepID, "status": t.Status})
}

// stepItemView — элемент очереди «мои шаги» (api-contract StepItem).
type stepItemView struct {
	RunID       int64     `json:"run_id"`
	Task        taskRef   `json:"task"`
	Project     nameRef   `json:"project"`
	Epic        nameRef   `json:"epic"`
	Step        stepRef   `json:"step"`
	Participant string    `json:"participant"`
	Addressed   string    `json:"addressed"`
	Ask         string    `json:"ask"`
	Context     string    `json:"context"`
	CreatedAt   time.Time `json:"created_at"`
}

type taskRef struct {
	ID     string `json:"id"`
	Num    int64  `json:"num"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Branch string `json:"branch"`
	PRURL  string `json:"pr_url"`
}

type nameRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type stepRef struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// mySteps — GET /me/steps.
func (s *Server) mySteps(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	items, err := s.St.MySteps(r.Context(), u.ID, u.Login)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]stepItemView, 0, len(items))
	for _, it := range items {
		title := it.Run.StepID
		if proc := store.TaskProcess(it.Task); proc != nil {
			if st, ok := proc.Step(it.Run.StepID); ok {
				title = st.Title
			}
		}
		out = append(out, stepItemView{
			RunID: it.Run.ID,
			Task: taskRef{ID: it.Task.ID, Num: it.Task.Num, Title: it.Task.Title, Status: string(it.Task.Status),
				Branch: it.Task.Branch, PRURL: it.Task.PRURL},
			Project: nameRef{ID: it.ProjectID, Title: it.Project}, Epic: nameRef{ID: it.EpicID, Title: it.Epic},
			Step:        stepRef{ID: it.Run.StepID, Kind: it.Run.StepKind, Title: title},
			Participant: it.Run.Participant, Addressed: it.Addressed, Ask: it.Run.StepKind,
			Context: it.Context, CreatedAt: it.Run.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// runVerdict — POST /tasks/{id}/runs/{run}/verdict.
func (s *Server) runVerdict(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if !s.requireTaskMember(w, r, taskID) {
		return
	}
	runID, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	if err != nil {
		writeErr(w, store.ErrNotFound)
		return
	}
	var in struct {
		Verdict string `json:"verdict"`
		Detail  string `json:"detail"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	switch in.Verdict {
	case policy.OutcomeOk, policy.OutcomeChanges, policy.OutcomeFail:
	default:
		unprocessable(w, "verdict: ожидается ok, changes или fail")
		return
	}
	u := currentUser(r)
	run, err := s.St.RunForVerdict(r.Context(), taskID, runID, u.ID, u.Login)
	if errors.Is(err, store.ErrNotAddressed) {
		writeJSON(w, http.StatusForbidden, map[string]apiError{"error": {Code: "forbidden", Message: err.Error()}})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if run.Verdict != "" {
		writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "run_closed", Message: "запуск уже закрыт"}})
		return
	}
	in.Detail = strings.TrimSpace(in.Detail)
	if in.Verdict == policy.OutcomeChanges && run.StepKind == policy.StepReview && in.Detail == "" {
		unprocessable(w, "detail: замечания обязательны")
		return
	}
	task, err := s.St.GetTask(r.Context(), taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.Engine.ApplyVerdict(r.Context(), task, run, in.Verdict, in.Detail, u.Login); err != nil {
		if errors.Is(err, orchestrator.ErrRunClosed) {
			// Запуск не взят: либо его закрыл другой адресат (409), либо
			// адресация изменилась между проверкой и вердиктом (403).
			if _, aerr := s.St.RunForVerdict(r.Context(), taskID, runID, u.ID, u.Login); errors.Is(aerr, store.ErrNotAddressed) {
				writeJSON(w, http.StatusForbidden, map[string]apiError{"error": {Code: "forbidden", Message: aerr.Error()}})
				return
			}
			writeJSON(w, http.StatusConflict, map[string]apiError{"error": {Code: "run_closed", Message: "запуск уже закрыт"}})
			return
		}
		writeErr(w, err)
		return
	}
	task, err = s.St.GetTask(r.Context(), taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}
