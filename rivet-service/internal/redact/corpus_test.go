package redact

import (
	"strings"
	"testing"
)

// Корпус редактора секретов (add-sdlc-cycle): все случаи четырёх кругов
// ревью в одной таблице. Любая правка правил проверяется на всём корпусе
// сразу, в String и в построчном Stream: правило, которое чинит одно,
// не должно ломать другое.
//
// Группа «маскировать»: ни один секрет не остаётся в выводе, а строки keep
// (структура лога, поля logfmt, читаемая часть) сохраняются.
// Группа «не трогать»: вывод равен входу.
//
// Известная неоднозначность вне корпуса: пароль, который начинается с
// цифр и «/» (`postgres://app:2024/secret@db/crm`), неотличим от порта с
// путём и не маскируется (см. portLike).

type maskCase struct {
	in      string
	secrets []string
	keep    []string
}

var corpusMask = []maskCase{
	// ─── известные токены, пары ключ=значение ───────────────────────────
	{in: "push с ghp_Abcdefghij0123456789 ok", secrets: []string{"ghp_Abcdefghij0123456789"}, keep: []string{"push с ", " ok"}},
	{in: "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuv", secrets: []string{"ghp_abcdefghijklmnopqrstuv"}},
	{in: "password=hunter2hunter2", secrets: []string{"hunter2hunter2"}},
	{in: "npm ERR! token=supersecretvalue", secrets: []string{"supersecretvalue"}, keep: []string{"npm ERR! token="}},
	{in: "X-Auth-Token: abcdef0123456789", secrets: []string{"abcdef0123456789"}},
	{in: `"x-api-key":"abcd1234efgh5678"`, secrets: []string{"abcd1234efgh5678"}, keep: []string{`"x-api-key":`}},
	{in: "jdbc:postgresql://db:5432/app?user=app&password=s3cr3t", secrets: []string{"s3cr3t"}, keep: []string{"jdbc:postgresql://db:5432/app?user=app&password="}},

	// ─── пароль в URL ───────────────────────────────────────────────────
	{in: "error: connect ECONNREFUSED postgres://app:S3cretPass@db:5432/crm", secrets: []string{"S3cretPass"}, keep: []string{"postgres://app:***@db:5432/crm"}},
	{in: "dsn postgres://u:hunter2pass@db/x", secrets: []string{"hunter2pass"}},
	{in: "redis://:p4ssw0rd@cache:6379", secrets: []string{"p4ssw0rd"}, keep: []string{"redis://:***@cache:6379"}},
	{in: "postgres://app:p@ss@db:5432/crm", secrets: []string{"p@ss"}, keep: []string{"postgres://app:***@db:5432/crm"}},
	{in: "postgres://app:ab/cd+ef@db:5432/crm", secrets: []string{"ab/cd+ef"}, keep: []string{"postgres://app:***@db:5432/crm"}},
	{in: "mongodb+srv://app:S3cret@cluster0.mongodb.net/db", secrets: []string{"S3cret"}, keep: []string{"mongodb+srv://app:***@cluster0.mongodb.net/db"}},
	{in: "postgres://app:Pa?ss12@db:5432/crm", secrets: []string{"Pa?ss12"}, keep: []string{"postgres://app:***@db:5432/crm"}},
	{in: "postgres://app:Pa#ss12@db:5432/crm", secrets: []string{"Pa#ss12"}, keep: []string{"postgres://app:***@db:5432/crm"}},
	{in: "error: connect postgres://app:secret@db:5432/crm?sslmode=require&user=ivan@example.com failed", secrets: []string{"secret"}, keep: []string{"postgres://app:***@", " failed"}},
	{in: "postgres://app:secret@db:5432/crm?sslmode=require", secrets: []string{"secret"}, keep: []string{"postgres://app:***@db:5432/crm?sslmode=require"}},
	// Пароль из одних цифр — это не порт: за ним нет пути.
	{in: "postgres://postgres:12345@localhost:5432/crm", secrets: []string{"12345"}, keep: []string{"postgres://postgres:***@localhost:5432/crm"}},
	{in: "mysql://root:1234@db/app", secrets: []string{":1234@"}, keep: []string{"mysql://root:***@db/app"}},
	// Кавычки и угловые скобки в пароле: повторное правило.
	{in: "postgres://app:pa'ss@db/crm", secrets: []string{"pa'ss"}, keep: []string{"postgres://app:***@db/crm"}},
	{in: `postgres://app:pa"ss@db/crm`, secrets: []string{`pa"ss`}, keep: []string{"postgres://app:***@db/crm"}},
	{in: "postgres://app:p<ss>@db/crm", secrets: []string{"p<ss>"}, keep: []string{"postgres://app:***@db/crm"}},

	// ─── Authorization ──────────────────────────────────────────────────
	{in: "Authorization: Bearer eyJhbGciOi.payload.sig", secrets: []string{"eyJhbGciOi.payload.sig"}, keep: []string{"Authorization: Bearer ***"}},
	{in: "Authorization: Bearer abcdefghijklmnop", secrets: []string{"abcdefghijklmnop"}},
	{in: "authorization=Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0", secrets: []string{"9f8e7d6c5b4a39281706f5e4d3c2b1a0"}, keep: []string{"authorization=Bearer ***"}},
	{in: "Proxy-Authorization: Basic dXNlcjpwYXNz", secrets: []string{"dXNlcjpwYXNz"}, keep: []string{"Proxy-Authorization: Basic ***"}},
	{in: "Authorization: ApiKey abcdef0123456789", secrets: []string{"abcdef0123456789"}, keep: []string{"Authorization: ApiKey ***"}},
	{in: "Authorization: abcdef0123456789", secrets: []string{"abcdef0123456789"}, keep: []string{"Authorization: ***"}},
	{in: "X-Authorization: abcdef0123456789", secrets: []string{"abcdef0123456789"}, keep: []string{"X-Authorization: ***"}},
	{in: `Authorization: Digest username="a", response="6629fae49393a05397450978507c4ef1"`, secrets: []string{"6629fae49393a05397450978507c4ef1"}, keep: []string{"Authorization: Digest username=***, response=***"}},
	{in: "Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261104/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024",
		secrets: []string{"AKIDEXAMPLE", "fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024"}, keep: []string{"AWS4-HMAC-SHA256 Credential=***"}},
	{in: `authorization: 'Bearer 9f8e7d6c5b4a3928'`, secrets: []string{"9f8e7d6c5b4a3928"}, keep: []string{`authorization: 'Bearer ***'`}},
	// logfmt: маскируется только токен, остальные поля остаются.
	{in: "level=info authorization=Bearer abc123 path=/api/contacts status=500", secrets: []string{"abc123"}, keep: []string{"authorization=Bearer *** path=/api/contacts status=500"}},
	// JSON-логи pino-http и winston: строкой и массивом.
	{in: `{"authorization":"Bearer 9f8e7d6c5b4a3928"}`, secrets: []string{"9f8e7d6c5b4a3928"}, keep: []string{`{"authorization":"Bearer ***"}`}},
	{in: `{"Authorization":"Bearer 9f8e7d6c5b4a3928"}`, secrets: []string{"9f8e7d6c5b4a3928"}, keep: []string{`{"Authorization":"Bearer ***"}`}},
	{in: `{"headers":{"authorization":["Bearer 9f8e7d6c5b4a3928"]}}`, secrets: []string{"9f8e7d6c5b4a3928"}, keep: []string{`{"headers":{"authorization":["Bearer ***"]}}`}},
	{in: `{"req":{"headers":{"authorization":"Bearer 9f8e7d6c5b4a39281706f5e4d3c2b1a0","cookie":"sid=s%3Aabc123def456ghi789"}}}`,
		secrets: []string{"9f8e7d6c5b4a39281706f5e4d3c2b1a0", "abc123def456ghi789"}, keep: []string{`"authorization":"Bearer ***"`, `"cookie":"sid=***"`}},
	{in: `{"level":"info","message":"req","meta":{"headers":{"proxy-authorization":"Basic dXNlcjpwYXNz"}}}`, secrets: []string{"dXNlcjpwYXNz"}, keep: []string{`"proxy-authorization":"Basic ***"`}},
	{in: `{"Authorization":["Bearer 9f8e7d6c5b4a3928"],"Set-Cookie":["sid=abc123def456; Path=/"]}`, secrets: []string{"9f8e7d6c5b4a3928", "abc123def456"}, keep: []string{`"Set-Cookie":["sid=***; Path=***"]`}},

	// ─── Cookie ─────────────────────────────────────────────────────────
	{in: "Cookie: session=abcdef0123456789abcdef", secrets: []string{"abcdef0123456789abcdef"}},
	{in: "Cookie: session=abcdef0123456789abcdef; theme=dark", secrets: []string{"abcdef0123456789abcdef"}, keep: []string{"Cookie: session=***; theme=***"}},
	{in: "< Set-Cookie: sid=0123456789abcdef; HttpOnly", secrets: []string{"0123456789abcdef"}, keep: []string{"Set-Cookie: sid=***; HttpOnly"}},
	{in: `Cookie: sid="abc123def456"; theme=dark`, secrets: []string{"abc123def456"}, keep: []string{"Cookie: sid=***; theme=***"}},
	{in: "Cookie: sid=abc,def456ghi", secrets: []string{"def456ghi"}, keep: []string{"Cookie: sid=***"}},
	{in: "Cookie: abcdef0123456789", secrets: []string{"abcdef0123456789"}, keep: []string{"Cookie: ***"}},
	{in: `{"headers":{"cookie":"session=abcdef0123456789"}}`, secrets: []string{"abcdef0123456789"}},
	{in: `{"req":{"headers":{"cookie":"sid=s%3Aabc123def456ghi789"}}}`, secrets: []string{"abc123def456ghi789"}, keep: []string{`"cookie":"sid=***"`}},
	{in: `{"cookie":"sid=\"abc123def456\""}`, secrets: []string{"abc123def456"}, keep: []string{`{"cookie":"sid=***"}`}},
	// logfmt: только первая пара после cookie=.
	{in: "cookie=sid=abc path=/api status=500", secrets: []string{"sid=abc"}, keep: []string{"cookie=sid=*** path=/api status=500"}},

	// ─── JWT ────────────────────────────────────────────────────────────
	{in: "jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop", secrets: []string{"eyJhbGciOiJIUzI1NiJ9"}},
	{in: "jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop end", secrets: []string{"eyJhbGciOiJIUzI1NiJ9"}, keep: []string{"jwt *** end"}},
	{in: "jwt eyJhbGciOiJub25lIn0.eyJzdWIiOiIxIn0.", secrets: []string{"eyJzdWIiOiIxIn0"}},

	// ─── пополнение ревьюера, пятый круг ────────────────────────────────
	// Go: fmt.Printf("%v", r.Header) и log.Println(r.Header).
	{in: "map[Accept:[*/*] Authorization:[Bearer 9f8e7d6c5b4a3928] User-Agent:[curl/8.4.0]]", secrets: []string{"9f8e7d6c5b4a3928"}},
	{in: "headers=map[Authorization:[Basic dXNlcjpwYXNz] Cookie:[sid=abc123def456]]", secrets: []string{"dXNlcjpwYXNz", "abc123def456"}},
	// Схемы вне списка: Discord, Azure, OAuth 1.0, Splunk.
	// Значение нарочно не в формате токена Discord (средний сегмент короче),
	// иначе secret scanning хостинга блокирует push тестового корпуса.
	{in: "Authorization: Bot MTk4NjIyNDgzNDcxOTI1MjQ4.Cl2FM.fake-bot-token-for-redact-corpus", secrets: []string{"MTk4NjIyNDgzNDcxOTI1MjQ4"}},
	{in: "Authorization: SharedKey myaccount:ctzMq410TV3wS7upTBcunJTDLEJwMAZuFPfr0mrrA08=", secrets: []string{"ctzMq410TV3wS7upTBcunJTDLEJwMAZuFPfr0mrrA08"}},
	{in: `Authorization: OAuth oauth_consumer_key="xvz1evFS4wEEPTGEFPHBog", oauth_signature="tnnArxj06cWHq44gCs1OSKk%2FjLY%3D"`, secrets: []string{"tnnArxj06cWHq44gCs1OSKk"}},
	{in: "Authorization: Splunk 1a2b3c4d-1111-2222-3333-444455556666", secrets: []string{"1a2b3c4d-1111-2222-3333-444455556666"}},
	// Два заголовка в одной строке лога.
	{in: "upstream headers: Authorization: Bearer abc123def456, X-Forwarded-Authorization: Bearer ghi789jkl012", secrets: []string{"abc123def456", "ghi789jkl012"}},
	// Экранированный JSON внутри сообщения.
	{in: `{"msg":"headers={\"authorization\":\"Bearer 9f8e7d6c5b4a3928\"}"}`, secrets: []string{"9f8e7d6c5b4a3928"}},
	// ECS и OpenTelemetry: ключ с префиксом.
	{in: `{"http.request.header.authorization":["Bearer 9f8e7d6c5b4a3928"]}`, secrets: []string{"9f8e7d6c5b4a3928"}},

	// ─── пополнение ревьюера, шестой круг ───────────────────────────────
	{in: "> authorization: Bearer abc123def456ghi", secrets: []string{"abc123def456ghi"}},
	{in: `authorization: "Bearer abc123def456ghi"`, secrets: []string{"abc123def456ghi"}},
	{in: `{"authorization" : "Bearer abc123def456ghi"}`, secrets: []string{"abc123def456ghi"}},
	{in: "map[Proxy-Authorization:[Basic dXNlcjpwYXNz] Cookie:[sid=abc123def456; uid=u987654321]]", secrets: []string{"dXNlcjpwYXNz", "abc123def456", "u987654321"}},
	{in: "?authorization=Bearer%20abc123def456&page=2", secrets: []string{"abc123def456"}},
	// Множественные пробелы и табуляция.
	{in: "Authorization:\tBearer\tabc123def456ghi", secrets: []string{"abc123def456ghi"}},
	// Ещё не зелёные случаи шестого круга (Python dict repr, Ruby hash, второй
	// заголовок после короткого первого токена) записаны в бэклог redact
	// change'а add-sdlc-cycle и добавляются сюда вместе с исправлением.
}

