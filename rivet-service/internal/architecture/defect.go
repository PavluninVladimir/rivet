package architecture

import (
	"fmt"
	"sort"
)

// Severity — важность дефекта. Описание корректно, если в нём нет ошибок.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Defect — дефект описания. Rule — стабильный код правила: его читают
// агенты и тесты. Message — для человека. Location — идентификатор
// записи (элемента, разрыва, узла BPMN) или JSON Pointer в документе.
type Defect struct {
	Severity Severity `json:"severity"`
	Rule     string   `json:"rule"`
	File     string   `json:"file"`
	Location string   `json:"location,omitempty"`
	Message  string   `json:"message"`
}

func (d Defect) String() string {
	where := d.File
	if d.Location != "" {
		where += " " + d.Location
	}
	return fmt.Sprintf("%s %s [%s] %s", d.Severity, where, d.Rule, d.Message)
}

// Defects — результат проверки, отсортированный по файлу, месту и правилу.
type Defects []Defect

// HasErrors сообщает, есть ли дефекты с важностью error.
func (ds Defects) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Count возвращает число ошибок и предупреждений.
func (ds Defects) Count() (errs, warns int) {
	for _, d := range ds {
		if d.Severity == SeverityError {
			errs++
		} else {
			warns++
		}
	}
	return errs, warns
}

// reporter копит дефекты всех проходов. Одинаковые дефекты схлопываются:
// проверка целевой модели изменения повторяет дефекты нетронутой части
// текущей модели, и показывать их дважды незачем.
type reporter struct {
	seen    map[Defect]bool
	defects []Defect
}

func newReporter() *reporter {
	return &reporter{seen: map[Defect]bool{}}
}

func (r *reporter) add(d Defect) {
	if r.seen[d] {
		return
	}
	r.seen[d] = true
	r.defects = append(r.defects, d)
}

func (r *reporter) errorf(rule, file, loc, format string, args ...any) {
	r.add(Defect{SeverityError, rule, file, loc, fmt.Sprintf(format, args...)})
}

func (r *reporter) warnf(rule, file, loc, format string, args ...any) {
	r.add(Defect{SeverityWarning, rule, file, loc, fmt.Sprintf(format, args...)})
}

func (r *reporter) merge(ds Defects) {
	for _, d := range ds {
		r.add(d)
	}
}

// result возвращает дефекты в стабильном порядке: одинаковый вход даёт
// побайтно одинаковый вывод.
func (r *reporter) result() Defects {
	out := make(Defects, len(r.defects))
	copy(out, r.defects)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Location != b.Location {
			return a.Location < b.Location
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		return a.Severity < b.Severity
	})
	return out
}
