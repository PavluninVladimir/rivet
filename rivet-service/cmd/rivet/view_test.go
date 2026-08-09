package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// стенд: подменяет API-сервер и перехватывает вывод команды.
func run(t *testing.T, handler http.HandlerFunc, cmd func([]string) error, args []string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	t.Setenv("RIVET_API_URL", srv.URL)
	var buf bytes.Buffer
	output = &buf
	defer func() { output = nil }()
	err := cmd(args)
	return buf.String(), err
}

func TestCmdProjectsTable(t *testing.T) {
	out, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" {
			t.Errorf("путь %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"ID":"p1","Name":"Rivet","Repo":"o/r"}]`))
	}, cmdProjects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ID") || !strings.Contains(out, "p1") || !strings.Contains(out, "Rivet") {
		t.Errorf("нет ожидаемых колонок: %q", out)
	}
}

func TestCmdEpicsUsage(t *testing.T) {
	if err := cmdEpics(nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("ожидалась usage-ошибка, получено %v", err)
	}
}

func TestCmdTasksTable(t *testing.T) {
	out, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/epics/e1" {
			t.Errorf("путь %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"Tasks":[{"ID":"t1","Title":"Задача","Status":"ready","AttemptUsed":1,"AttemptLimit":3}]}`))
	}, cmdTasks, []string{"e1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1/3") || !strings.Contains(out, "ready") {
		t.Errorf("нет статуса или попыток: %q", out)
	}
}

func TestAPIErrorBody(t *testing.T) {
	_, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "нет такого проекта", http.StatusNotFound)
	}, cmdEpics, []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "нет такого проекта") {
		t.Errorf("ошибка должна содержать тело ответа, получено %v", err)
	}
}
