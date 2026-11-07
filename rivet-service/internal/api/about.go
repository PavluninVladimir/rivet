package api

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/config"
)

// Сведения о сборке и ссылка на исходный код (спека source-availability,
// AGPL §13): GET /api/v1/about без входа и без базы.

// License: SPDX-идентификатор лицензии Rivet.
const License = "AGPL-3.0-only"

// About: ответ GET /api/v1/about. Только сведения о сборке: данных
// установки (пользователей, проектов, runner'ов, настроек) здесь нет.
type About struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	SourceURL    string `json:"source_url"`
	SourceRefURL string `json:"source_ref_url"`
	License      string `json:"license"`
}

// releaseTag: версия, которая сама является тегом: v1.2.3 и
// v1.2.3-rc.1 (только с v, как теги репозитория). Вывод git describe между тегами (v1.2.3-4-gabc1234) и
// сборка из грязного дерева (…-dirty) тегом не являются.
var (
	releaseTag   = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)
	describeTail = regexp.MustCompile(`-\d+-g[0-9a-f]{4,40}(-dirty)?$|-dirty$`)
	commitSHA    = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// NewAbout собирает сведения о сборке. override: адрес из настройки
// установки (RIVET_SOURCE_URL): форк указывает свой репозиторий без
// пересборки. Ссылка ведёт на тег, если версия является тегом, иначе на коммит, а у
// сборки без версии и коммита на корень репозитория.
func NewAbout(version, commit, buildSourceURL, override string) About {
	a := About{
		Version:   strings.TrimSpace(version),
		Commit:    strings.ToLower(strings.TrimSpace(commit)),
		SourceURL: strings.TrimSuffix(strings.TrimSpace(buildSourceURL), "/"),
		License:   License,
	}
	if a.Version == "" {
		a.Version = "dev"
	}
	// Адрес из сборки проверяется так же, как настройка: без него ссылки
	// не было бы вовсе (консоль отбрасывает не-http адреса), а AGPL §13
	// требует её показывать.
	if config.CheckSourceURL(a.SourceURL) != nil {
		a.SourceURL = DefaultSourceURL
	}
	if !commitSHA.MatchString(a.Commit) {
		a.Commit = ""
	}
	if o := strings.TrimSuffix(strings.TrimSpace(override), "/"); o != "" && config.CheckSourceURL(o) == nil {
		a.SourceURL = o
	}
	switch {
	case releaseTag.MatchString(a.Version) && !describeTail.MatchString(a.Version):
		a.SourceRefURL = a.SourceURL + "/tree/" + a.Version
	case a.Commit != "":
		a.SourceRefURL = a.SourceURL + "/tree/" + a.Commit
	default:
		a.SourceRefURL = a.SourceURL
	}
	return a
}

// about отвечает на GET /api/v1/about. Путь открыт без входа (authExempt) и в базу
// не ходит, поэтому работает и тогда, когда /health отвечает down.
func (s *Server) about(w http.ResponseWriter, _ *http.Request) {
	a := s.About
	if a.License == "" {
		// Сервер собран без NewAbout (тесты, встраивание): сборка без версии.
		a = NewAbout(s.Version, "", DefaultSourceURL, "")
	}
	writeJSON(w, http.StatusOK, a)
}

// DefaultSourceURL: публичный репозиторий Rivet.
const DefaultSourceURL = "https://github.com/PavluninVladimir/rivet"
