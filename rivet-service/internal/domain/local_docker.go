package domain

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

// LocalDockerEnvironmentConfig содержит только параметры приложения.
// Публикуемый адрес, порт хоста и команды Docker задаёт платформа.
type LocalDockerEnvironmentConfig struct {
	ContainerPort   int               `json:"container_port"`
	HealthPath      string            `json:"health_path"`
	Recipe          string            `json:"recipe"`
	BuildContext    string            `json:"build_context"`
	StartupTimeoutS int               `json:"startup_timeout_s"`
	StopTimeoutS    int               `json:"stop_timeout_s"`
	Env             map[string]string `json:"env,omitempty"`
}

func strictConfig(data []byte, dst any, field string) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		msg := "невалидный объект конфигурации"
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			msg = "неизвестное поле " + strings.TrimPrefix(err.Error(), "json: unknown field ")
		}
		return &EnvFieldError{field, msg}
	}
	return nil
}

func (c *LocalDockerEnvironmentConfig) UnmarshalJSON(data []byte) error {
	type plain LocalDockerEnvironmentConfig
	var v plain
	if err := strictConfig(data, &v, "config.local_docker"); err != nil {
		return err
	}
	*c = LocalDockerEnvironmentConfig(v)
	return nil
}

func (c *EnvConfig) UnmarshalJSON(data []byte) error {
	type plain EnvConfig
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	// Старые типы сохраняют совместимость со своими прежними DTO.
	if v.LocalDocker != nil {
		if err := strictConfig(data, &v, "config"); err != nil {
			return err
		}
	}
	*c = EnvConfig(v)
	return nil
}

func (c *LocalDockerEnvironmentConfig) Defaults() {
	if c.HealthPath == "" {
		c.HealthPath = "/"
	}
	if c.Recipe == "" {
		c.Recipe = "auto"
	}
	if c.BuildContext == "" {
		c.BuildContext = "."
	}
	if c.StartupTimeoutS == 0 {
		c.StartupTimeoutS = 120
	}
	if c.StopTimeoutS == 0 {
		c.StopTimeoutS = 10
	}
}

var dockerEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c EnvConfig) validateLocalDocker() error {
	bad := func(field, msg string) error { return &EnvFieldError{"config." + field, msg} }
	if c.LocalDocker == nil {
		return bad("local_docker", "нужна конфигурация приложения")
	}
	// Никакие команды, сетевые режимы и поля чужих доставок не переходят
	// из настроек окружения в привилегированное задание Docker-runner'а.
	copy := c
	copy.LocalDocker, copy.Monitor = nil, nil
	b, _ := json.Marshal(copy)
	var other map[string]json.RawMessage
	_ = json.Unmarshal(b, &other)
	for field := range other {
		return bad(field, "локальная Docker-публикация не принимает поля других типов доставки")
	}
	if c.Monitor != nil && (c.Monitor.CheckCmd != "" || c.Monitor.LogsCmd != "" || c.Monitor.CheckURL != "") {
		return bad("monitor", "команды и адрес наблюдения задаёт платформа")
	}
	v := c.LocalDocker
	v.Defaults()
	if v.ContainerPort < 1 || v.ContainerPort > 65535 {
		return bad("local_docker.container_port", "ожидается 1–65535")
	}
	if !strings.HasPrefix(v.HealthPath, "/") || strings.HasPrefix(v.HealthPath, "//") || len(v.HealthPath) > 200 || strings.ContainsAny(v.HealthPath, "\x00\r\n\\#") {
		return bad("local_docker.health_path", "ожидается путь от / длиной до 200 символов")
	}
	switch v.Recipe {
	case "auto", "go", "node", "static":
	default:
		return bad("local_docker.recipe", "ожидается auto, go, node или static")
	}
	if path.IsAbs(v.BuildContext) || strings.ContainsAny(v.BuildContext, "\\\x00\r\n") {
		return bad("local_docker.build_context", "ожидается относительный путь внутри репозитория")
	}
	for _, part := range strings.Split(v.BuildContext, "/") {
		if part == ".." {
			return bad("local_docker.build_context", "выход из репозитория запрещён")
		}
	}
	if v.StartupTimeoutS < 10 || v.StartupTimeoutS > 600 {
		return bad("local_docker.startup_timeout_s", "ожидается 10–600")
	}
	if v.StopTimeoutS < 1 || v.StopTimeoutS > 120 {
		return bad("local_docker.stop_timeout_s", "ожидается 1–120")
	}
	if len(v.Env) > 64 {
		return bad("local_docker.env", "допустимо не больше 64 переменных")
	}
	for k, val := range v.Env {
		if !dockerEnvName.MatchString(k) || strings.HasPrefix(k, "RIVET_") {
			return bad("local_docker.env", "недопустимое или зарезервированное имя переменной")
		}
		if len(val) > 4096 || strings.ContainsAny(val, "\x00\r\n") {
			return bad("local_docker.env", "значение должно быть одной строкой до 4096 символов")
		}
	}
	return nil
}
