// Package policy — политики конвейера пресетами (change add-policy-presets,
// спеки backend/access-policy, orchestration, task-pipeline): документ
// пресетов установки, переопределения проекта, вычисление действующей
// политики, канонический хэш версии и сопоставление путей PR.
//
// Пакет чистый: ни базы, ни движка. Store читает и пишет версии, движок
// и API зовут Effective; OPA-режимы из спеки лягут за тот же шов позже.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"sigs.k8s.io/yaml"
)

// Presets — полный документ политики (область «установка» и действующая
// политика проекта). DailyTokenBudget nil — без ограничения.
type Presets struct {
	AutoMerge        bool     `json:"auto_merge"`
	HumanReviewPaths []string `json:"human_review_paths"`
	AttemptLimit     int      `json:"attempt_limit"`
	ReviewLimit      int      `json:"review_limit"`
	DailyTokenBudget *int64   `json:"daily_token_budget"`
	AutoPublish      bool     `json:"auto_publish"`
	// Process — процесс задачи (спека backend/process). nil — процесс по
	// умолчанию; omitempty, чтобы хэши версий без процесса не менялись.
	Process *Process `json:"process,omitempty"`
}

// Overrides — документ проекта: каждое поле nil означает «наследуется от
// установки». DailyTokenBudget: nil — наследуется, 0 — переопределено на
// «без ограничения», >0 — бюджет проекта.
type Overrides struct {
	AutoMerge        *bool     `json:"auto_merge"`
	HumanReviewPaths *[]string `json:"human_review_paths"`
	AttemptLimit     *int      `json:"attempt_limit"`
	ReviewLimit      *int      `json:"review_limit"`
	DailyTokenBudget *int64    `json:"daily_token_budget"`
	AutoPublish      *bool     `json:"auto_publish"`
	// Process — процесс проекта целиком (не поэлементно); nil — процесс
	// установки.
	Process *Process `json:"process,omitempty"`
}

// Defaults — значения по умолчанию, совпадающие с поведением до появления
// политик: авто-merge выключен, лимит 3, бюджета нет, автопубликация разрешена.
func Defaults() Presets {
	return Presets{
		AutoMerge: false, HumanReviewPaths: []string{},
		AttemptLimit: 3, ReviewLimit: 3,
		DailyTokenBudget: nil, AutoPublish: true,
	}
}

// PolicyDir — каталог политики как кода в репозитории проекта. Защищён
// всегда: PR, меняющий файлы в нём, не проходит авто-merge (метаправило
// «Защита от самоослабления»), пресетами не отключается.
const PolicyDir = ".rivet/"

// PolicyFile — файл политики проекта в репозитории (git-провайдер).
const PolicyFile = PolicyDir + "policy.yaml"

// Источники политики проекта (спека access-policy «Хранение политик»).
const (
	SourceStore = "store"
	SourceGit   = "git"
)

// ParseOverrides разбирает файл политики проекта: YAML или JSON (YAML —
// его надмножество). Набор полей и валидация те же, что у правки из
// консоли: два источника не должны расходиться в том, что вообще можно
// задать.
func ParseOverrides(raw []byte) (Overrides, error) {
	var o Overrides
	// Строгий разбор: опечатка в имени ключа (auto_merges вместо
	// auto_merge) должна быть видимой ошибкой, а не молча проигнорированным
	// полем — иначе автор думает, что политика изменилась, а она нет.
	if err := yaml.UnmarshalStrict(raw, &o); err != nil {
		return Overrides{}, fmt.Errorf("%w: файл политики не разбирается: %v", ErrInvalid, err)
	}
	if err := o.Validate(); err != nil {
		return Overrides{}, err
	}
	return o, nil
}

// ErrInvalid — документ не проходит валидацию.
var ErrInvalid = errors.New("некорректная политика")

