package runner

import "strings"

// Секреты runner'а (review add-native-agent, P1-2): токен регистрации и
// ключи подключений не живут в окружении процесса runner'а. Команда
// rivet-runner при старте забирает их из окружения и аргументов в память
// и перезапускает себя с очищенным окружением: unsetenv не меняет
// окружение, которое видно в /proc/<pid>/environ и ps eww.

// IsSecretEnv — переменная окружения с секретом Rivet: токен регистрации,
// ключ native-агента, ключи подключений и токен хостинга.
func IsSecretEnv(name string) bool {
	switch name {
	case "RIVET_RUNNER_TOKEN", "RIVET_NATIVE_KEY", "RIVET_GIT_TOKEN":
		return true
	}
	return strings.HasPrefix(name, "RIVET_PROVIDER_KEY_")
}

// scrubEnv — окружение без секретов Rivet (проверки проекта и прочие
// команды runner'а, которым нужно его обычное окружение).
func scrubEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !IsSecretEnv(name) {
			out = append(out, kv)
		}
	}
	return out
}
