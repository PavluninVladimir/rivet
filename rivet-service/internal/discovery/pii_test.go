package discovery

import (
	"strings"
	"testing"
)

const longQuote = "клиент просил набрать во вторник после совещания, я набрал в четверг"

// Дословный фрагмент цитаты от LeakWords слов находится независимо от
// регистра, «ё» и знаков препинания. Короткие совпадения не считаются.
func TestQuoteLeak(t *testing.T) {
	quotes := []string{"Вот прямо на прошлой неделе: " + longQuote + ". Он уже купил у конкурента."}
	frag, ok := QuoteLeak("Менеджер пишет: «Клиент просил набрать во вторник после совещания, я набрал в четверг»", quotes)
	if !ok || !strings.HasPrefix(frag, "клиент просил набрать во вторник после совещания") {
		t.Fatalf("перенос цитаты не найден: %q %v", frag, ok)
	}
	if _, ok := QuoteLeak("Напоминание: клиент просил набрать во вторник после совещания", quotes); ok {
		t.Fatal("семь слов подряд ниже порога")
	}
	if _, ok := QuoteLeak("", quotes); ok {
		t.Fatal("пустой текст")
	}
}

// Требование, пересказанное обычными словами, не считается переносом
// цитаты (четыре случая ревью Opus, круг 3).
func TestQuoteLeakFalsePositives(t *testing.T) {
	cases := []struct{ quote, text string }{
		{"Хочу, чтобы система сама напоминала о звонке за день до встречи.", "Напоминание о звонке за день до встречи приходит менеджеру"},
		{"Нам нужно фильтровать заявки по статусу и по дате создания, иначе тонем.", "Список заявок фильтруется по статусу и по дате создания"},
		{"Чтобы был экспорт в Excel и выгрузка в 1С раз в сутки.", "Экспорт в Excel и выгрузка в 1С по расписанию"},
		{"Я бы хотел видеть все просроченные задачи на одном экране.", "Менеджер видит все просроченные задачи на одном экране"},
	}
	for _, c := range cases {
		if frag, ok := QuoteLeak(c.text, []string{c.quote}); ok {
			t.Errorf("ложное срабатывание %q: %q", c.text, frag)
		}
	}
}

// Фрагмент из одних служебных слов не считается переносом.
func TestQuoteLeakStopWords(t *testing.T) {
	q := "и вот я то ли не так и не там и не тогда это всё"
	if _, ok := QuoteLeak("а он говорит: и вот я то ли не так и не там и не тогда", []string{q}); ok {
		t.Fatal("служебные слова не перенос цитаты")
	}
}

// Цель Epic'а с дословной цитатой: дефект и повтор, результат при этом
// годен (после повтора принимается с предупреждением).
func TestCheckSynthesisQuoteInEpic(t *testing.T) {
	in := baseSynthesis()
	in.Needs[0]["evidence"] = []map[string]any{{"interview": "iv-manager", "quote": longQuote}}
	in.Epics[1]["goal"] = "Менеджер говорит: " + longQuote + ". Нужен список звонков на сегодня."
	c := CheckSynthesis(demoInterviews(t), nil, in.raw())
	if !c.Retry || !c.Usable {
		t.Fatalf("цитата в цели: ждали повтор при годном результате: %+v", c)
	}
	if !strings.Contains(strings.Join(c.Defects, "\n"), "Epic e2: цель содержит дословный фрагмент цитаты") {
		t.Fatalf("дефект переноса цитаты: %v", c.Defects)
	}
}

// Цитата в цели строки бэклога — ошибка поля до утверждения.
func TestBacklogQuoteLeak(t *testing.T) {
	e := BacklogEpic{Key: "e1", Title: "Напоминания", Goal: "Нужно потому что " + longQuote + " и потерял сделку."}
	ferr := BacklogQuoteLeak([]BacklogEpic{{Key: "e0", Title: "Другое", Goal: "без цитат"}, e}, []string{longQuote})
	if ferr == nil || ferr.Field != "epics[1].goal" || !strings.Contains(ferr.Msg, "перескажите своими словами") {
		t.Fatalf("цитата в цели строки: %+v", ferr)
	}
	if ferr := BacklogQuoteLeak([]BacklogEpic{{Key: "e0", Title: "Напоминания о звонках", Goal: "Менеджер видит звонки на сегодня"}}, []string{longQuote}); ferr != nil {
		t.Fatalf("пересказ без цитаты: %+v", ferr)
	}
}

// Признаки персональных данных: email, телефон, имя и описание
// респондента из метаданных. Короткая роль признаком не считается.
func TestPIISign(t *testing.T) {
	markers := PIIMarkers([]string{"Кирилл Л., менеджер по продажам ООО «ТехноПривод»", "Менеджер по продажам"})
	for _, c := range []struct {
		text string
		want bool
	}{
		{"Написать на kirill@technoprivod.ru после встречи", true},
		{"Позвонить по +7 (912) 345-67-89", true},
		{"Кирилл Л. видит список звонков", true},
		{"Импорт клиентов ТехноПривод из таблицы", true},
		{"Менеджер по продажам видит воронку сделок", false},
		{"Список заявок фильтруется по статусу и по дате создания", false},
		{"Версия 2.0.1 выходит 12.11", false},
	} {
		if _, ok := PIISign(c.text, markers); ok != c.want {
			t.Errorf("%q: признак %v, ждали %v", c.text, ok, c.want)
		}
	}
}

// Правило о персональных данных в задании синтеза.
func TestSynthesisPromptForbidsPII(t *testing.T) {
	p, _ := SynthesisPrompt(SynthesisTask{Interviews: demoInterviews(t)})
	if !strings.Contains(p, "без цитат, имён людей, названий компаний, контактов") {
		t.Fatal("в задании синтеза нет запрета на персональные данные в Epic'ах")
	}
}

// Нулевой символ в правке бэклога — ошибка поля.
func TestValidateBacklogNUL(t *testing.T) {
	e := BacklogEpic{Key: "h1", Title: "Компании\x00контакты", Goal: strings.Repeat("ц", 50), Priority: "must", Size: "S"}
	ferr := ValidateBacklog([]BacklogEpic{e}, nil, nil)
	if ferr == nil || ferr.Field != "epics[0].title" {
		t.Fatalf("NUL в названии: %+v", ferr)
	}
}
