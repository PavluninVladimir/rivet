package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/ops"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Цикл, релизы, инциденты и наблюдение: права участника, владельца и
// администратора, валидация настроек наблюдения.
func TestSDLCAPI(t *testing.T) {
	f := seedEnvAPI(t)
	ctx := context.Background()
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	member := loginSession(t, f.srv, f.member, "pw-testpass")
	mallory := loginSession(t, f.srv, f.mallory, "pw-testpass")
	base := f.srv.URL + "/api/v1"
	envs := base + "/projects/" + f.projectID + "/environments"

	// Наблюдение: команда в несколько строк, интервал вне 1–1440, URL с
	// userinfo и включённое наблюдение без проверки отклоняются с полем.
	bad := []struct {
		monitor map[string]any
		field   string
	}{
		{map[string]any{"enabled": true, "check_cmd": "a\nb", "interval_min": 1}, "config.monitor.check_cmd"},
		{map[string]any{"enabled": true, "check_cmd": "true", "interval_min": 5000}, "config.monitor.interval_min"},
		{map[string]any{"enabled": true, "check_url": "http://u:p@h/", "interval_min": 1}, "config.monitor.check_url"},
		{map[string]any{"enabled": true, "interval_min": 1}, "config.monitor"},
	}
	for _, c := range bad {
		body := validEnvBody("demo")
		body["config"].(map[string]any)["monitor"] = c.monitor
		resp, raw := call(t, "POST", envs, admin, "", body)
		var e struct {
			Field string `json:"field"`
		}
		_ = json.Unmarshal(raw, &e)
		if resp.StatusCode != http.StatusUnprocessableEntity || e.Field != c.field {
			t.Fatalf("наблюдение %v: %d %s", c.monitor, resp.StatusCode, raw)
		}
	}
	pipe := map[string]any{"name": "ext", "exec_type": "pipeline", "trigger": "manual",
		"config": map[string]any{"verify_url": "https://example.test/health",
			"monitor": map[string]any{"enabled": true, "check_cmd": "true", "interval_min": 1}}}
	resp, raw := call(t, "POST", envs, admin, "", pipe)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "config.monitor.check_cmd") {
		t.Fatalf("команда для внешнего пайплайна: %d %s", resp.StatusCode, raw)
	}
	body := validEnvBody("demo")
	body["config"].(map[string]any)["monitor"] = map[string]any{"enabled": true, "check_cmd": "sh scripts/smoke.sh", "interval_min": 1}
	resp, raw = call(t, "POST", envs, admin, "", body)
	mustStatus(t, resp, http.StatusCreated, "окружение с наблюдением")
	var env struct {
		ID           string `json:"id"`
		MonitorState string `json:"monitor_state"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.MonitorState != "unknown" {
		t.Fatalf("monitor_state: %s", raw)
	}
	if !strings.Contains(string(raw), `"monitor_reason"`) || !strings.Contains(string(raw), `"monitor_last_at"`) {
		t.Fatalf("DTO окружения без причины и времени проверки: %s", raw)
	}
	// Выключение наблюдения сохраняет команды и URL проверки (ревью, круг
	// 3): у выключенного проверяются только интервал и userinfo.
	off := validEnvBody("off")
	off["config"].(map[string]any)["monitor"] = map[string]any{"enabled": false, "check_cmd": "sh scripts/smoke.sh",
		"logs_cmd": "tail -n 50 app.log", "check_url": "https://example.test/h", "interval_min": 5}
	resp, raw = call(t, "POST", envs, admin, "", off)
	mustStatus(t, resp, http.StatusCreated, "выключенное наблюдение")
	if !strings.Contains(string(raw), "sh scripts/smoke.sh") || !strings.Contains(string(raw), "tail -n 50 app.log") {
		t.Fatalf("поля выключенного наблюдения потеряны: %s", raw)
	}
	off["name"] = "off-userinfo"
	off["config"].(map[string]any)["monitor"] = map[string]any{"enabled": false, "check_url": "http://u:p@h/", "interval_min": 5}
	resp, raw = call(t, "POST", envs, admin, "", off)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "config.monitor.check_url") {
		t.Fatalf("userinfo у выключенного наблюдения: %d %s", resp.StatusCode, raw)
	}
	resp, _ = call(t, "GET", base+"/environments/"+env.ID+"/monitor", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "наблюдение участнику")
	resp, _ = call(t, "GET", base+"/environments/"+env.ID+"/monitor", mallory, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "наблюдение чужому")

	// Цикл: участнику восемь фаз, неверный период — 400, чужому — 404.
	resp, raw = call(t, "GET", base+"/projects/"+f.projectID+"/cycle?period=30d&tz=Europe/Moscow", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "цикл участнику")
	var v struct {
		Phases []struct {
			ID string `json:"id"`
		} `json:"phases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Phases) != 8 || v.Phases[0].ID != "discovery" {
		t.Fatalf("цикл: %s", raw)
	}
	resp, _ = call(t, "GET", base+"/projects/"+f.projectID+"/cycle?period=year", member, "", nil)
	mustStatus(t, resp, http.StatusBadRequest, "неверный период")
	resp, _ = call(t, "GET", base+"/projects/"+f.projectID+"/cycle?tz=Mars/Base", member, "", nil)
	mustStatus(t, resp, http.StatusBadRequest, "неверная зона")
	resp, _ = call(t, "GET", base+"/projects/"+f.projectID+"/cycle", mallory, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "цикл чужому")

	// Инцидент: список участнику, закрытие только владельцем.
	res, err := f.st.RecordFailure(ctx, store.FailureInput{EnvID: env.ID, Source: ops.SourceMonitor, Output: "error: 500", ConsecutiveFail: 1})
	if err != nil {
		t.Fatal(err)
	}
	resp, raw = call(t, "GET", base+"/projects/"+f.projectID+"/incidents", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "инциденты участнику")
	if !strings.Contains(string(raw), res.IncidentID) || strings.Contains(string(raw), `"evidence"`) {
		t.Fatalf("список инцидентов: %s", raw)
	}
	resp, raw = call(t, "GET", base+"/incidents/"+res.IncidentID, member, "", nil)
	mustStatus(t, resp, http.StatusOK, "инцидент участнику")
	if !strings.Contains(string(raw), `"evidence"`) || !strings.Contains(string(raw), `"history"`) {
		t.Fatalf("инцидент целиком: %s", raw)
	}
	resp, _ = call(t, "GET", base+"/incidents/"+res.IncidentID, mallory, "", nil)
	mustStatus(t, resp, http.StatusNotFound, "инцидент чужому")
	resolve := base + "/incidents/" + res.IncidentID + "/resolve"
	resp, _ = call(t, "POST", resolve, member, "", map[string]any{"comment": "ок"})
	mustStatus(t, resp, http.StatusForbidden, "закрытие участником")
	resp, _ = call(t, "POST", resolve, admin, "", map[string]any{"comment": ""})
	mustStatus(t, resp, http.StatusBadRequest, "пустой комментарий")
	resp, raw = call(t, "POST", resolve, admin, "", map[string]any{"comment": "проверено вручную"})
	mustStatus(t, resp, http.StatusOK, "закрытие владельцем")
	if !strings.Contains(string(raw), `"resolution":"manual"`) {
		t.Fatalf("закрытый инцидент: %s", raw)
	}
	resp, raw = call(t, "POST", resolve, admin, "", map[string]any{"comment": "ещё раз"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "incident_resolved") {
		t.Fatalf("повторное закрытие: %d %s", resp.StatusCode, raw)
	}

	// Релизы: пустой список и курсор null.
	resp, raw = call(t, "GET", base+"/projects/"+f.projectID+"/releases", member, "", nil)
	mustStatus(t, resp, http.StatusOK, "релизы участнику")
	if !strings.Contains(string(raw), `"releases":[]`) || !strings.Contains(string(raw), `"next":null`) {
		t.Fatalf("релизы: %s", raw)
	}
	resp, _ = call(t, "GET", base+"/projects/"+f.projectID+"/releases?limit=0", member, "", nil)
	mustStatus(t, resp, http.StatusBadRequest, "limit вне границ")
}