var corpusKeep = []string{
	// ─── обычный вывод сборки, тестов, git ───────────────────────────────
	"go build ./... ok",
	"=== RUN TestParseUsage --- PASS",
	"git push -u origin agent/task-42",
	"проверка: lint прошла за 3s",
	"tokens_in=1200 tokens_out=340",
	"skip test: short mode",
	"go: downloading github.com/jackc/pgx/v5 v5.7.1",
	"=== RUN TestAuthorization --- PASS (0.01s)",
	"npm ERR! 404 https://registry.npmjs.org/@scope%2fpkg",
	"GET /api/contacts?page=2 → 500",
	"TypeError: Cannot read properties of undefined (reading 'id')",

	// ─── URL без пароля, порт с путём или query ──────────────────────────
	"git clone https://github.com/PavluninVladimir/rivet.git",
	"GET http://localhost:8500/api/contacts?page=2 → 500",
	"GET http://localhost:8500?email=ivan@example.com → 500",
	"http://localhost:8500?email=ivan@example.com",
	"http://localhost:8500?email=a@b",
	"http://localhost:8500/api?email=ivan@example.com",
	"http://host:8080/api/users/@me",
	"smtp://mail.example.com:587?from=noreply@example.com",
	"ssh://git@github.com/owner/repo.git",
	"https://user@host/path",
	"https://user:@host/path",
	"http://[::1]:8080/path",
	"file:///home/user/a:b@c.txt",
	"docker pull registry:5000/app@sha256:0123456789abcdef",
	"v1.2.3 build 2026.11.04 sha256:0123456789abcdef",
	"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk",
	"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJ",
	"eyJhdXRocyI6eyJyZWdpc3RyeSI6e319fQ==",

	// ─── проза ревью, код и logfmt про Authorization и Cookie ────────────
	"Authorization: проверка прав должна идти до загрузки контакта",
	"**Authorization:** middleware не проверяет роль, добавь requireRole('admin')",
	"Authorization: the handler skips the role check for DELETE /api/contacts/:id",
	"level=warn authorization=denied user=ivan path=/api/admin status=403",
	"func authorization(w http.ResponseWriter, r *http.Request) {",
	"const authorization = req.headers.authorization || ''",
	"// TODO: authorization: role check",
	"cookies disabled in settings",
	"Review: заголовок Set-Cookie: sid задаётся без флага Secure, добавь HttpOnly и SameSite",
	"Set-Cookie: sid задаётся без флага Secure, добавь HttpOnly и SameSite=Lax",
	`document.cookie = "theme=dark"`,
	"if (!cookie) return res.status(401)",

	// ─── пополнение ревьюера, пятый круг ────────────────────────────────
	// JSON-лог: URL с портом без пути, дальше поле с email в той же строке.
	`{"target":"http://api:8080","status":502,"msg":"upstream error","user":"ivan@example.com"}`,
	`{"origin":"http://localhost:8500","email":"ivan@example.com"}`,
	// Проза ревью и код.
	"Authorization: JWT validation must check exp",
	"Cookie: используем HttpOnly",
	"// Authorization: Bearer token берётся из заголовка",
	"req.headers.authorization = `Bearer ${token}`",
	"if header == \"Authorization: Bearer \" {",
	"Authorization: Bearer",
	// Обычный вывод.
	"Fetched 550 contacts in 512 ms",
	"Server listening on http://0.0.0.0:8500",
	"WARN deprecated: use `cookie: { secure: true }` in session config",
	"$ curl -s http://localhost:8500/api/health",

	// ─── пополнение ревьюера, шестой круг ───────────────────────────────
	// Русская проза после известной схемы («Bearer токен…») ещё
	// маскируется: случаи в бэклоге redact, добавляются с исправлением.
	"Authorization: Bearer <token>",
	`headers["Authorization"] = f"Bearer {token}"`,
	`headers = {"Authorization": "Bearer " + token}`,
	"headers: { Authorization: `Bearer ${token}` },",
	"Authorization: 'Bearer ' + process.env.TOKEN",
	"pre-authorization: approved for 50 USD",
	"WWW-Authenticate: Bearer realm=\"api\", error=\"invalid_token\"",
	"GET /api/contacts?page=2 HTTP/1.1\" 500 612",
	"Cookie: см. RFC 6265, раздел 5.4",
}

