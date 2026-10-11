# Стандартная политика Rivet: мутации людей, управляемые политикой.
# Права выдаёт код (роли, членство) — движок может только запретить, поэтому
# стандартная политика разрешает всё, кроме записи политики автоматикой.
package rivet.mutation

default allow := true

default reason := ""

allow := false if {
	input.action == "policy.write"
	input.actor.kind != "user"
}

reason := "automation_cannot_write_policy" if {
	input.action == "policy.write"
	input.actor.kind != "user"
}
