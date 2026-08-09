package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// output — куда пишут команды просмотра; тесты подменяют на буфер.
var output io.Writer = os.Stdout

// apiURL — базовый адрес rivetd для команд просмотра.
func apiURL() string {
	if v := os.Getenv("RIVET_API_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

// apiGet выполняет GET к rivetd и декодирует JSON-ответ в out.
func apiGet(path string, out any) error {
	resp, err := http.Get(apiURL() + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
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
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.ID, p.Name, p.Repo)
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
	fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tATTEMPTS")
	for _, t := range epic.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\n", t.ID, t.Title, t.Status, t.AttemptUsed, t.AttemptLimit)
	}
	return tw.Flush()
}