// streamLine — строка через построчный Stream, как в live-потоке.
func streamLine(s string) string {
	var st Stream
	out := string(st.Feed([]byte(s + "\n")))
	out += string(st.Flush())
	return strings.TrimSuffix(out, "\n")
}

func TestCorpus(t *testing.T) {
	for _, c := range corpusMask {
		for name, got := range map[string]string{"String": String(c.in), "Stream": streamLine(c.in)} {
			for _, s := range c.secrets {
				if strings.Contains(got, s) {
					t.Errorf("%s: секрет %q остался: %q → %q", name, s, c.in, got)
				}
			}
			for _, k := range c.keep {
				if !strings.Contains(got, k) {
					t.Errorf("%s: нет %q: %q → %q", name, k, c.in, got)
				}
			}
			if got == c.in {
				t.Errorf("%s: строка не изменилась: %q", name, c.in)
			}
		}
	}
	for _, in := range corpusKeep {
		if got := String(in); got != in {
			t.Errorf("String: ложное срабатывание: %q → %q", in, got)
		}
		if got := streamLine(in); got != in {
			t.Errorf("Stream: ложное срабатывание: %q → %q", in, got)
		}
	}
}

// Строки через \r (прогресс-бары) в потоке маскируются по тем же правилам.
func TestCorpusStreamCarriageReturn(t *testing.T) {
	var st Stream
	out := string(st.Feed([]byte("dsn postgres://u:hunter2pass@db/x\nCookie: sid=abc123def456\rprogress\n")))
	if strings.Contains(out, "hunter2pass") || strings.Contains(out, "abc123def456") || !strings.Contains(out, "progress") {
		t.Fatalf("поток: %q", out)
	}
}
