package localdocker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSecretEnvFileAndNoDockerOutput(t *testing.T) {
	for _, runFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[runFailure], func(t *testing.T) {
			j := fixtureJob()
			j.Secrets = map[string]string{"ADMIN_PASSWORD": "unique-app-password = with spaces"}
			j.Sensitive = true
			root := files(t, map[string]string{"index.html": "hello"})
			var log bytes.Buffer
			secretPath := ""
			runCalled := false
			r := Runtime{Log: &log, Command: func(ctx context.Context, args []string, w io.Writer) (string, error) {
				if strings.Contains(strings.Join(args, " "), j.Secrets["ADMIN_PASSWORD"]) {
					t.Fatal("secret in argv")
				}
				if w != nil {
					_, _ = io.WriteString(w, j.Secrets["ADMIN_PASSWORD"])
				}
				switch args[0] {
				case "info", "build":
					return "ok", nil
				case "image":
					return "sha256:" + strings.Repeat("b", 64), nil
				case "volume":
					if args[1] == "inspect" {
						return "", errors.New("absent")
					}
					return "volume", nil
				case "run":
					runCalled = true
					for i, arg := range args {
						if arg == "--env-file" {
							secretPath = args[i+1]
						}
					}
					if secretPath == "" || strings.HasPrefix(secretPath, root) {
						t.Fatal("missing or unsafe env-file path")
					}
					stat, err := os.Stat(secretPath)
					if err != nil || stat.Mode().Perm() != 0600 {
						t.Fatal("env-file permissions", err)
					}
					b, err := os.ReadFile(secretPath)
					if err != nil || string(b) != "ADMIN_PASSWORD="+j.Secrets["ADMIN_PASSWORD"]+"\n" {
						t.Fatal("env-file content mismatch", err)
					}
					if runFailure {
						return j.Secrets["ADMIN_PASSWORD"], errors.New("run failed")
					}
					return strings.Repeat("a", 64), nil
				case "container":
					return "", errors.New("inspect unavailable")
				}
				return "", nil
			}}
			res := r.Publish(context.Background(), root, j)
			if !runCalled {
				t.Fatal("run not called", res)
			}
			if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
				t.Fatal("env-file remains", err)
			}
			if log.Len() != 0 {
				t.Fatal("raw Docker output was streamed")
			}
			if res.Reason != nil && strings.Contains(res.Reason.Message, j.Secrets["ADMIN_PASSWORD"]) {
				t.Fatal("secret in result")
			}
			logs := r.Operate(context.Background(), "logs", j)
			if logs.Logs != SensitiveLogMessage {
				t.Fatal("application output was exposed")
			}
		})
	}
}

func TestInspectNeverStreamsRetiredGenerationSecrets(t *testing.T) {
	var log bytes.Buffer
	r := Runtime{Log: &log, Command: func(ctx context.Context, args []string, w io.Writer) (string, error) {
		if w != nil {
			_, _ = io.WriteString(w, "retired-generation-secret")
		}
		return `[{"Config":{"Env":["OLD_SECRET=retired-generation-secret"]}}]`, nil
	}}
	for _, kind := range []string{"container", "image", "volume"} {
		if _, err := r.call(context.Background(), kind, "inspect", "old-generation"); err != nil {
			t.Fatal(err)
		}
	}
	if log.Len() != 0 {
		t.Fatal("inspect output exposed a previous generation")
	}
}
