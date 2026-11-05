package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Архитектура проекта (add-architecture-c4, api-contract): снимок описания,
// постановка задачи описания и ручное обновление снимка. Доступ у любого
// участника проекта, чужой проект неотличим от несуществующего.

// architectureView — api-contract ProjectArchitecture.
type architectureView struct {
	ProjectID string     `json:"project_id"`
	Status    string     `json:"status"`
	SHA       *string    `json:"sha"`
	ReadAt    *time.Time `json:"read_at"`
	Error     *string    `json:"error"`
	// ErrorKind — вид ошибки при status=error: host, limit, data,
	// no_credentials (решения по контракту, пункт 4); иначе null.
	ErrorKind *string                     `json:"error_kind"`
	Defects   json.RawMessage             `json:"defects"`
	Source    architectureSource          `json:"source"`
	Current   json.RawMessage             `json:"current"`
	LastValid *architectureLastValid      `json:"last_valid"`
	Task      *store.ArchitectureTaskInfo `json:"task"`
}

type architectureSource struct {
	Branch string `json:"branch"`
	Path   string `json:"path"`
	URL    string `json:"url"`
}

type architectureLastValid struct {
	SHA    string          `json:"sha"`
	ReadAt *time.Time      `json:"read_at"`
	Data   json.RawMessage `json:"data"`
}

func (s *Server) getProjectArchitecture(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	p, err := s.St.GetProject(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	row, exists, err := s.St.GetProjectArchitecture(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	task, hasTask, err := s.St.ArchitectureTask(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	branch := p.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	v := architectureView{
		ProjectID: projectID, Status: "pending", Defects: json.RawMessage("[]"),
		Source:  architectureSource{Branch: branch, Path: architecture.DefaultDir, URL: treeURL(p.Provider, p.WebURL(), branch, architecture.DefaultDir)},
		Current: json.RawMessage("null"),
	}
	if hasTask {
		v.Task = &task
	}
	if !exists {
		// Снимок ещё не строился: построение идёт в фоне, результат придёт
		// событием project.architecture_updated.
		if p.RepoPath != "" {
			s.refreshArchitecture(projectID, false)
		}
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.Status, v.ReadAt, v.Defects = row.Status, row.ReadAt, row.Defects
	if row.SHA != "" {
		sha := row.SHA
		v.SHA = &sha
	}
	if row.Error != "" {
		msg := row.Error
		v.Error = &msg
	}
	if row.Status == "error" && row.ErrorKind != "" {
		kind := row.ErrorKind
		v.ErrorKind = &kind
	}
	if row.Status == architecture.SnapshotOK && len(row.Data) > 0 {
		v.Current = row.Data
	}
	if (row.Status == "invalid" || row.Status == "error") && row.ValidSHA != "" && len(row.ValidData) > 0 {
		v.LastValid = &architectureLastValid{SHA: row.ValidSHA, ReadAt: row.ValidReadAt, Data: row.ValidData}
	}
	writeJSON(w, http.StatusOK, v)
}

// refreshArchitecture ставит обновление снимка в фон. Сервер без движка
// (часть тестов API) снимков не строит.
func (s *Server) refreshArchitecture(projectID string, force bool) {
	if s.Engine != nil {
		s.Engine.RefreshArchitecture(projectID, force)
	}
}

// connectArchitecture — первый снимок после подключения репозитория
// (повод «подключение» автоописания).
func (s *Server) connectArchitecture(projectID string) {
	if s.Engine != nil {
		s.Engine.ConnectArchitecture(projectID)
	}
}

// treeURL — ссылка на каталог описания на хостинге.
func treeURL(provider, web, branch, dir string) string {
	if web == "" {
		return ""
	}
	if provider == string(scm.ProviderGitLab) {
		return web + "/-/tree/" + branch + "/" + dir
	}
	return web + "/tree/" + branch + "/" + dir
}

func (s *Server) describeProjectArchitecture(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	// Постановка задачи описания запускает системный Epic: тот же запрет
	// движка политик, что у запуска Epic (спека access-policy «Точки
	// принуждения»).
	if s.policyBlocks(w, r, actionEpicStatus, projectID) {
		return
	}
	epicID, taskID, err := s.Engine.DescribeArchitecture(r.Context(), projectID, "manual", user(r))
	var active *store.ErrArchitectureTaskActive
	switch {
	case errors.As(err, &active):
		// details — объект, а не строка с JSON (api-contract: в
		// error.details лежат epic_id и task_id идущей задачи).
		writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{
			"code": "architecture_task_active", "message": "задача описания архитектуры уже идёт",
			"details": map[string]string{"epic_id": active.EpicID, "task_id": active.TaskID}}})
		return
	case errors.Is(err, orchestrator.ErrRepoNotConnected):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]apiError{"error": {
			Code: "repo_not_connected", Message: "у проекта нет подключённого репозитория"}})
		return
	case errors.Is(err, orchestrator.ErrNoCredentials):
		writeNoCredentials(w)
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"epic_id": epicID, "task_id": taskID})
}

// writeNoCredentials — 422 no_credentials: проект на глобальном токене
// установки, описание архитектуры не читается и не пишется.
func writeNoCredentials(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]apiError{"error": {
		Code: "no_credentials", Message: "у проекта нет собственных учётных данных хостинга: подключите токен проекта"}})
}

// refreshProjectArchitecture — ручное перечитывание снимка. 202 pending —
// чтение запущено, придёт project.architecture_updated. 200 fresh — снимок
// того же SHA прочитан меньше 30 секунд назад, чтение не запускается и
// события не будет, retry_after_s — через сколько можно повторить.
func (s *Server) refreshProjectArchitecture(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireMember(w, r, projectID) {
		return
	}
	p, err := s.St.GetProject(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.RepoPath == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]apiError{"error": {
			Code: "repo_not_connected", Message: "у проекта нет подключённого репозитория"}})
		return
	}
	if s.Engine == nil {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	}
	if p.CredentialID == "" && !s.Engine.Adapters.Force {
		writeNoCredentials(w)
		return
	}
	wait, err := s.Engine.RefreshWait(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if wait > 0 {
		secs := int(math.Ceil(wait.Seconds()))
		writeJSON(w, http.StatusOK, map[string]any{"status": "fresh", "retry_after_s": secs})
		return
	}
	s.Engine.RefreshArchitecture(projectID, true)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
}
