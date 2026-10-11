package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// output — куда пишут команды просмотра; тесты подменяют на буфер.
var output io.Writer = os.Stdout

// httpClient с таймаутом: CLI не должен зависать на подвисшем rivetd.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiURL — базовый адрес rivetd для команд просмотра.
func apiURL() string {
	if v := os.Getenv("RIVET_API_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

// apiGet выполняет GET к rivetd (PAT из RIVET_TOKEN) и декодирует JSON в out.
func apiGet(path string, out any) error {
	token := os.Getenv("RIVET_TOKEN")
	if token == "" {
		return fmt.Errorf("не задан RIVET_TOKEN: выпусти personal access token (POST /api/v1/tokens) и экспортируй его")
	}
	req, err := http.NewRequest(http.MethodGet, apiURL()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("rivetd отклонил RIVET_TOKEN (HTTP 401): токен отозван, истёк или неверен")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

func cmdProjects(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: rivet projects")
	}
	var projects []domain.Project
	if err := apiGet("/api/v1/projects", &projects); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(output, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tREPO")
	for _, p := range projects {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.ID, p.Name, p.RepoPath)
	}
	return tw.Flush()
}

func cmdEpics(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rivet epics <project-id>")
	}
	var epics []domain.Epic
	if err := apiGet("/api/v1/projects/"+args[0]+"/epics", &epics); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(output, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tSTATUS")
	for _, e := range epics {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.ID, e.Title, e.Status)
	}
	return tw.Flush()
}

func cmdTasks(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: rivet tasks <epic-id>")
	}
	// Список задач приходит внутри карточки Epic (GET /api/v1/epics/{id}).
	var epic struct {
		Tasks []domain.Task
	}
	if err := apiGet("/api/v1/epics/"+args[0], &epic); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(output, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tSTEP\tATTEMPTS")
	for _, t := range epic.Tasks {
		step := t.StepID
		if t.WaitReason != "" {
			step += " (" + t.WaitReason + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\n", t.ID, t.Title, t.Status, step, t.AttemptUsed, t.AttemptLimit)
	}
	return tw.Flush()
}
