package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/history"
)

// Импорт истории проекта (спека domain-model «Импорт истории проекта»):
// history-manifest собирает манифест из архива OpenSpec и PR хостинга,
// history-import отправляет его в API. Разделение даёт посмотреть манифест
// глазами до импорта.

func cmdHistoryManifest(args []string) error {
	fs := flag.NewFlagSet("rivet history-manifest", flag.ContinueOnError)
	archive := fs.String("archive", "", "каталог changes/archive хранилища OpenSpec")
	repo := fs.String("repo", "", "основной репозиторий owner/name (короткое имя — rivet)")
	extra := fs.String("repos", "", "остальные репозитории через запятую: owner/name (короткое имя — часть после «/»)")
	mapFile := fs.String("map", "", "карта PR без метки: {\"имя-change\": {\"rivet\": 8, \"rivet-web\": 3}}")
	apiBase := fs.String("github-api", "https://api.github.com", "адрес API GitHub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archive == "" || *repo == "" {
		return fmt.Errorf("нужны -archive и -repo")
	}
	changes, err := history.ReadArchive(*archive)
	if err != nil {
		return fmt.Errorf("архив: %w", err)
	}
	token := os.Getenv("RIVET_GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	if strings.TrimSpace(*repo) == "" {
		return fmt.Errorf("укажи основной репозиторий: -repo owner/name")
	}
	repos := []string{*repo}
	if *extra != "" {
		repos = append(repos, strings.Split(*extra, ",")...)
	}
	var prs []history.PullRequest
	for _, full := range repos {
		full = strings.TrimSpace(full)
		// Короткое имя репозитория совпадает с названием секции в tasks.md
		// (rivet, rivet-web, rivet-e2e).
		short := full[strings.LastIndex(full, "/")+1:]
		list, err := mergedPRs(context.Background(), *apiBase, token, full, short)
		if err != nil {
			return fmt.Errorf("PR %s: %w", full, err)
		}
		prs = append(prs, list...)
	}
	var lm history.LinkMap
	if *mapFile != "" {
		raw, err := os.ReadFile(*mapFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &lm); err != nil {
			return fmt.Errorf("карта: %w", err)
		}
	}
	links, report := history.LinkChanges(changes, prs, lm)
	mainRepo := repos[0][strings.LastIndex(repos[0], "/")+1:]
	m := history.BuildManifest(links, report.OrphanPRs, mainRepo)
	// Отчёт — в stderr, манифест — в stdout: их удобно разделять.
	fmt.Fprintf(os.Stderr, "change'ей: %d, PR: %d, без PR: %d, PR без change'а: %d\n",
		len(changes), len(prs), len(report.ChangesWithoutPR), len(report.OrphanPRs))
	for _, name := range report.ChangesWithoutPR {
		fmt.Fprintln(os.Stderr, "  без PR:", name)
	}
	for _, pr := range report.OrphanPRs {
		fmt.Fprintf(os.Stderr, "  PR без change'а: %s#%d %s\n", pr.Repo, pr.Number, pr.Title)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// maxPRPages — аварийный предел страниц по 100 PR на репозиторий.
const maxPRPages = 500

// mergedPRs — смерженные PR репозитория через API GitHub (постранично).
func mergedPRs(ctx context.Context, apiBase, token, full, short string) ([]history.PullRequest, error) {
	var out []history.PullRequest
	// Идём до короткой страницы; maxPRPages — аварийный предел, о котором
	// сообщаем, чтобы неполный список не выглядел полным.
	for page := 1; ; page++ {
		if page > maxPRPages {
			fmt.Fprintf(os.Stderr, "предупреждение: %s — прочитано только %d страниц PR, старые PR не учтены\n", full, maxPRPages)
			break
		}
		url := fmt.Sprintf("%s/repos/%s/pulls?state=closed&per_page=100&page=%d", apiBase, full, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("github: %d: %s", resp.StatusCode, clipBytes(raw))
		}
		var items []struct {
			Number   int        `json:"number"`
			Title    string     `json:"title"`
			Body     string     `json:"body"`
			HTMLURL  string     `json:"html_url"`
			MergedAt *time.Time `json:"merged_at"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.MergedAt == nil {
				continue // закрыт без merge — не история
			}
			out = append(out, history.PullRequest{Repo: short, Number: it.Number, Title: it.Title,
				Body: it.Body, URL: it.HTMLURL, MergedAt: *it.MergedAt})
		}
		if len(items) < 100 {
			break
		}
	}
	return out, nil
}

func cmdHistoryImport(args []string) error {
	fs := flag.NewFlagSet("rivet history-import", flag.ContinueOnError)
	api := fs.String("api", "http://localhost:8080", "адрес API Rivet")
	token := fs.String("token", os.Getenv("RIVET_TOKEN"), "personal access token владельца проекта")
	project := fs.String("project", "", "идентификатор проекта")
	manifest := fs.String("manifest", "", "файл манифеста (пусто — stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *token == "" {
		return fmt.Errorf("нужны -project и -token")
	}
	var raw []byte
	var err error
	if *manifest == "" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*manifest)
	}
	if err != nil {
		return err
	}
	var m history.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("манифест: %w", err)
	}
	if err := m.Normalize().Validate(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(*api, "/")+"/api/v1/projects/"+*project+"/history", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("импорт: %d: %s", resp.StatusCode, clipBytes(body))
	}
	fmt.Println(string(body))
	return nil
}

func clipBytes(b []byte) string {
	if len(b) > 500 {
		return string(b[:500]) + "…"
	}
	return string(b)
}
