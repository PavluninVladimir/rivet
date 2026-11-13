package discovery

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Лимиты результата синтеза (спека «Проверка результата анализа»).
const (
	MaxEpics          = 12
	MaxNeeds          = 60
	MaxPersonas       = 8
	MaxQuotesPerNeed  = 5
	MaxTitleRunes     = 120
	MaxGoalRunes      = 4000
	MinGoalRunes      = 40
	MaxRationaleRunes = 2000
)

// Приоритеты и размеры Epic'а бэклога.
const (
	PriorityMust   = "must"
	PriorityShould = "should"
	PriorityCould  = "could"
)

var (
	modelKeyRe = regexp.MustCompile(`^e[0-9]{1,3}$`)
	humanKeyRe = regexp.MustCompile(`^h[0-9]{1,4}$`)
	itemIDRe   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// StatusArchived — статус Epic'а, с которым нельзя связать предложение.
const StatusArchived = "archived"

// InterviewInput — интервью версии исследования: нормализованный текст и
// принятый конспект (после CheckSummary и MergeSummaries).
type InterviewInput struct {
	ID string
	// Kind — вид материала: пусто или MaterialInterview у интервью,
	// MaterialFeedback у отзыва пользователя.
	Kind           string
	Title          string
	RespondentRole string
	Text           string
	Summary        Summary
}

// ExistingEpic — Epic проекта, который видит Аналитик и с которым можно
// связать предложение.
type ExistingEpic struct {
	ID     string
	Title  string
	Goal   string
	Status string
	// Needs — названия связанных потребностей прошлых версий.
	Needs []string
}

// Persona — персона результата.
type Persona struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Interviews  []string `json:"interviews"`
}

