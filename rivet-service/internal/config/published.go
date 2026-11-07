package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Опубликованные значения (спека access-policy «Безопасные умолчания
// установки»): ключи шифрования и пароли администратора, которые
// когда-либо стояли в документации, примерах конфигурации и стендах
// репозиториев Rivet. Их знает любой, кто читал репозиторий, поэтому
// установка с ними не защищена: rivetd с ними не стартует.
//
// Список только растёт. Значение, которое появилось в README, примере или
// скрипте стенда, добавляется сюда в той же правке.

// publishedSecretKeys: ключи RIVET_SECRET_KEY в base64.
var publishedSecretKeys = []string{
	// docker-compose.yml до open-source: ключ по умолчанию локального стенда.
	"cml2ZXQtbG9jYWwtZGV2LWtleS0zMi1ieXRlcyEhISE=",
}

// publishedAdminPasswords: пароли RIVET_ADMIN_PASSWORD.
var publishedAdminPasswords = []string{
	"admin-local",  // docker-compose.yml и README до open-source
	"change-me",    // README rivet и rivet-web до open-source
	"e2e-password", // scripts/e2e-stand.sh и сценарии rivet-e2e до open-source
}

// ErrPublished: значение настройки опубликовано в документации или
// примерах.
var ErrPublished = errors.New("значение опубликовано")

// IsPublishedSecretKey: ключ совпадает с опубликованным. Сравниваются
// байты ключа, а не строка: тот же ключ с другим переносом или пробелами
// по краям остаётся тем же ключом.
func IsPublishedSecretKey(keyB64 string) bool {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil || len(key) == 0 {
		return false
	}
	for _, p := range publishedSecretKeys {
		pk, _ := base64.StdEncoding.DecodeString(p)
		if bytes.Equal(key, pk) {
			return true
		}
	}
	return false
}

// IsPublishedAdminPassword: пароль совпадает с опубликованным.
func IsPublishedAdminPassword(pw string) bool {
	for _, p := range publishedAdminPasswords {
		if pw == p {
			return true
		}
	}
	return false
}

// PublishedAdminPasswords: копия списка опубликованных паролей для
// проверки хэшей в базе и новых паролей пользователей.
func PublishedAdminPasswords() []string {
	return append([]string(nil), publishedAdminPasswords...)
}

// CheckPublished: rivetd не стартует с ключом шифрования из документации и
// примеров. Ошибка называет переменную и команду, которой получить своё
// значение, само значение в ошибку и журнал не попадает. Пароль
// администратора проверяет AdminPasswordCheck: ему нужно знать, есть ли
// пользователи.
func (c Config) CheckPublished() error {
	if c.SecretKey != "" && IsPublishedSecretKey(c.SecretKey) {
		return fmt.Errorf("%w: RIVET_SECRET_KEY совпадает с ключом из документации или примеров конфигурации, им может расшифровать секреты кто угодно. "+
			"Сгенерируйте свой ключ: openssl rand -base64 32. Если секреты в базе уже зашифрованы этим ключом, перешифруйте их: "+
			"RIVET_SECRET_KEY_OLD=<прежний ключ> RIVET_SECRET_KEY=<новый ключ> rivetd -rotate-key", ErrPublished)
	}
	return nil
}

// AdminPasswordCheck решает, что делать с опубликованным
// RIVET_ADMIN_PASSWORD. Пока пользователей нет, переменная станет паролем
// первого администратора, поэтому это отказ старта. Когда пользователи
// есть, переменная не читается, и опубликованное значение даёт только
// предупреждение (пароли в базе проверяет store.FlagPublishedPasswords).
func AdminPasswordCheck(pw string, hasUsers bool) (warning string, err error) {
	if pw == "" || !IsPublishedAdminPassword(pw) {
		return "", nil
	}
	if !hasUsers {
		return "", fmt.Errorf("%w: RIVET_ADMIN_PASSWORD совпадает с паролем из документации или примеров конфигурации, а он станет паролем первого администратора. "+
			"Задайте свой пароль не короче 8 символов, например: openssl rand -base64 18", ErrPublished)
	}
	return "RIVET_ADMIN_PASSWORD совпадает с паролем из документации. Пользователи уже есть, и переменная не читается, но лучше убрать её или задать своё значение", nil
}

// CheckSourceURL: адрес репозитория для ссылки на исходники. Только
// http(s) с хостом, без учётных данных, query и фрагмента: адрес отдаётся
// без входа любому, кто откроет страницу входа.
func CheckSourceURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("ожидается адрес репозитория вида https://host/owner/repo, без логина, пароля и токена в адресе")
	}
	return nil
}

// CheckServe: проверки конфигурации, нужные только серверу rivetd (CLI
// rivet читает ту же конфигурацию ради адреса базы и их не делает).
func (c Config) CheckServe() error {
	if err := c.CheckPublished(); err != nil {
		return err
	}
	if c.SourceURL != "" {
		if err := CheckSourceURL(c.SourceURL); err != nil {
			return fmt.Errorf("RIVET_SOURCE_URL: %w", err)
		}
	}
	return nil
}

// IsLoopbackAddr: адрес вида host:port (или просто хост) доступен только
// с этой машины: localhost или loopback-IP. Пустой хост (":8080"), 0.0.0.0,
// :: и любые другие имена считаются открытыми наружу. Единственный такой
// помощник: им пользуются и предупреждения о TLS, и о клиентских
// сертификатах runner'ов.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ListenWarnings: предупреждения журнала о сетевых адресах: протокол
// runner'ов вне loopback без TLS передаёт токены runner'ов и секреты
// назначений открытым текстом.
func (c Config) ListenWarnings() []string {
	var out []string
	if c.GRPCTLSCert == "" && !IsLoopbackAddr(c.GRPCAddr) {
		out = append(out, "протокол runner'ов слушает "+c.GRPCAddr+" без TLS: токены runner'ов и секреты назначений пойдут по сети открытым текстом. "+
			"Задайте RIVET_GRPC_TLS_CERT и RIVET_GRPC_TLS_KEY или закройте порт периметром (docker-compose публикует его только на 127.0.0.1 хоста)")
	}
	return out
}
