package architecture

import (
	"strings"
	"testing"
)

// Знак процента и угловые скобки в именах и описаниях не доходят до схем
// C4, flowchart, sequence и ERD: %%{init}%% из имени элемента иначе стал
// бы директивой Mermaid, а <img src> — HTML-подписью, за которой браузер
// сходит на внешний адрес (решения по контракту, ревью консоли круги 1 и 2).
func TestMermaidPercentEscaped(t *testing.T) {
	evil := `Сервис %%{init: {"securityLevel":"loose"}}%% 100% <img src=https://evil.example/x.png>`
	for name, got := range map[string]string{
		"c4q":      c4q(evil),
		"c4Title":  c4Title(evil),
		"seqText":  seqText(evil),
		"erdText":  erdText(evil, "x"),
		"mlabel":   mlabel(evil),
		"c4q-desc": c4q("загрузка 50% CPU <b>"),
	} {
		if strings.ContainsAny(got, "%<>") {
			t.Errorf("%s: процент или угловая скобка в тексте схемы: %s", name, got)
		}
	}
}
