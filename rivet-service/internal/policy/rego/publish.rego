# Стандартная политика Rivet: автоматическая публикация окружения после
# merge задачи.
package rivet.publish

default allow := false

default reason := "auto_publish_off"

allow if input.presets.auto_publish

reason := "" if allow
