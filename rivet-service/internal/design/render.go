package design

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

var screenshotSlots = make(chan struct{}, 2)

type ImageAsset struct {
	MIME string
	Data []byte
}

// RenderHTML emits trusted, data-only HTML. Imported values cannot introduce
// markup, CSS declarations or network URLs. This renderer is also suitable for
// isolated previews; screenshots run with JavaScript disabled.
func RenderHTML(m Model, assets map[string]ImageAsset) (string, int, int, error) {
	if err := Validate(m); err != nil {
		return "", 0, 0, err
	}
	if len(m.Screens) == 0 {
		return "", 0, 0, Invalid("invalid_ref", "screens", "для снимка нужен хотя бы один экран")
	}
	tokens := map[string]Token{}
	for _, t := range m.Tokens {
		tokens[t.ID] = t
	}
	components := map[string]Component{}
	for _, c := range m.Components {
		components[c.ID] = c
	}
	count := 0
	resolve := func(p Properties) Properties {
		r := Properties{}
		for k, v := range p {
			r[k] = v
			if id, ok := ref(v); ok {
				t := tokens[id]
				var vals map[string]json.RawMessage
				_ = json.Unmarshal(t.Value, &vals)
				switch t.Type {
				case "spacing":
					r[k] = vals["px"]
				case "color":
					r[k] = vals["hex"]
				}
			}
		}
		return r
	}
	value := func(p Properties, k, def string) string {
		v, ok := p[k]
		if !ok {
			return def
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		return string(v)
	}
	var render func(Node, Properties) (string, error)
	render = func(n Node, extra Properties) (string, error) {
		count++
		if count > 20000 {
			return "", Invalid("size_limit", "components", "слишком много развёрнутых узлов")
		}
		props := Properties{}
		for k, v := range n.Props {
			props[k] = v
		}
		for k, v := range extra {
			props[k] = v
		}
		if n.Type == "instance" {
			defaults := Properties{}
			for _, v := range components[n.ComponentID].Variants {
				if v.State == "default" {
					for k, value := range v.Props {
						defaults[k] = value
					}
				}
			}
			for k, value := range props {
				defaults[k] = value
			}
			props = defaults
			for _, o := range m.Overrides {
				if o.InstanceID == n.ID {
					props[o.Property] = o.Value
				}
			}
			return render(components[n.ComponentID].Root, props)
		}
		p := resolve(props)
		styles := []string{"box-sizing:border-box"}
		if _, hasDirection := p["direction"]; hasDirection {
			styles = append(styles, "display:flex", "flex-direction:"+value(p, "direction", "column"), "align-items:"+map[string]string{"start": "flex-start", "center": "center", "end": "flex-end", "stretch": "stretch"}[value(p, "alignment", "stretch")])
		}
		for key, css := range map[string]string{"color": "color", "background": "background-color", "border_color": "border-color"} {
			if v, ok := p[key]; ok {
				var s string
				_ = json.Unmarshal(v, &s)
				styles = append(styles, css+":"+s)
			}
		}
		for key, css := range map[string]string{"padding": "padding", "gap": "gap", "width": "width", "height": "height", "font_size": "font-size", "radius": "border-radius", "border": "border-width"} {
			if v, ok := p[key]; ok {
				styles = append(styles, css+":"+string(v)+"px")
			}
		}
		if _, ok := p["border"]; ok {
			styles = append(styles, "border-style:solid")
		}
		if v, ok := p["font_weight"]; ok {
			styles = append(styles, "font-weight:"+string(v))
		}
		if v, ok := p["line_height"]; ok {
			styles = append(styles, "line-height:"+string(v))
		}
		if _, ok := p["font_family"]; ok {
			styles = append(styles, "font-family:"+value(p, "font_family", "sans-serif"))
		}
		if id, ok := ref(n.Props["typography"]); ok {
			var v struct {
				FontFamily string  `json:"font_family"`
				FontSize   float64 `json:"font_size"`
				FontWeight int     `json:"font_weight"`
				LineHeight float64 `json:"line_height"`
			}
			_ = json.Unmarshal(tokens[id].Value, &v)
			styles = append(styles, fmt.Sprintf("font-family:%s;font-size:%gpx;font-weight:%d;line-height:%g", v.FontFamily, v.FontSize, v.FontWeight, v.LineHeight))
		}
		attrs := " data-element-id=\"" + n.ID + "\" style=\"" + html.EscapeString(strings.Join(styles, ";")) + "\""
		tag := "div"
		content := html.EscapeString(value(p, "text", ""))
		switch n.Type {
		case "text":
			tag = "span"
		case "button":
			tag = "button"
			attrs += " type=\"button\""
		case "input":
			tag = "input"
			attrs += " placeholder=\"" + html.EscapeString(value(p, "placeholder", value(p, "text", ""))) + "\" readonly"
		case "image":
			tag = "img"
			id := value(p, "asset_id", "")
			a, ok := assets[id]
			if !ok {
				return "", Invalid("invalid_ref", "asset_id", "нет private image bytes")
			}
			if a.MIME != "image/png" && a.MIME != "image/jpeg" {
				return "", Invalid("unsupported_property", "asset_id", "неподдерживаемый MIME")
			}
			attrs += " src=\"data:" + a.MIME + ";base64," + base64.StdEncoding.EncodeToString(a.Data) + "\" alt=\"" + html.EscapeString(value(p, "text", "")) + "\""
		}
		if string(p["disabled"]) == "true" {
			attrs += " disabled"
		}
		if tag == "img" || tag == "input" {
			return "<" + tag + attrs + ">", nil
		}
		for _, c := range n.Children {
			child, err := render(c, nil)
			if err != nil {
				return "", err
			}
			content += child
		}
		return "<" + tag + attrs + ">" + content + "</" + tag + ">", nil
	}
	// A handoff screenshot records the first named screen at its explicit viewport.
	// Every screen is preserved in the immutable model, never silently discarded.
	s := m.Screens[0]
	body, err := render(s.Root, nil)
	if err != nil {
		return "", 0, 0, err
	}
	return "<!doctype html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"Content-Security-Policy\" content=\"default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'\"><style>html,body{margin:0;width:100%;height:100%;overflow:hidden;font:16px sans-serif;color:#17212b;background:white}button,input{font:inherit}input{min-height:32px}button{cursor:default}</style></head><body>" + body + "</body></html>", s.Viewport.Width, s.Viewport.Height, nil
}

// ChromiumScreenshot never accepts arbitrary HTML or command-line arguments.
// The operator chooses the executable; each render has a fresh private profile,
// no scripts, a closed network and a mandatory Chromium sandbox.
func ChromiumScreenshot(ctx context.Context, binary string, m Model, assets map[string]ImageAsset) ([]byte, error) {
	select {
	case screenshotSlots <- struct{}{}:
		defer func() { <-screenshotSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !filepath.IsAbs(binary) {
		return nil, Invalid("screenshot_unavailable", "renderer", "нужен абсолютный путь Chromium")
	}
	page, w, h, err := RenderHTML(m, assets)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "rivet-design-render-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	input, output := filepath.Join(dir, "document.html"), filepath.Join(dir, "screenshot.png")
	if err = os.WriteFile(input, []byte(page), 0600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, "--headless", "--force-color-profile=srgb", "--disable-gpu", "--disable-gpu-shader-disk-cache", "--disable-gpu-compositing", "--disable-webgl", "--disable-webgl2", "--disable-features=Vulkan", "--disable-dev-shm-usage", "--disable-background-networking", "--disable-extensions", "--disable-sync", "--no-first-run", "--no-default-browser-check", "--disable-component-update", "--disable-default-apps", "--disable-javascript", "--host-resolver-rules=MAP * ~NOTFOUND", "--proxy-server=direct://", "--proxy-bypass-list=*", "--user-data-dir="+filepath.Join(dir, "profile"), "--screenshot="+output, "--window-size="+strconv.Itoa(w)+","+strconv.Itoa(h), "--hide-scrollbars", "--run-all-compositor-stages-before-draw", "--virtual-time-budget=1000", "file://"+input)
	// Browser diagnostics can contain local paths; do not expose them through API.
	cmd.Env = screenshotEnvironment(dir)
	cmd.Stdout = ioDiscard{}
	var diagnostics boundedChromeLog
	cmd.Stderr = &diagnostics
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("chromium screenshot failed: %w: %s", err, string(diagnostics.data))
	}
	f, err := os.Open(output)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > MaxAssetBytes {
		return nil, Invalid("size_limit", "screenshot", "PNG больше10МиБ")
	}
	cfg, err := png.DecodeConfig(f)
	if err != nil || cfg.Width != w || cfg.Height != h {
		return nil, Invalid("invalid_ref", "screenshot", "размер PNG не совпадает с viewport")
	}
	return os.ReadFile(output)
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

type boundedChromeLog struct{ data []byte }

func (b *boundedChromeLog) Write(p []byte) (int, error) {
	n := len(p)
	if n > 8192 {
		p = p[n-8192:]
	}
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = b.data[len(b.data)-8192:]
	}
	return n, nil
}

// A browser is not a control-plane process. Never inherit database/S3/SCM/model
// credentials, proxy credentials or dynamic-loader injection from rivetd.
func screenshotEnvironment(dir string) []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8", "TZ=UTC"}
	if runtime.GOOS == "linux" {
		return append(env, "HOME="+dir, "XDG_CACHE_HOME="+filepath.Join(dir, "cache"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "TMPDIR="+dir)
	}
	// macOS Chrome requires the system user's HOME for its platform lifecycle;
	// the browsing profile is nevertheless always the fresh --user-data-dir.
	if home, err := os.UserHomeDir(); err == nil {
		env = append(env, "HOME="+home)
	}
	return append(env, "TMPDIR="+os.TempDir())
}
