package orchestrator

import (
	"fmt"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Публикация в Kubernetes (change add-k8s-delivery, спека deployment
// «Собственная способность деплоить»): команды доставки и проверки
// собирает control plane из параметров окружения, исполняет их
// deploy-runner как обычную джобу — протокол не меняется.

// k8sRolloutTimeout — сколько kubectl ждёт готовности выката; сверху ещё
// общий дедлайн деплой-джобы.
const k8sRolloutTimeout = "300s"

// k8sJob — команды доставки и проверки k8s-окружения. Значения приходят
// уже проверенными форматом (домен), но всё равно экранируются: команда
// собирается для /bin/sh на стороне runner'а.
func k8sJob(cfg domain.EnvConfig, version string) (deployCmd, verifyCmd string) {
	ns := ""
	if cfg.Namespace != "" {
		ns = " -n " + shellQuote(cfg.Namespace)
	}
	switch {
	case cfg.Chart != "":
		// helm сам ждёт готовности релиза (--wait), поэтому Verify по
		// умолчанию — статус релиза, а не отдельное ожидание.
		var b strings.Builder
		fmt.Fprintf(&b, "helm upgrade --install %s %s%s --wait --timeout %s",
			shellQuote(cfg.Release), shellQuote(cfg.Chart), ns, k8sRolloutTimeout)
		for _, kv := range sortedValues(cfg.Values) {
			fmt.Fprintf(&b, " --set %s", shellQuote(kv))
		}
		// Версию подставляет plane и пишет её последней: значение из
		// конфигурации не должно переопределить публикуемую версию.
		fmt.Fprintf(&b, " --set %s", shellQuote("rivetVersion="+version))
		deployCmd = b.String()
		verifyCmd = fmt.Sprintf("helm status %s%s", shellQuote(cfg.Release), ns)
	default:
		// Манифесты применяются из рабочей копии; версия доезжает
		// переменной окружения команды (RIVET_VERSION).
		deployCmd = fmt.Sprintf("kubectl apply%s -f %s", ns, shellQuote(cfg.Manifests))
		if cfg.Workload != "" {
			verifyCmd = fmt.Sprintf("kubectl rollout status%s %s --timeout=%s",
				ns, shellQuote(cfg.Workload), k8sRolloutTimeout)
		}
	}
	// Заданная администратором проверка перекрывает умолчание; команду
	// доставки у k8s-окружения задать нельзя (её собирает система, иначе
	// параметры кластера теряют смысл вместе с их валидацией).
	if cfg.VerifyCmd != "" {
		verifyCmd = cfg.VerifyCmd
	}
	return deployCmd, verifyCmd
}

// sortedValues — значения --set в стабильном порядке: команда публикации
// не должна меняться от прохода к проходу.
func sortedValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	sortStrings(out)
	return out
}

// shellQuote — значение одним аргументом /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
