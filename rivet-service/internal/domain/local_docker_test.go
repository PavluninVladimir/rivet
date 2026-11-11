package domain

import (
	"encoding/json"
	"testing"
)

func TestLocalDockerConfig(t *testing.T) {
	c := EnvConfig{LocalDocker: &LocalDockerEnvironmentConfig{ContainerPort: 8080}}
	if err := c.Validate(ExecLocalDocker); err != nil {
		t.Fatal(err)
	}
	v := c.LocalDocker
	if v.HealthPath != "/" || v.Recipe != "auto" || v.BuildContext != "." || v.StartupTimeoutS != 120 || v.StopTimeoutS != 10 {
		t.Fatalf("defaults: %+v", v)
	}
	if err := c.Validate(ExecSSH); err == nil {
		t.Fatal("local config accepted for ssh")
	}
	for _, tc := range []struct {
		name string
		edit func(*EnvConfig)
	}{
		{"missing", func(c *EnvConfig) { c.LocalDocker = nil }},
		{"port", func(c *EnvConfig) { c.LocalDocker.ContainerPort = 0 }},
		{"large port", func(c *EnvConfig) { c.LocalDocker.ContainerPort = 65536 }},
		{"host", func(c *EnvConfig) { c.Host = "example.org" }},
		{"command", func(c *EnvConfig) { c.DeployCmd = "docker run arbitrary" }},
		{"pipeline", func(c *EnvConfig) { c.Pipeline = "workflow" }},
		{"monitor command", func(c *EnvConfig) { c.Monitor = &MonitorConfig{CheckCmd: "curl x"} }},
		{"monitor url", func(c *EnvConfig) { c.Monitor = &MonitorConfig{CheckURL: "http://remote"} }},
		{"absolute context", func(c *EnvConfig) { c.LocalDocker.BuildContext = "/tmp" }},
		{"parent context", func(c *EnvConfig) { c.LocalDocker.BuildContext = "a/../b" }},
		{"windows context", func(c *EnvConfig) { c.LocalDocker.BuildContext = `a\..\b` }},
		{"health url", func(c *EnvConfig) { c.LocalDocker.HealthPath = "http://remote" }},
		{"health authority", func(c *EnvConfig) { c.LocalDocker.HealthPath = "//remote" }},
		{"timeout", func(c *EnvConfig) { c.LocalDocker.StartupTimeoutS = 601 }},
		{"stop timeout", func(c *EnvConfig) { c.LocalDocker.StopTimeoutS = -1 }},
		{"recipe", func(c *EnvConfig) { c.LocalDocker.Recipe = "shell" }},
		{"reserved env", func(c *EnvConfig) { c.LocalDocker.Env = map[string]string{"RIVET_REPO": "x"} }},
		{"bad env", func(c *EnvConfig) { c.LocalDocker.Env = map[string]string{"a-b": "x"} }},
		{"multiline env", func(c *EnvConfig) { c.LocalDocker.Env = map[string]string{"A": "x\ny"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := EnvConfig{LocalDocker: &LocalDockerEnvironmentConfig{ContainerPort: 8080}}
			tc.edit(&c)
			if err := c.Validate(ExecLocalDocker); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestLocalDockerStrictJSON(t *testing.T) {
	for _, raw := range []string{
		`{"local_docker":{"container_port":8080,"port":9090}}`,
		`{"local_docker":{"container_port":8080,"network_mode":"host"}}`,
		`{"local_docker":{"container_port":8080},"network_mode":"host"}`,
	} {
		var c EnvConfig
		if err := json.Unmarshal([]byte(raw), &c); err == nil {
			t.Fatalf("unknown field accepted: %s", raw)
		}
	}
	var legacy EnvConfig
	if err := json.Unmarshal([]byte(`{"deploy_cmd":"true","future_field":true}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.DeployCmd != "true" {
		t.Fatal("legacy config lost")
	}
}