// Права на действия с релизами, инцидентами, Epic'ом «Сопровождение» и
// настройками наблюдения (ревью круг 1).
func TestSDLCAPIRights(t *testing.T) {
	f := seedEnvAPI(t)
	ctx := context.Background()
	admin := loginSession(t, f.srv, f.admin, "pw-testpass")
	member := loginSession(t, f.srv, f.member, "pw-testpass")
	base := f.srv.URL + "/api/v1"

	resp, raw := call(t, "POST", base+"/projects/"+f.projectID+"/environments", admin, "", validEnvBody("demo"))
	mustStatus(t, resp, http.StatusCreated, "окружение")
	var env struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &env)

	// Настройки наблюдения меняет только администратор.
	patch := map[string]any{"config": map[string]any{"deploy_cmd": "true", "verify_cmd": "true",
		"monitor": map[string]any{"enabled": true, "check_cmd": "true", "interval_min": 5}}}
	resp, _ = call(t, "PATCH", base+"/environments/"+env.ID, member, "", patch)
	mustStatus(t, resp, http.StatusForbidden, "наблюдение участником")
	resp, _ = call(t, "PATCH", base+"/environments/"+env.ID, admin, "", patch)
	mustStatus(t, resp, http.StatusOK, "наблюдение администратором")

	// Перегенерация release notes: участнику 403, владельцу 202, повтор 409.
	d, err := f.st.EnqueueDeployment(ctx, env.ID, "v1", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE deployments SET status='verifying' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CompleteDeployment(ctx, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	rel, err := f.st.ReleaseByDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetReleaseNotes(ctx, rel.ID, "# r", "template", "", "", 0); err != nil {
		t.Fatal(err)
	}
	notes := base + "/releases/" + rel.ID + "/notes"
	resp, _ = call(t, "POST", notes, member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "перегенерация участником")
	resp, _ = call(t, "POST", notes, admin, "", nil)
	mustStatus(t, resp, http.StatusAccepted, "перегенерация владельцем")
	// Задание отработает по шаблону (состав пуст): ждём итога, затем
	// ставим свежий pending без задания и проверяем отказ.
	for i := 0; i < 100; i++ {
		if r, _ := f.st.GetRelease(ctx, rel.ID); r.NotesStatus != "pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE releases SET notes_status='pending', notes_requested_at=now() WHERE id=$1`, rel.ID); err != nil {
		t.Fatal(err)
	}
	resp, raw = call(t, "POST", notes, admin, "", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "notes_pending") {
		t.Fatalf("повторная перегенерация: %d %s", resp.StatusCode, raw)
	}

	// «Создать баг заново»: участнику 403, владельцу 201, повтор 409 bug_exists.
	res, err := f.st.RecordFailure(ctx, store.FailureInput{EnvID: env.ID, Source: ops.SourceMonitor, Output: "error: 500", ConsecutiveFail: 1})
	if err != nil {
		t.Fatal(err)
	}
	bug := base + "/incidents/" + res.IncidentID + "/bug"
	resp, _ = call(t, "POST", bug, member, "", nil)
	mustStatus(t, resp, http.StatusForbidden, "баг участником")
	resp, _ = call(t, "POST", bug, admin, "", nil)
	mustStatus(t, resp, http.StatusCreated, "баг владельцем")
	resp, raw = call(t, "POST", bug, admin, "", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "bug_exists") {
		t.Fatalf("второй баг: %d %s", resp.StatusCode, raw)
	}

	// Epic «Сопровождение» не архивируется при открытом инциденте.
	var epicID string
	if err := f.st.Pool.QueryRow(ctx, `SELECT id FROM epics WHERE project_id=$1 AND kind='maintenance'`, f.projectID).Scan(&epicID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `UPDATE epics SET status='paused' WHERE id=$1`, epicID); err != nil {
		t.Fatal(err)
	}
	resp, raw = call(t, "POST", base+"/epics/"+epicID+"/archive", admin, "", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "maintenance_locked") {
		t.Fatalf("архивация Сопровождения: %d %s", resp.StatusCode, raw)
	}
}
