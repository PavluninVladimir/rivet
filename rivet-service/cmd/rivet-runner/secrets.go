package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/PavluninVladimir/rivet/internal/runner"
)

// Секреты runner'а при старте (review add-native-agent, P1-2).
//
// Токен регистрации и ключи подключений не должны оставаться ни в
// окружении процесса, ни в его командной строке: их видно через ps eww и
// /proc/<pid>/environ любому процессу того же пользователя, а unsetenv
// окружение, которое показывает ядро, не меняет. Поэтому:
//
//  1. токен лучше передавать файлом с правами 0600 (RIVET_RUNNER_TOKEN_FILE);
//  2. если секреты пришли переменными окружения или флагом -token, runner
//     переписывает их во временный файл 0600 и перезапускает себя (execve,
//     тот же pid) с очищенными окружением и аргументами. Новый процесс
//     читает файл, удаляет его и держит секреты только в памяти;
//  3. на Linux процесс помечается недампируемым (PR_SET_DUMPABLE=0): его
//     память и окружение не читаются процессами того же пользователя.
//
// Если перезапуск не удался, runner не стартует (fail closed): иначе
// секреты остались бы в окружении, которое показывает ядро. Продолжить с
// секретами в окружении можно только осознанно, с allowEnvSecretsEnv=1.

const (
	// secretsFileEnv — путь к файлу секретов при перезапуске (служебная
	// переменная, файл удаляется сразу после чтения).
	secretsFileEnv = "RIVET_RUNNER_SECRETS_FILE"
	tokenFileEnv   = "RIVET_RUNNER_TOKEN_FILE"
	tokenEnv       = "RIVET_RUNNER_TOKEN"
	// allowEnvSecretsEnv=1 — продолжить работу, если перезапуск с
	// очищенным окружением не удался (секреты останутся в окружении
	// процесса). По умолчанию runner в этом случае не стартует.
	allowEnvSecretsEnv = "RIVET_RUNNER_ALLOW_ENV_SECRETS"
)

// startup — секреты и предупреждения, с которыми runner продолжает работу.
type startup struct {
	args     []string
	secrets  map[string]string
	warnings []string
	// reexeced — процесс уже перезапущен с очищенным окружением.
	reexeced bool
}

// splitSecrets отделяет секреты от окружения и аргументов: возвращает
// аргументы без -token, окружение без секретных переменных и сами секреты.
func splitSecrets(args, environ []string) (cleanArgs, cleanEnv []string, secrets map[string]string, warnings []string) {
	secrets = map[string]string{}
	for _, kv := range environ {
		name, val, _ := strings.Cut(kv, "=")
		if runner.IsSecretEnv(name) {
			if val != "" {
				secrets[name] = val
			}
			continue
		}
		cleanEnv = append(cleanEnv, kv)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			cleanArgs = append(cleanArgs, args[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "token" {
			cleanArgs = append(cleanArgs, a)
			continue
		}
		if !hasVal && i+1 < len(args) {
			i++
			val = args[i]
		}
		if val != "" {
			secrets[tokenEnv] = val
		}
		warnings = append(warnings, "флаг -token устарел: токен виден в списке процессов, передавай его файлом с правами 0600 в "+tokenFileEnv)
	}
	return cleanArgs, cleanEnv, secrets, warnings
}

// tokenFilePath — путь файла токена из RIVET_RUNNER_TOKEN_FILE, абсолютный
// от каталога запуска runner'а. Абсолютный путь нужен песочнице: её
// помощник работает в рабочей копии, и относительный путь он искал бы там,
// а не там, где лежит файл. Читается и скрывается один и тот же путь.
func tokenFilePath() string {
	p := os.Getenv(tokenFileEnv)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// readTokenFile — токен из файла: только владелец может читать и писать.
func readTokenFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tokenFileEnv, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s: файл %s доступен не только владельцу (%v), нужны права 0600", tokenFileEnv, path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tokenFileEnv, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s: файл %s пуст", tokenFileEnv, path)
	}
	return tok, nil
}

// loadSecretsFile читает файл секретов перезапуска и удаляет его.
func loadSecretsFile(path string) (map[string]string, error) {
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// prepareSecrets — шаг старта runner'а: при необходимости перезапускает
// процесс с очищенным окружением (не возвращается) и возвращает аргументы
// для разбора флагов и секреты в памяти.
func prepareSecrets(args []string) (startup, error) {
	hardenProcess()
	st := startup{secrets: map[string]string{}}
	if path := os.Getenv(secretsFileEnv); path != "" {
		st.reexeced = true
		_ = os.Unsetenv(secretsFileEnv)
		m, err := loadSecretsFile(path)
		if err != nil {
			return st, fmt.Errorf("секреты runner'а: %w", err)
		}
		for k, v := range m {
			st.secrets[k] = v
		}
	}
	cleanArgs, cleanEnv, found, warnings := splitSecrets(args, os.Environ())
	st.args, st.warnings = cleanArgs, warnings
	for k, v := range found {
		st.secrets[k] = v
	}
	if len(found) > 0 && !st.reexeced {
		if err := reexecClean(cleanArgs, cleanEnv, st.secrets); err != nil {
			if os.Getenv(allowEnvSecretsEnv) != "1" {
				return st, fmt.Errorf("не удалось перезапуститься с очищенным окружением (%v): секреты остались бы в окружении процесса "+
					"(видно в ps eww и /proc/<pid>/environ); передай токен файлом с правами 0600 в %s и убери секреты из окружения "+
					"либо, если это осознанно, задай %s=1", err, tokenFileEnv, allowEnvSecretsEnv)
			}
			st.warnings = append(st.warnings, "не удалось перезапуститься с очищенным окружением ("+err.Error()+"): секреты остаются в окружении процесса ("+
				allowEnvSecretsEnv+"=1), передавай токен файлом "+tokenFileEnv)
		}
	}
	// Секреты больше не нужны в окружении этого процесса (для потомков
	// runner'а: они получают окружение из os.Environ()).
	for k := range found {
		_ = os.Unsetenv(k)
	}
	if path := tokenFilePath(); path != "" {
		tok, err := readTokenFile(path)
		if err != nil {
			return st, err
		}
		st.secrets[tokenEnv] = tok
	}
	return st, nil
}

// reexecClean перезапускает runner с очищенными окружением и аргументами,
// передавая секреты файлом 0600. Успешный вызов не возвращается.
func reexecClean(args, env []string, secrets map[string]string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "rivet-runner-secrets-*")
	if err != nil {
		return err
	}
	raw, err := json.Marshal(secrets)
	if err == nil {
		_, err = f.Write(raw)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	argv := append([]string{os.Args[0]}, args...)
	err = syscall.Exec(self, argv, append(env, secretsFileEnv+"="+f.Name()))
	_ = os.Remove(f.Name())
	if err == nil {
		err = errors.New("exec вернулся без ошибки")
	}
	return err
}
