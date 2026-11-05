package policy

import "testing"

// Пресет автоописания архитектуры (add-architecture-c4): по умолчанию
// включён, проект может его выключить, а хэш версии без поля не меняется.
func TestAutoDescribeArchitecturePreset(t *testing.T) {
	inst := Defaults()
	if !inst.AutoDescribe() {
		t.Fatal("по умолчанию автоописание включено")
	}
	off := false
	eff := Effective(inst, Overrides{AutoDescribeArchitecture: &off})
	if eff.AutoDescribe() {
		t.Fatal("переопределение проекта выключает автоописание")
	}
	if inst.AutoDescribeArchitecture != nil {
		t.Fatal("установка без поля: значение nil, хэш прежних версий не меняется")
	}
	o, err := ParseOverrides([]byte("auto_describe_architecture: false\n"))
	if err != nil || o.AutoDescribeArchitecture == nil || *o.AutoDescribeArchitecture {
		t.Fatalf("разбор файла политики: %+v %v", o, err)
	}
	if eff2 := Effective(inst, o); eff2.AutoDescribe() {
		t.Fatal("значение из файла политики применяется")
	}
}

// Ответ API показывает действующее значение явно, исходные пресеты не меняются.
func TestPresetsExplicit(t *testing.T) {
	inst := Defaults()
	view := inst.Explicit()
	if view.AutoDescribeArchitecture == nil || !*view.AutoDescribeArchitecture {
		t.Fatalf("явное значение по умолчанию: %+v", view.AutoDescribeArchitecture)
	}
	if inst.AutoDescribeArchitecture != nil {
		t.Fatal("Explicit меняет исходные пресеты")
	}
}