// Normalize приводит документ к каноническому виду: nil-список путей
// становится пустым, бюджет 0 — отсутствием ограничения.
func (p Presets) Normalize() Presets {
	if p.HumanReviewPaths == nil {
		p.HumanReviewPaths = []string{}
	}
	if p.DailyTokenBudget != nil && *p.DailyTokenBudget == 0 {
		p.DailyTokenBudget = nil
	}
	return p
}

// Validate проверяет пресеты: лимиты не меньше 1, бюджет не отрицательный,
// шаблоны путей корректны и не несут управляющих символов (пути уезжают в
// промпт агента — перевод строки превратил бы путь в отдельную инструкцию).
func (p Presets) Validate() error {
	if p.AttemptLimit < 1 {
		return fmt.Errorf("%w: лимит попыток должен быть не меньше 1", ErrInvalid)
	}
	if p.ReviewLimit < 1 {
		return fmt.Errorf("%w: лимит отказов review должен быть не меньше 1", ErrInvalid)
	}
	if p.DailyTokenBudget != nil && *p.DailyTokenBudget < 0 {
		return fmt.Errorf("%w: дневной бюджет токенов не может быть отрицательным", ErrInvalid)
	}
	if err := validatePatterns(p.HumanReviewPaths); err != nil {
		return err
	}
	if p.Process != nil {
		return p.Process.Validate()
	}
	return nil
}

// EffectiveProcess — разрешённый процесс действующей политики: документ
// политики или процесс по умолчанию, с лимитами из пресетов.
func (p Presets) EffectiveProcess() Resolved {
	doc := DefaultProcess()
	if p.Process != nil {
		doc = *p.Process
	}
	return Resolve(doc, p)
}

// Validate проверяет переопределения: те же правила для заданных полей.
func (o Overrides) Validate() error {
	if o.AttemptLimit != nil && *o.AttemptLimit < 1 {
		return fmt.Errorf("%w: лимит попыток должен быть не меньше 1", ErrInvalid)
	}
	if o.ReviewLimit != nil && *o.ReviewLimit < 1 {
		return fmt.Errorf("%w: лимит отказов review должен быть не меньше 1", ErrInvalid)
	}
	if o.DailyTokenBudget != nil && *o.DailyTokenBudget < 0 {
		return fmt.Errorf("%w: дневной бюджет токенов не может быть отрицательным", ErrInvalid)
	}
	if o.HumanReviewPaths != nil {
		if err := validatePatterns(*o.HumanReviewPaths); err != nil {
			return err
		}
	}
	if o.Process != nil {
		return o.Process.Validate()
	}
	return nil
}

func validatePatterns(patterns []string) error {
	for _, raw := range patterns {
		// Управляющие символы ищем в исходном значении: хранится оно как
		// есть, а TrimSpace убрал бы перевод строки только из проверки.
		if strings.ContainsFunc(raw, promptBreaking) {
			return fmt.Errorf("%w: шаблон пути %q содержит управляющий символ", ErrInvalid, raw)
		}
		pat := strings.TrimSpace(raw)
		if pat == "" {
			return fmt.Errorf("%w: пустой шаблон пути", ErrInvalid)
		}
		if strings.HasPrefix(pat, "/") {
			return fmt.Errorf("%w: шаблон %q должен быть от корня репозитория без ведущего «/»", ErrInvalid, pat)
		}
		if !doublestar.ValidatePattern(pat) {
			return fmt.Errorf("%w: некорректный шаблон пути %q", ErrInvalid, pat)
		}
	}
	return nil
}

// promptBreaking — символ, который в промпте агента начал бы новую строку
// или сообщение: пути политики уезжают туда как есть (доставка политики
// runner'у), и такой шаблон стал бы отдельной инструкцией.
func promptBreaking(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}

