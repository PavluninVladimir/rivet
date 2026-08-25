package history

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Привязка change'ей к PR хостинга: PR с меткой «`имя-change`» в теле
// относится к change'у; PR без метки — по явной карте; остальное видно в
// отчёте, а не теряется молча.

// PullRequest — смерженный PR репозитория (то, что нужно импорту).
type PullRequest struct {
	Repo     string // короткое имя репозитория: rivet, rivet-web, rivet-e2e
	Number   int
	Title    string
	Body     string
	URL      string
	MergedAt time.Time
}

// LinkMap — явная карта для PR без метки: имя change'а → репозиторий →
// номер PR.
type LinkMap map[string]map[string]int

// Link — итог привязки change'а: PR по репозиториям.
type Link struct {
	Change Change
	PRs    map[string]PullRequest
}

// Report — что не удалось привязать.
type Report struct {
	ChangesWithoutPR []string
	OrphanPRs        []PullRequest
}

// LinkChanges привязывает PR к change'ам в два прохода: сначала по основной
// метке («Change `имя`», «часть change'а `имя`»), затем по любому
// упоминанию «`имя`» — иначе change-исправление, которое ссылается на
// родителя, отдало бы свой PR родителю. Потом — по карте. PR без change'а
// попадают в отчёт как сироты.
func LinkChanges(changes []Change, prs []PullRequest, lm LinkMap) ([]Link, Report) {
	used := map[string]bool{}
	key := func(pr PullRequest) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }
	links := make([]Link, len(changes))
	for i, c := range changes {
		links[i] = Link{Change: c, PRs: map[string]PullRequest{}}
	}
	// Проходы: основная метка, затем упоминание; в каждом — по возрастанию
	// номера PR, чтобы при двух кандидатах побеждал первый.
	sorted := append([]PullRequest(nil), prs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Repo != sorted[j].Repo {
			return sorted[i].Repo < sorted[j].Repo
		}
		return sorted[i].Number < sorted[j].Number
	})
	for _, match := range []func(body, name string) bool{primaryMark, anyMention} {
		for i := range links {
			name := links[i].Change.Name
			for _, pr := range sorted {
				if used[key(pr)] || !match(pr.Body, name) {
					continue
				}
				if _, taken := links[i].PRs[pr.Repo]; taken {
					continue
				}
				links[i].PRs[pr.Repo] = pr
				used[key(pr)] = true
			}
		}
	}
	var rep Report
	for i := range links {
		for repo, num := range lm[links[i].Change.Name] {
			if _, taken := links[i].PRs[repo]; taken {
				continue
			}
			for _, pr := range sorted {
				if pr.Repo == repo && pr.Number == num && !used[key(pr)] {
					links[i].PRs[repo] = pr
					used[key(pr)] = true
					break
				}
			}
		}
		if len(links[i].PRs) == 0 {
			rep.ChangesWithoutPR = append(rep.ChangesWithoutPR, links[i].Change.Name)
		}
	}
	for _, pr := range sorted {
		if !used[key(pr)] {
			rep.OrphanPRs = append(rep.OrphanPRs, pr)
		}
	}
	return links, rep
}

// primaryMark — PR описан как часть именно этого change'а: «Change `имя`»
// или «change'а `имя`» (клиентская и e2e-части).
func primaryMark(body, name string) bool {
	lower := strings.ToLower(body)
	tag := "`" + strings.ToLower(name) + "`"
	for _, prefix := range []string{"change " + tag, "change'а " + tag, "change’а " + tag} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}

// anyMention — имя change'а хоть где-то упомянуто в теле.
func anyMention(body, name string) bool {
	return strings.Contains(body, "`"+name+"`")
}

// BuildManifest — манифест из привязанных change'ей и PR-сирот. Задачи
// получают PR репозитория своей секции; Epic завершён датой последнего
// merge, иначе датой архивации. Сирота становится Epic'ом с одной задачей.
func BuildManifest(links []Link, orphans []PullRequest, mainRepo string) Manifest {
	m := Manifest{Source: "openspec"}
	for _, l := range links {
		c := l.Change
		e := Epic{Key: c.Key, Title: epicTitle(c, l.PRs, mainRepo), Goal: c.Goal, CreatedAt: c.Date, DoneAt: c.Date}
		for _, pr := range l.PRs {
			if pr.MergedAt.After(e.DoneAt) {
				e.DoneAt = pr.MergedAt
			}
		}
		for _, t := range c.Tasks {
			task := Task{Title: t.Title, Done: t.Done, Repo: t.Section}
			if pr, ok := l.PRs[t.Section]; ok {
				task.PRURL = pr.URL
			}
			e.Tasks = append(e.Tasks, task)
		}
		m.Epics = append(m.Epics, e)
	}
	for _, pr := range orphans {
		m.Epics = append(m.Epics, Epic{
			Key: fmt.Sprintf("pr-%s-%d", pr.Repo, pr.Number), Title: pr.Title,
			Goal:      "PR без change'а в архиве OpenSpec: " + pr.URL,
			CreatedAt: pr.MergedAt, DoneAt: pr.MergedAt,
			Tasks: []Task{{Title: pr.Title, Done: true, Repo: pr.Repo, PRURL: pr.URL}},
		})
	}
	return m
}

// epicTitle — заголовок Epic'а: из proposal.md, а если там только имя
// change'а (нет H1), то заголовок PR основного репозитория или любого
// другого, он человекочитаемее.
func epicTitle(c Change, prs map[string]PullRequest, mainRepo string) string {
	if c.Title != "" && c.Title != c.Name {
		return c.Title
	}
	if pr, ok := prs[mainRepo]; ok && pr.Title != "" {
		return pr.Title
	}
	repos := make([]string, 0, len(prs))
	for r := range prs {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, r := range repos {
		if prs[r].Title != "" {
			return prs[r].Title
		}
	}
	return c.Name
}
