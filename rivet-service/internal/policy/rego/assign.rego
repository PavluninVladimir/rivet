# Стандартная политика Rivet: можно ли назначать задачи проекта в этот
# проход планировщика. Бюджет 0 означает «без ограничения».
package rivet.assign

default allow := false

default reason := ""

allow if {
	not installation_exceeded
	not project_exceeded
	not epic_exceeded
}

installation_exceeded if {
	input.installation.budget > 0
	input.installation.used >= input.installation.budget
}

project_exceeded if {
	input.project.budget > 0
	input.project.used >= input.project.budget
}

epic_exceeded if {
	input.epic.budget > 0
	input.epic.used >= input.epic.budget
}

reason := "installation" if installation_exceeded

reason := "project" if {
	not installation_exceeded
	project_exceeded
}

reason := "epic" if {
	not installation_exceeded
	not project_exceeded
	epic_exceeded
}
