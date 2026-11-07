package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Корни клиентских сертификатов без TLS протокола runner'ов: ошибка
// конфигурации (add-runner-identity): mTLS без TLS не бывает.
func TestGRPCClientCARequiresTLS(t *testing.T) {
	t.Setenv("RIVET_GRPC_TLS_CERT", "")
	t.Setenv("RIVET_GRPC_TLS_KEY", "")
	t.Setenv("RIVET_GRPC_CLIENT_CA", "/etc/rivet/runners-ca.pem")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "RIVET_GRPC_CLIENT_CA требует TLS") {
		t.Fatalf("корни клиентов без TLS: %v", err)
	}
	t.Setenv("RIVET_GRPC_TLS_CERT", "/etc/rivet/grpc.pem")
	t.Setenv("RIVET_GRPC_TLS_KEY", "/etc/rivet/grpc-key.pem")
	c, err := FromEnv()
	if err != nil || c.GRPCClientCA != "/etc/rivet/runners-ca.pem" {
		t.Fatalf("корни клиентов с TLS: %v %+v", err, c.GRPCClientCA)
	}
	t.Setenv("RIVET_GRPC_CLIENT_CA", "")
	if c, err := FromEnv(); err != nil || c.GRPCClientCA != "" {
		t.Fatalf("без корней клиентов: %v", err)
	}
}

// Без настроек адресов rivetd слушает только loopback (спека access-policy
// «Безопасные умолчания установки», сценарий «Запуск без настроек»).
func TestDefaultsAreLoopback(t *testing.T) {
	t.Setenv("RIVET_HTTP_ADDR", "")
	t.Setenv("RIVET_GRPC_ADDR", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != "127.0.0.1:8080" || c.GRPCAddr != "127.0.0.1:8090" {
		t.Fatalf("адреса по умолчанию: %q, %q", c.HTTPAddr, c.GRPCAddr)
	}
	if !IsLoopbackAddr(c.HTTPAddr) || !IsLoopbackAddr(c.GRPCAddr) {
		t.Fatal("адреса по умолчанию должны быть loopback")
	}
	if w := c.ListenWarnings(); len(w) != 0 {
		t.Fatalf("на loopback без TLS предупреждений нет: %v", w)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true,
		"127.0.0.2:8080": true,
		"localhost:8090": true,
		"[::1]:8090":     true,
		":8080":          false,
		"0.0.0.0:8090":   false,
		"[::]:8090":      false,
		"10.0.0.5:8090":  false,
		"rivet.lan:8090": false,
		"8090":           false,
		"127.0.0.1":      true,
		"LOCALHOST:8090": true,
		"rivet.example":  false,
	} {
		if got := IsLoopbackAddr(addr); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v, ожидалось %v", addr, got, want)
		}
	}
}

// Протокол runner'ов на всех интерфейсах без TLS даёт предупреждение
// (сценарий «Открытый наружу порт без TLS»), с TLS предупреждения нет.
func TestListenWarnings(t *testing.T) {
	for _, addr := range []string{":8090", "0.0.0.0:8090", "[::]:8090", "192.168.1.10:8090"} {
		w := Config{GRPCAddr: addr}.ListenWarnings()
		if len(w) != 1 || !strings.Contains(w[0], "открытым текстом") || !strings.Contains(w[0], "RIVET_GRPC_TLS_CERT") {
			t.Fatalf("%s без TLS: %v", addr, w)
		}
	}
	if w := (Config{GRPCAddr: "0.0.0.0:8090", GRPCTLSCert: "cert.pem", GRPCTLSKey: "key.pem"}).ListenWarnings(); len(w) != 0 {
		t.Fatalf("с TLS предупреждений нет: %v", w)
	}
}

func freshKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// С ключом из документации и примеров rivetd не стартует и объясняет
// причину (сценарий «Ключ из примера»). Тест на каждое опубликованное
// значение.
func TestCheckPublished(t *testing.T) {
	for _, k := range publishedSecretKeys {
		for _, v := range []string{k, " " + k + "\n"} {
			err := Config{SecretKey: v}.CheckPublished()
			if !errors.Is(err, ErrPublished) || !strings.Contains(err.Error(), "RIVET_SECRET_KEY") ||
				!strings.Contains(err.Error(), "openssl rand -base64 32") || !strings.Contains(err.Error(), "-rotate-key") {
				t.Fatalf("ключ %q: %v", v, err)
			}
			if strings.Contains(err.Error(), strings.TrimSpace(v)) {
				t.Fatal("ошибка не должна повторять ключ")
			}
		}
	}
	if err := (Config{SecretKey: freshKey(t), AdminPassword: "admin-local"}).CheckPublished(); err != nil {
		t.Fatalf("пароль проверяет AdminPasswordCheck, а не CheckPublished: %v", err)
	}
	if err := (Config{}).CheckPublished(); err != nil {
		t.Fatalf("без ключа (установка без шифрования секретов): %v", err)
	}
}

// Опубликованный RIVET_ADMIN_PASSWORD: на пустой установке он стал бы
// паролем первого администратора, это отказ (сценарий «Пароль из
// примера»). Когда пользователи есть, переменная не читается, и это
// только предупреждение (сценарий «Опубликованный пароль в настройке у
// установки с пользователями»).
func TestAdminPasswordCheck(t *testing.T) {
	for _, pw := range publishedAdminPasswords {
		warn, err := AdminPasswordCheck(pw, false)
		if !errors.Is(err, ErrPublished) || warn != "" || !strings.Contains(err.Error(), "RIVET_ADMIN_PASSWORD") || strings.Contains(err.Error(), pw) {
			t.Fatalf("пустая установка, %q: warn=%q err=%v", pw, warn, err)
		}
		warn, err = AdminPasswordCheck(pw, true)
		if err != nil || !strings.Contains(warn, "RIVET_ADMIN_PASSWORD") || strings.Contains(warn, pw) {
			t.Fatalf("установка с пользователями, %q: warn=%q err=%v", pw, warn, err)
		}
	}
	for _, users := range []bool{false, true} {
		if warn, err := AdminPasswordCheck("свой-длинный-пароль", users); warn != "" || err != nil {
			t.Fatalf("свой пароль: %q, %v", warn, err)
		}
		if warn, err := AdminPasswordCheck("", users); warn != "" || err != nil {
			t.Fatalf("без пароля: %q, %v", warn, err)
		}
	}
}

func TestCheckServeSourceURL(t *testing.T) {
	for _, ok := range []string{"", "https://github.com/acme/rivet", "http://git.lan/acme/rivet"} {
		if err := (Config{SourceURL: ok}).CheckServe(); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "github.com/acme/rivet", "ftp://host/x", "https://host/x?a=1", "https:///x",
		"https://oauth2:glpat-fake@gitlab.example.org/acme/rivet", "https://user@git.example.org/acme/rivet", "https://host/x#frag"} {
		if err := (Config{SourceURL: bad}).CheckServe(); err == nil {
			t.Fatalf("%q должен отклоняться", bad)
		}
	}
}

