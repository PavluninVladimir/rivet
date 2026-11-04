package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// Проверки URL силами control plane (Verify внешней доставки, синхронизация
// GitOps, наблюдение окружений с внешней доставкой). Адрес задаёт
// администратор, но запрос уходит из сети control plane: без ограничения он
// достаёт метаданные облака, MinIO и внутренние сервисы установки (SSRF).
// Поэтому по умолчанию закрыты loopback, link-local, частные, служебные и
// multicast-адреса, в том числе IPv4 внутри NAT64, 6to4 и IPv4-совместимых
// IPv6. Проверка идёт после резолва, в Dialer.Control: подмена DNS между
// проверкой и соединением её не обходит.
//
// RIVET_ALLOW_PRIVATE_URLS снимает ограничение: `true` — целиком (тогда
// работает и прокси окружения), список CIDR — открывает эти диапазоны,
// остальное по-прежнему закрыто, прокси не используется (он обошёл бы
// фильтр).

// URLPolicy — какие адреса доступны проверкам URL control plane'ом.
type URLPolicy struct {
	// AllowAll — фильтр снят: любые адреса, прокси из окружения.
	AllowAll bool
	// Allow — диапазоны, открытые поверх фильтра.
	Allow []netip.Prefix
}

// ParseURLPolicy — значение RIVET_ALLOW_PRIVATE_URLS: пусто, `true` или
// список CIDR через запятую.
func ParseURLPolicy(s string) (URLPolicy, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "false", "0":
		return URLPolicy{}, nil
	case "true", "1":
		return URLPolicy{AllowAll: true}, nil
	}
	var p URLPolicy
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pr, err := netip.ParsePrefix(part)
		if err != nil && !strings.Contains(part, "/") {
			// Голый адрес — диапазон из одного адреса (/32 или /128).
			if a, aerr := netip.ParseAddr(part); aerr == nil {
				pr, err = netip.PrefixFrom(a, a.BitLen()), nil
			}
		}
		if err != nil {
			return URLPolicy{}, fmt.Errorf("RIVET_ALLOW_PRIVATE_URLS: ожидается true или список CIDR, %q: %w", part, err)
		}
		p.Allow = append(p.Allow, pr.Masked())
	}
	return p, nil
}

// errPrivateAddress — адрес закрыт политикой установки. Это ошибка
// настройки, а не сбой приложения: проверки дают unknown или эскалацию, но
// не инцидент и не задачу-баг.
var errPrivateAddress = errors.New("адрес во внутренней сети запрещён политикой установки, задайте RIVET_ALLOW_PRIVATE_URLS")

// isPrivateAddress — ошибка проверки из-за политики адресов.
func isPrivateAddress(err error) bool { return errors.Is(err, errPrivateAddress) }

// blockedPrefixes — служебные диапазоны (IANA special-purpose) без публичных
// адресов назначения.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "::ffff:0:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32",
		"fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour  = netip.MustParsePrefix("2002::/16")
	v4compat   = netip.MustParsePrefix("::/96")
	v4fromIPv6 = func(a netip.Addr, at int) netip.Addr {
		b := a.As16()
		return netip.AddrFrom4([4]byte{b[at], b[at+1], b[at+2], b[at+3]})
	}
)

// embeddedIPv4 — IPv4 внутри IPv6-адреса: IPv4-mapped, NAT64, 6to4,
// IPv4-совместимый. ok=false — встроенного IPv4 нет.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	switch {
	case a.Is4In6():
		return a.Unmap(), true
	case nat64.Contains(a):
		return v4fromIPv6(a, 12), true
	case sixToFour.Contains(a):
		return v4fromIPv6(a, 2), true
	case v4compat.Contains(a) && a != netip.IPv6Unspecified() && a != netip.IPv6Loopback():
		return v4fromIPv6(a, 12), true
	}
	return netip.Addr{}, false
}

// blocked — адрес закрыт для проверок при политике p.
func (p URLPolicy) blocked(a netip.Addr) bool {
	if p.AllowAll {
		return false
	}
	for _, pr := range p.Allow {
		if pr.Contains(a) {
			return false
		}
	}
	if v4, ok := embeddedIPv4(a); ok && a.Is6() {
		return p.blocked(v4)
	}
	if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() ||
		a.IsPrivate() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return true
	}
	for _, pr := range blockedPrefixes {
		if pr.Contains(a) {
			return true
		}
	}
	return false
}

// blockedIP — адрес закрыт фильтром по умолчанию (без открытых диапазонов).
func blockedIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	return !ok || URLPolicy{}.blocked(a)
}

func (p URLPolicy) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	a, err := netip.ParseAddr(host)
	if err != nil || p.blocked(a.WithZone("")) {
		return fmt.Errorf("%s: %w", host, errPrivateAddress)
	}
	return nil
}

// urlHTTP — общий клиент проверок: один Transport на процесс (соединения
// переиспользуются), без редиректов. Без фильтра работает прокси
// окружения, как до появления фильтра.
func (e *Engine) urlHTTP() *http.Client {
	e.urlOnce.Do(func() {
		p := e.URLPolicy
		d := &net.Dialer{Timeout: 10 * time.Second}
		tr := &http.Transport{
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        16,
			IdleConnTimeout:     60 * time.Second,
		}
		if p.AllowAll {
			tr.Proxy = http.ProxyFromEnvironment
		} else {
			d.Control = p.control
		}
		e.urlClient = &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return e.urlClient
}

// verifyDeployURL — health-check окружения силами control plane: 2xx на
// GET считается успехом, как и у проверки runner'ом.
func (e *Engine) verifyDeployURL(ctx context.Context, rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("у окружения не настроен verify_url")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := e.urlHTTP().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s ответил %d", rawURL, resp.StatusCode)
	}
	return nil
}