// Effective — действующая политика проекта: пресеты установки, перекрытые
// ненулевыми полями проекта.
func Effective(installation Presets, o Overrides) Presets {
	eff := installation.Normalize()
	if o.AutoMerge != nil {
		eff.AutoMerge = *o.AutoMerge
	}
	if o.HumanReviewPaths != nil {
		eff.HumanReviewPaths = append([]string{}, (*o.HumanReviewPaths)...)
	}
	if o.AttemptLimit != nil {
		eff.AttemptLimit = *o.AttemptLimit
	}
	if o.ReviewLimit != nil {
		eff.ReviewLimit = *o.ReviewLimit
	}
	if o.DailyTokenBudget != nil {
		if *o.DailyTokenBudget == 0 {
			eff.DailyTokenBudget = nil
		} else {
			v := *o.DailyTokenBudget
			eff.DailyTokenBudget = &v
		}
	}
	if o.AutoPublish != nil {
		eff.AutoPublish = *o.AutoPublish
	}
	if o.Process != nil {
		cp := *o.Process
		cp.Steps = append([]Step{}, o.Process.Steps...)
		eff.Process = &cp
	}
	return eff
}

// Hash — sha256 канонического JSON документа. Канонический вид — порядок
// полей структуры, списки как есть, nil как null.
func Hash(doc any) string {
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// MatchAny — попадает ли путь (от корня репозитория, без ведущего «/») под
// один из шаблонов glob с «**».
func MatchAny(patterns []string, path string) bool {
	path = strings.TrimPrefix(path, "/")
	for _, pat := range patterns {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if ok, err := doublestar.Match(pat, path); err == nil && ok {
			return true
		}
		// Шаблон-каталог («infra/») защищает всё внутри.
		if strings.HasSuffix(pat, "/") && strings.HasPrefix(path, pat) {
			return true
		}
	}
	return false
}

// IsPolicyPath — путь лежит в каталоге политики (.rivet/).
func IsPolicyPath(path string) bool {
	path = strings.TrimPrefix(path, "/")
	return strings.HasPrefix(path, PolicyDir)
}

// PathsFromDiff разбирает пути изменённых файлов из unified diff: строки
// «diff --git a/… b/…», обе стороны (переименования). Пути уникальны, в
// порядке появления. Пустой результат при непустом diff трактуется
// вызывающим как «список получить не удалось» (fail-closed).
func PathsFromDiff(diff string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || p == "/dev/null" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, line := range strings.Split(diff, "\n") {
		rest, ok := strings.CutPrefix(line, "diff --git ")
		if !ok {
			continue
		}
		a, b := splitDiffHeader(strings.TrimRight(rest, "\r"))
		add(strings.TrimPrefix(a, "a/"))
		add(strings.TrimPrefix(b, "b/"))
	}
	return out
}

// splitDiffHeader делит «a/x b/y» на две стороны. Пути с пробелами git
// берёт в кавычки; без кавычек предпочитается разбиение, где стороны
// совпадают (обычный случай без переименования), иначе первое « b/».
func splitDiffHeader(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		a, rest := unquote(s)
		rest = strings.TrimSpace(rest)
		if strings.HasPrefix(rest, `"`) {
			b, _ := unquote(rest)
			return a, b
		}
		return a, rest
	}
	idx := -1
	for i := 0; i+3 <= len(s); i++ {
		if s[i:i+3] == " b/" {
			left, right := s[:i], s[i+1:]
			if strings.TrimPrefix(left, "a/") == strings.TrimPrefix(right, "b/") {
				return left, right
			}
			if idx < 0 {
				idx = i
			}
		}
	}
	if idx < 0 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// unquote снимает C-кавычки git с начала строки; возвращает путь и хвост.
func unquote(s string) (string, string) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '"' {
			return b.String(), s[i+1:]
		}
		if c == '\\' && i+1 < len(s) {
			i++
			// Октальная запись байта (\320\277 — так git пишет не-ASCII).
			if i+3 <= len(s) && isOctal(s[i]) && isOctal(s[i+1]) && isOctal(s[i+2]) {
				b.WriteByte((s[i]-'0')<<6 | (s[i+1]-'0')<<3 | (s[i+2] - '0'))
				i += 3
				continue
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
			}
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), ""
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
