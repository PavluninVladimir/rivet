# Стандартная политика Rivet: авто-merge задачи после пройденного review.
# Значения пресетов и факты PR приходят в input; сопоставление путей с
# шаблонами делает control plane (одна реализация glob на систему), сюда
# приезжают уже отобранные списки.
package rivet.merge

default allow := false

# Причина отказа: пресет выключен, список файлов PR неизвестен (fail-closed),
# PR меняет файлы политики или защищённые пути.
default reason := "auto_merge_off"

allow if {
	input.presets.auto_merge
	not input.files_unknown
	count(input.policy_files) == 0
	count(input.protected) == 0
}

reason := "" if allow

reason := "files_unknown" if {
	input.presets.auto_merge
	input.files_unknown
}

reason := "policy_file" if {
	input.presets.auto_merge
	not input.files_unknown
	count(input.policy_files) > 0
}

reason := "human_review_path" if {
	input.presets.auto_merge
	not input.files_unknown
	count(input.policy_files) == 0
	count(input.protected) > 0
}