// Стандартная поставка не подставляет пароль администратора и ключ:
// docker-compose.yml требует их (${VAR:?подсказка}), а значения, которые
// стоят в .env.example, README и скриптах стендов, rivetd отклоняет
// (сценарии «Стенд без пароля и ключа» и «Ключ из примера»). Тест читает
// сами файлы: новое значение в документации без записи в список
// опубликованных роняет его.
func TestPublishedValuesCoverDocs(t *testing.T) {
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	compose := read("../../docker-compose.yml")
	// Подсказка в ${VAR:?…} не должна ломать YAML (двоеточие с пробелом в
	// значении без кавычек ломает разбор, и compose не стартует вовсе).
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		t.Fatalf("docker-compose.yml не разбирается как YAML: %v", err)
	}
	for _, v := range []string{"RIVET_ADMIN_PASSWORD", "RIVET_SECRET_KEY"} {
		re := regexp.MustCompile(`(?m)^\s+` + v + `:\s*"?\$\{` + v + `:\?([^}]+)\}"?\s*$`)
		m := re.FindStringSubmatch(compose)
		if m == nil {
			t.Fatalf("docker-compose.yml: %s должен быть обязательным (${%s:?подсказка})", v, v)
		}
		if !strings.Contains(m[1], ".env.example") {
			t.Fatalf("docker-compose.yml: подсказка %s должна ссылаться на .env.example: %q", v, m[1])
		}
	}

	// Литеральные значения: VAR=значение (в кавычках и без), YAML-форма
	// VAR: значение и умолчания ${VAR:-значение}. Подстановки ($…) и
	// заглушки (<…>) не значения.
	const names = `(RIVET_SECRET_KEY|RIVET_ADMIN_PASSWORD|E2E_ADMIN_PASSWORD|E2E_SECRET_KEY)(?:_OLD)?`
	assign := regexp.MustCompile(names + `=["']?([^\s"'$<\\]+)`)
	yamlForm := regexp.MustCompile(names + `:[ \t]+["']?([^\s"'$<\\{]+)`)
	def := regexp.MustCompile(`(SECRET_KEY|ADMIN_PASSWORD)(?:_OLD)?:-([^$}][^}]*)\}`)
	files := []string{"../../.env.example", "../../scripts/e2e-stand.sh", "../../docker-compose.yml"}
	files = append(files, docFiles(t, "../../..")...)
	// Голый ключ: строка base64 из 32 байт без имени переменной рядом
	// (например, прежний ключ в комментарии рецепта перешифровки).
	bareKey := regexp.MustCompile(`[A-Za-z0-9+/]{43}=`)
	for _, f := range files {
		text := read(f)
		for _, k := range bareKey.FindAllString(text, -1) {
			if b, err := base64.StdEncoding.DecodeString(k); err == nil && len(b) == 32 && !IsPublishedSecretKey(k) {
				t.Errorf("%s: ключ из документации не в списке опубликованных (config/published.go): %q", f, k)
			}
		}
		for _, re := range []*regexp.Regexp{assign, yamlForm, def} {
			for _, m := range re.FindAllStringSubmatch(text, -1) {
				name, val := m[1], m[2]
				published := IsPublishedAdminPassword(val)
				if strings.Contains(name, "SECRET_KEY") {
					published = IsPublishedSecretKey(val)
				}
				if !published {
					t.Errorf("%s: значение %s из документации не в списке опубликованных (config/published.go): %q", f, name, val)
				}
			}
		}
	}
	if env := read("../../.env.example"); !strings.Contains(env, "openssl rand -base64 32") {
		t.Fatal(".env.example должен подсказывать, как сгенерировать ключ")
	}
}

// docFiles: все *.md репозитория rivet: корень, docs/ и rivet-service/.
// Вложенные клиентские репозитории, зависимости и сборки пропускаются.
func docFiles(t *testing.T, root string) []string {
	t.Helper()
	skip := map[string]bool{".git": true, "node_modules": true, "dist": true, "testdata": true,
		"rivet-web": true, "rivet-e2e": true, "rivet-android": true, "rivet-ios": true, ".claude": true}
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && skip[d.Name()] {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) < 4 {
		t.Fatalf("найдено слишком мало документов (%d): тест смотрит не туда", len(out))
	}
	return out
}

// Образ сборки закреплён на версии Go из go.mod: в официальных образах
// GOTOOLCHAIN=local, и устаревший 1.26-alpine из кеша Docker не собрал бы
// модуль, которому нужен более новый Go.
func TestDockerfileGoMatchesGoMod(t *testing.T) {
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	df, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(mod)
	d := regexp.MustCompile(`(?m)^FROM golang:(\S+?)-alpine`).FindSubmatch(df)
	if m == nil {
		t.Fatal("в go.mod нет строки go <версия>")
	}
	if d == nil {
		t.Fatal("в Dockerfile нет строки FROM golang:<версия>-alpine")
	}
	if string(m[1]) != string(d[1]) {
		t.Fatalf("go.mod требует go %s, а Dockerfile собирает на golang:%s-alpine: поднимайте их вместе", m[1], d[1])
	}
}