// QuoteRef — подтверждённая цитата потребности (форма Quote в
// api-contract): интервью, его название и роль респондента, строка
// начала и точный фрагмент исходного текста.
type QuoteRef struct {
	Interview      string `json:"interview"`
	InterviewTitle string `json:"interview_title"`
	RespondentRole string `json:"respondent_role"`
	// Kind — feedback у цитаты из отзыва пользователя, у интервью пусто
	// (add-user-feedback): консоль подписывает источник цитаты.
	Kind string `json:"kind,omitempty"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Need — принятая потребность с подтверждёнными цитатами.
type Need struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Kind       string     `json:"kind"`
	Persona    string     `json:"persona"`
	Importance string     `json:"importance"`
	Evidence   []QuoteRef `json:"evidence"`
}

// BacklogEpic — Epic бэклога: предложение модели или правка человека.
// DependsOn — ключи бэклога или идентификаторы существующих Epic'ов.
type BacklogEpic struct {
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	Goal         string   `json:"goal"`
	Rationale    string   `json:"rationale,omitempty"`
	Needs        []string `json:"needs"`
	Priority     string   `json:"priority"`
	Size         string   `json:"size"`
	DependsOn    []string `json:"depends_on"`
	ExistingEpic *string  `json:"existing_epic"`
}

// Synthesis — принятый результат синтеза.
type Synthesis struct {
	Decision

	Personas []Persona     `json:"personas"`
	Needs    []Need        `json:"needs"`
	Epics    []BacklogEpic `json:"epics"`
}

// rawEvidence — цитата потребности в ответе модели.
type rawEvidence struct {
	Interview string `json:"interview"`
	Quote     string `json:"quote"`
	Line      int    `json:"line,omitempty"`
}

type rawNeed struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Kind       string        `json:"kind"`
	Persona    string        `json:"persona"`
	Importance string        `json:"importance"`
	Evidence   []rawEvidence `json:"evidence"`
}

type rawSynthesis struct {
	Decision

	Personas []Persona     `json:"personas"`
	Needs    []rawNeed     `json:"needs"`
	Epics    []BacklogEpic `json:"epics"`
}

// SynthesisCheck — итог проверки ответа второго этапа.
type SynthesisCheck struct {
	Result  Synthesis
	Dropped []Dropped
	Defects []string
	// Retry — повторить запрос с перечнем дефектов: ответ не разобрался,
	// есть цикл, нарушены лимиты или отброшено больше половины потребностей.
	Retry bool
	// Usable — результат можно принять (после повтора — даже при большом
	// числе отброшенного). false: не разобрался, цикл, нарушены лимиты.
	Usable bool
}

// CheckSynthesis разбирает и проверяет ответ синтеза: цитаты по тексту
// интервью, ссылки, поля и уникальность Epic'ов, связь с существующими
// Epic'ами, зависимости и лимиты. Порядок проверок — design, решение 3.
func CheckSynthesis(interviews []InterviewInput, existing []ExistingEpic, raw string) SynthesisCheck {
	in, warning, err := decodeAnswer[rawSynthesis](raw, "epics")
	if err != nil {
		return SynthesisCheck{Defects: []string{err.Error()}, Retry: true}
	}
	var out SynthesisCheck
	if warning != "" {
		out.Defects = append(out.Defects, warning)
	}
	// isFatal — результат принимать нельзя: схема, цикл, лимиты.
	isFatal := false
	fatal := func(format string, args ...any) {
		out.Defects = append(out.Defects, fmt.Sprintf(format, args...))
		out.Retry = true
		isFatal = true
	}
	byInterview := map[string]InterviewInput{}
	for _, iv := range interviews {
		byInterview[iv.ID] = iv
	}
	existingBy := map[string]ExistingEpic{}
	for _, e := range existing {
		existingBy[e.ID] = e
	}

	out.Result.Decision = in.Decision
	if in.Personas == nil || in.Needs == nil || in.Epics == nil {
		fatal("personas, needs, epics: обязательны массивы, не null")
	}
	for _, defect := range checkDecision(&out.Result.Decision, in.Epics, byInterview) {
		fatal("%s", defect)
	}
	// Персоны: идентификаторы уникальны, ссылки на интервью разрешаются.
	personaIDs := map[string]bool{}
	for i, p := range in.Personas {
		// Имена и названия идут дальше в задания (декомпозиция): в одну
		// строку, чтобы перевод строки не стал отдельной инструкцией.
		p.ID, p.Name, p.Description = strings.TrimSpace(p.ID), oneLine(p.Name), strings.TrimSpace(p.Description)
		if !itemIDRe.MatchString(p.ID) || p.Name == "" {
			fatal("персона %d: нужны идентификатор (латиница, цифры) и имя", i+1)
			continue
		}
		if personaIDs[p.ID] {
			fatal("персона %s повторяется", p.ID)
			continue
		}
		personaIDs[p.ID] = true
		var ivs []string
		for _, iid := range p.Interviews {
			if _, ok := byInterview[iid]; !ok {
				out.Dropped = append(out.Dropped, Dropped{Kind: "persona", Ref: p.ID + " → " + iid, Reason: ReasonUnknownInterview})
				continue
			}
			ivs = append(ivs, iid)
		}
		p.Interviews = nonNil(ivs)
		out.Result.Personas = append(out.Result.Personas, p)
	}

	// Потребности: поля, персона, цитаты по тексту своего интервью.
	needIDs := map[string]bool{}
	accepted := map[string]bool{}
	for i, n := range in.Needs {
		n.ID, n.Title = strings.TrimSpace(n.ID), oneLine(n.Title)
		ref := n.ID
		if ref == "" {
			ref = fmt.Sprintf("потребность %d", i+1)
		}
		switch {
		case !itemIDRe.MatchString(n.ID):
			fatal("%s: нужен идентификатор (латиница, цифры)", ref)
			continue
		case needIDs[n.ID]:
			fatal("потребность %s повторяется", n.ID)
			continue
		}
		needIDs[n.ID] = true
		reason := ""
		switch {
		case n.Title == "":
			reason = ReasonEmptyText
		case n.Kind != KindPain && n.Kind != KindNeed && n.Kind != KindWish:
			reason = ReasonBadKind + ": " + n.Kind
		case !validImportance(n.Importance):
			reason = ReasonBadImportance + ": " + n.Importance
		case !personaIDs[n.Persona]:
			reason = ReasonUnknownPersona + ": " + n.Persona
		}
		if reason != "" {
			out.Dropped = append(out.Dropped, Dropped{Kind: "need", Ref: ref + ": " + short(n.Title), Reason: reason})
			continue
		}
		// Цитаты собираются по интервью: повтор — одно начало в одном тексте,
		// из повторов остаётся более длинная.
		type found struct {
			iv  InterviewInput
			loc Located
		}
		var hits []found
		for _, e := range n.Evidence {
			qref := n.ID + ": «" + short(e.Quote) + "»"
			iv, ok := byInterview[e.Interview]
			if !ok {
				out.Dropped = append(out.Dropped, Dropped{Kind: "quote", Ref: qref, Reason: ReasonUnknownInterview + ": " + e.Interview})
				continue
			}
			loc, why, ok := locate(iv.Text, e.Quote, hintFor(iv, e))
			if !ok {
				why = materialReason(why, iv.Kind)
				where := "интервью"
				if iv.Kind == MaterialFeedback {
					where = "отзыве"
				}
				out.Dropped = append(out.Dropped, Dropped{Kind: "quote", Ref: qref, Reason: why})
				if why == ReasonQuoteTooShort {
					out.Defects = append(out.Defects, fmt.Sprintf("потребность %s: цитата «%s» из %s короче %d символов", n.ID, short(e.Quote), e.Interview, MinQuoteRunes))
				} else {
					out.Defects = append(out.Defects, fmt.Sprintf("потребность %s: цитата «%s» не найдена дословно в %s %s", n.ID, short(e.Quote), where, e.Interview))
				}
				continue
			}
			dup := false
			for j, h := range hits {
				if h.iv.ID != iv.ID || h.loc.Start != loc.Start {
					continue
				}
				dup = true
				loser := loc
				if loc.End > h.loc.End {
					hits[j].loc, loser = loc, h.loc
				}
				out.Dropped = append(out.Dropped, Dropped{Kind: "quote", Ref: n.ID + ": «" + short(loser.Text) + "»", Reason: ReasonQuoteDuplicate})
				break
			}
			if !dup {
				hits = append(hits, found{iv: iv, loc: loc})
			}
		}
		var ev []QuoteRef
		for _, h := range hits {
			if len(ev) == MaxQuotesPerNeed {
				out.Dropped = append(out.Dropped, Dropped{Kind: "quote", Ref: n.ID + ": «" + short(h.loc.Text) + "»", Reason: ReasonQuoteLimit})
				continue
			}
			ref := QuoteRef{Interview: h.iv.ID, InterviewTitle: h.iv.Title, RespondentRole: h.iv.RespondentRole, Line: h.loc.Line, Text: h.loc.Text}
			if h.iv.Kind == MaterialFeedback {
				ref.Kind = MaterialFeedback
			}
			ev = append(ev, ref)
		}
		if len(ev) == 0 {
			out.Dropped = append(out.Dropped, Dropped{Kind: "need", Ref: ref + ": " + short(n.Title), Reason: ReasonNoQuotes})
			continue
		}
		accepted[n.ID] = true
		out.Result.Needs = append(out.Result.Needs, Need{ID: n.ID, Title: n.Title, Kind: n.Kind, Persona: n.Persona, Importance: n.Importance, Evidence: ev})
	}
	droppedNeeds := len(in.Needs) - len(out.Result.Needs)
	if len(in.Needs) > 0 && droppedNeeds*2 > len(in.Needs) {
		out.Defects = append(out.Defects, fmt.Sprintf("отброшено %d из %d потребностей: цитаты должны дословно совпадать с текстом интервью", droppedNeeds, len(in.Needs)))
		out.Retry = true
	}

	// Epic'и: ключи, поля, потребности, уникальность названий, связь с
	// существующими Epic'ами.
	keys := map[string]bool{}
	titles := map[string]string{}
	for i, e := range in.Epics {
		e.Key = strings.TrimSpace(e.Key)
		ref := e.Key
		if ref == "" {
			ref = fmt.Sprintf("Epic %d", i+1)
		}
		switch {
		case !modelKeyRe.MatchString(e.Key):
			fatal("%s: ключ Epic'а должен быть вида e1, e2, …", ref)
			continue
		case keys[e.Key]:
			fatal("ключ Epic'а %s повторяется", e.Key)
			continue
		}
		keys[e.Key] = true
		e.Title, e.Goal, e.Rationale = oneLine(e.Title), strings.TrimSpace(e.Goal), strings.TrimSpace(e.Rationale)
		if reason := epicFieldReason(e); reason != "" {
			out.Dropped = append(out.Dropped, Dropped{Kind: "epic", Ref: ref + ": " + short(e.Title), Reason: reason})
			continue
		}
		var needs []string
		seenNeed := map[string]bool{}
		for _, nid := range e.Needs {
			if seenNeed[nid] {
				continue
			}
			seenNeed[nid] = true
			if !accepted[nid] {
				out.Dropped = append(out.Dropped, Dropped{Kind: "need", Ref: e.Key + " → " + nid, Reason: ReasonUnknownNeed})
				continue
			}
			needs = append(needs, nid)
		}
		if len(needs) == 0 {
			out.Dropped = append(out.Dropped, Dropped{Kind: "epic", Ref: ref + ": " + short(e.Title), Reason: ReasonNoNeeds})
			continue
		}
		e.Needs = needs
		norm := NormalizeTitle(e.Title)
		if prev, dup := titles[norm]; dup {
			out.Dropped = append(out.Dropped, Dropped{Kind: "epic", Ref: ref + ": " + short(e.Title), Reason: ReasonDuplicateTitle + " (" + prev + ")"})
			continue
		}
		titles[norm] = e.Key
		out.Result.Epics = append(out.Result.Epics, e)
	}

	// Связь с Epic'ами проекта до разбора зависимостей: зависимость на
	// связанный Epic — это зависимость на само предложение.
	linked := map[string]string{} // id Epic'а проекта → ключ предложения
	for i := range out.Result.Epics {
		e := &out.Result.Epics[i]
		if e.ExistingEpic == nil {
			continue
		}
		id := *e.ExistingEpic
		ex, ok := existingBy[id]
		switch {
		case !ok || ex.Status == StatusArchived:
			out.Dropped = append(out.Dropped, Dropped{Kind: "epic", Ref: e.Key + " → " + id, Reason: ReasonUnknownEpic})
			e.ExistingEpic = nil
		case linked[id] != "":
			out.Dropped = append(out.Dropped, Dropped{Kind: "epic", Ref: e.Key + " → " + id, Reason: ReasonDoubleLink + " (" + linked[id] + ")"})
			e.ExistingEpic = nil
		default:
			linked[id] = e.Key
		}
	}
	out.Result.Epics = LinkExisting(out.Result.Epics, existing)

	// Зависимости: только на принятые Epic'и бэклога или существующие
	// Epic'и проекта (не в архиве), без повторов. Зависимость на себя
	// остаётся: это цикл, его ловит проверка ниже. Зависимость на Epic
	// проекта, связанный с этим же предложением, снимается.
	finalKeys := map[string]bool{}
	for _, e := range out.Result.Epics {
		finalKeys[e.Key] = true
	}
	for i := range out.Result.Epics {
		e := &out.Result.Epics[i]
		var deps []string
		seen := map[string]bool{}
		for _, d := range e.DependsOn {
			d = strings.TrimSpace(d)
			if seen[d] {
				continue
			}
			if e.ExistingEpic != nil && d == *e.ExistingEpic {
				out.Dropped = append(out.Dropped, Dropped{Kind: "dependency", Ref: e.Key + " → " + d, Reason: ReasonSelfLinkedDep})
				continue
			}
			ex, isExisting := existingBy[d]
			if !finalKeys[d] && (!isExisting || ex.Status == StatusArchived) {
				out.Dropped = append(out.Dropped, Dropped{Kind: "dependency", Ref: e.Key + " → " + d, Reason: ReasonUnknownDependency})
				continue
			}
			seen[d] = true
			deps = append(deps, d)
		}
		e.DependsOn = nonNil(deps)
	}

	if cycle := FindCycle(epicGraph(out.Result.Epics)); cycle != nil {
		fatal("зависимости Epic'ов образуют цикл: %s", strings.Join(cycle, " → "))
	}
	switch n := len(out.Result.Epics); {
	case n == 0 && in.Outcome == OutcomeBacklogReady:
		fatal("нет ни одного принятого Epic'а: каждый Epic должен опираться на принятые потребности")
	case n > MaxEpics:
		fatal("Epic'ов %d, допустимо не больше %d", n, MaxEpics)
	}
	if n := len(out.Result.Needs); n > MaxNeeds {
		fatal("потребностей %d, допустимо не больше %d", n, MaxNeeds)
	}
	if n := len(out.Result.Personas); n > MaxPersonas {
		fatal("персон %d, допустимо не больше %d", n, MaxPersonas)
	}
	// Персональные данные: название, цель и обоснование Epic'а уходят в
	// задание Тимлиду, задачи и pull request'ы. Дословный фрагмент цитаты
	// там — дефект и повтор с просьбой пересказать своими словами.
	var quotes []string
	for _, n := range out.Result.Needs {
		for _, q := range n.Evidence {
			quotes = append(quotes, q.Text)
		}
	}
	for _, e := range out.Result.Epics {
		for _, f := range []struct{ name, text string }{{"название", e.Title}, {"цель", e.Goal}, {"обоснование", e.Rationale}} {
			if frag, ok := QuoteLeak(f.text, quotes); ok {
				out.Defects = append(out.Defects, fmt.Sprintf("Epic %s: %s содержит дословный фрагмент цитаты интервью «%s»: перескажи своими словами, без цитат, имён и названий компаний", e.Key, f.name, frag))
				out.Retry = true
			}
		}
	}
	out.Usable = !isFatal
	if out.Result.Personas == nil {
		out.Result.Personas = []Persona{}
	}
	if out.Result.Needs == nil {
		out.Result.Needs = []Need{}
	}
	if out.Result.Epics == nil {
		out.Result.Epics = []BacklogEpic{}
	}
	return out
}

// hintFor — подсказка строки для цитаты потребности: строка совпадающей
// подтверждённой цитаты конспекта того же интервью, иначе строка от модели.
func hintFor(iv InterviewInput, e rawEvidence) int {
	q := normalizeQuote(e.Quote)
	for _, f := range iv.Summary.Findings {
		for _, sq := range f.Quotes {
			if normalizeQuote(sq.Text) == q {
				return sq.Line
			}
		}
	}
	return e.Line
}

// epicFieldReason — причина отказа по полям Epic'а. Правила одни для
// ответа модели и правки человека: принятый синтез обязан проходить
// ValidateBacklog, иначе первая же правка бэклога упала бы на чужом поле.
func epicFieldReason(e BacklogEpic) string {
	switch {
	case e.Title == "":
		return "пустое название"
	case utf8.RuneCountInString(e.Title) > MaxTitleRunes:
		return fmt.Sprintf("название длиннее %d символов", MaxTitleRunes)
	case e.Goal == "":
		return "пустая цель"
	case utf8.RuneCountInString(e.Goal) < MinGoalRunes:
		return fmt.Sprintf("цель короче %d символов", MinGoalRunes)
	case utf8.RuneCountInString(e.Goal) > MaxGoalRunes:
		return fmt.Sprintf("цель длиннее %d символов", MaxGoalRunes)
	case utf8.RuneCountInString(e.Rationale) > MaxRationaleRunes:
		return fmt.Sprintf("обоснование длиннее %d символов", MaxRationaleRunes)
	case e.Priority != PriorityMust && e.Priority != PriorityShould && e.Priority != PriorityCould:
		return "приоритет должен быть must, should или could"
	case e.Size != "S" && e.Size != "M" && e.Size != "L":
		return "размер должен быть S, M или L"
	}
	return ""
}

// LinkExisting связывает предложения без existing_epic с существующими
// Epic'ами проекта по нормализованному названию. Epic в архиве не
// связывается: предлагается новый. Один существующий Epic связывается не
// больше чем с одним предложением.
func LinkExisting(epics []BacklogEpic, existing []ExistingEpic) []BacklogEpic {
	taken := map[string]bool{}
	for _, e := range epics {
		if e.ExistingEpic != nil {
			taken[*e.ExistingEpic] = true
		}
	}
	byTitle := map[string]string{}
	for _, ex := range existing {
		if ex.Status == StatusArchived {
			continue
		}
		if _, dup := byTitle[NormalizeTitle(ex.Title)]; !dup {
			byTitle[NormalizeTitle(ex.Title)] = ex.ID
		}
	}
	out := make([]BacklogEpic, len(epics))
	copy(out, epics)
	for i := range out {
		if out[i].ExistingEpic != nil {
			continue
		}
		if id, ok := byTitle[NormalizeTitle(out[i].Title)]; ok && !taken[id] {
			v := id
			out[i].ExistingEpic = &v
			taken[id] = true
		}
	}
	return out
}

// epicGraph — зависимости бэклога как граф по ключам. Зависимость на Epic
// проекта, связанный с предложением бэклога, становится ребром на ключ
// этого предложения: после утверждения это один и тот же Epic, и цикл
// через него настоящий. Прочие Epic'и проекта в граф не входят.
func epicGraph(epics []BacklogEpic) map[string][]string {
	keys := map[string]bool{}
	linked := map[string]string{}
	for _, e := range epics {
		keys[e.Key] = true
		if e.ExistingEpic != nil {
			linked[*e.ExistingEpic] = e.Key
		}
	}
	g := map[string][]string{}
	for _, e := range epics {
		var deps []string
		for _, d := range e.DependsOn {
			switch {
			case keys[d]:
				deps = append(deps, d)
			case linked[d] != "":
				deps = append(deps, linked[d])
			}
		}
		g[e.Key] = deps
	}
	return g
}

// FindCycle ищет цикл в графе зависимостей (узел → его зависимости) и
// возвращает путь цикла с повтором первого узла в конце, либо nil.
// Обход детерминирован: узлы и рёбра по возрастанию. Ссылки на узлы вне
// графа не учитываются. Пригоден и для проверки графа Epic'ов проекта.
func FindCycle(deps map[string][]string) []string {
	nodes := make([]string, 0, len(deps))
	for n := range deps {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var visit func(n string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		next := append([]string{}, deps[n]...)
		sort.Strings(next)
		for _, m := range next {
			if _, ok := deps[m]; !ok {
				continue
			}
			switch color[m] {
			case grey:
				for i := len(stack) - 1; i >= 0; i-- {
					if stack[i] == m {
						cycle = append(append([]string{}, stack[i:]...), m)
						return true
					}
				}
			case white:
				if visit(m) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	for _, n := range nodes {
		if color[n] == white && visit(n) {
			return cycle
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
