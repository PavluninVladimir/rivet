package design

import (
	"bytes"
	"context"
	"encoding/json"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

const screenID = "11111111-1111-4111-8111-111111111111"
const rootID = "22222222-2222-4222-8222-222222222222"
const buttonID = "33333333-3333-4333-8333-333333333333"
const tokenID = "44444444-4444-4444-8444-444444444444"

func fixture() Model {
	return Model{Schema: Schema, Tokens: []Token{{ID: tokenID, Name: "space", Type: "spacing", Value: json.RawMessage(`{"px":12}`)}}, Components: []Component{}, Screens: []Screen{{ID: screenID, Name: "Main", Viewport: Viewport{320, 240}, Root: Node{ID: rootID, Type: "container", Props: Properties{"padding": json.RawMessage(`{"token_id":"` + tokenID + `"}`), "width": json.RawMessage(`320`), "height": json.RawMessage(`240`), "background": json.RawMessage(`"#102030"`)}, Children: []Node{{ID: buttonID, Type: "button", Props: Properties{"text": json.RawMessage(`"Сохранить"`), "padding": json.RawMessage(`{"token_id":"` + tokenID + `"}`), "width": json.RawMessage(`160`), "height": json.RawMessage(`80`), "background": json.RawMessage(`"#3498db"`), "border": json.RawMessage(`0`), "radius": json.RawMessage(`0`)}, Children: []Node{}}}}}}, Overrides: []Override{}}
}
func TestModelStrictHashSecurity(t *testing.T) {
	m := fixture()
	raw, sha, e := Canonical(m)
	if e != nil {
		t.Fatal(e)
	}
	parsed, e := Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	raw2, sha2, e := Canonical(parsed)
	if e != nil || sha != sha2 || !bytes.Equal(raw, raw2) {
		t.Fatal("unstable hash")
	}
	for _, bad := range []string{strings.Replace(string(raw), `"schema":"rivet.design/v1"`, `"schema":"react/v1"`, 1), strings.Replace(string(raw), `"type":"button"`, `"type":"script"`, 1), strings.Replace(string(raw), `"text":"Сохранить"`, `"onclick":"alert(1)"`, 1), strings.Replace(string(raw), `"text":"Сохранить"`, `"color":"url(https://evil.test)"`, 1), strings.Replace(string(raw), `"text":"Сохранить"`, `"asset_id":"https://evil.test"`, 1), strings.Replace(string(raw), `"schema":"rivet.design/v1"`, `"schema":"rivet.design/v1","unknown":"lost"`, 1)} {
		if _, e := Parse([]byte(bad)); e == nil {
			t.Errorf("accepted unsafe import %s", bad)
		}
	}
	m.Screens[0].Root.Children[0].ID = rootID
	if e := Validate(m); e == nil {
		t.Fatal("duplicate ID accepted")
	}
}
func TestPatchesImpactAndUndo(t *testing.T) {
	m := fixture()
	after, d, e := Apply(m, 1, []Patch{{Op: "set_property", Scope: "instance", ElementID: buttonID, Property: "padding", Value: json.RawMessage(`24`)}})
	if e != nil {
		t.Fatal(e)
	}
	if len(d.AffectedElementIDs) != 1 || d.AffectedElementIDs[0] != buttonID {
		t.Fatal(d)
	}
	if string(after.Tokens[0].Value) != `{"px":12}` {
		t.Fatal("local edit changed token")
	}
	shared, d, e := Apply(m, 1, []Patch{{Op: "set_token", TokenID: tokenID, Value: json.RawMessage(`{"px":20}`)}})
	if e != nil || len(d.AffectedElementIDs) != 2 {
		t.Fatal(d, e)
	}
	restored, e := Inverse(shared, m, shared)
	if e != nil || !same(restored, m) {
		t.Fatal("undo failed", e)
	}
	if _, e := Inverse(after, m, shared); e == nil {
		t.Fatal("conflicting undo accepted")
	}
}
func TestComponentCycleAndLimit(t *testing.T) {
	m := fixture()
	a := "55555555-5555-4555-8555-555555555555"
	b := "66666666-6666-4666-8666-666666666666"
	m.Components = []Component{{ID: a, Name: "A", Root: Node{ID: b, Type: "instance", ComponentID: a, Props: Properties{}, Children: []Node{}}, Variants: []Variant{}}}
	if e := Validate(m); e == nil {
		t.Fatal("component cycle accepted")
	}
	m = fixture()
	m.Screens[0].Root.Props["height"] = json.RawMessage(`999999999`)
	if e := Validate(m); e == nil {
		t.Fatal("unbounded dimension")
	}
}
func TestRenderEscapesAndNoNetwork(t *testing.T) {
	m := fixture()
	m.Screens[0].Root.Children[0].Props["text"] = json.RawMessage(`"<img src=https://evil.test onerror=alert(1)>"`)
	html, _, _, err := RenderHTML(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "<img src=https") || strings.Contains(html, "<script") {
		t.Fatal("executable markup")
	}
	if !strings.Contains(html, "&lt;img") || !strings.Contains(html, "default-src 'none'") {
		t.Fatal("not escaped or CSP missing")
	}
}
func TestChromiumRealScreenshot(t *testing.T) {
	binary := os.Getenv("RIVET_DESIGN_CHROMIUM_TEST")
	if binary == "" {
		t.Skip("RIVET_DESIGN_CHROMIUM_TEST selects real sandboxed fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := ChromiumScreenshot(ctx, binary, fixture(), nil)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil || img.Bounds().Dx() != 320 || img.Bounds().Dy() != 240 {
		t.Fatal("invalid complete PNG", err)
	}
	for _, p := range []struct {
		x, y int
		want color.NRGBA
	}{{2, 2, color.NRGBA{R: 0x10, G: 0x20, B: 0x30, A: 255}}, {30, 30, color.NRGBA{R: 0x34, G: 0x98, B: 0xdb, A: 255}}, {310, 230, color.NRGBA{R: 0x10, G: 0x20, B: 0x30, A: 255}}} {
		got := color.NRGBAModel.Convert(img.At(p.x, p.y)).(color.NRGBA)
		if got != p.want {
			t.Fatalf("pixel (%d,%d): got %#v want %#v; blank/incomplete screenshot is not accepted", p.x, p.y, got, p.want)
		}
	}
}

func TestScreenshotEnvironmentDoesNotInheritControlPlaneSecrets(t *testing.T) {
	t.Setenv("RIVET_DATABASE_URL", "postgres://private:database-canary@example")
	t.Setenv("RIVET_S3_SECRET_KEY", "s3-secret-canary")
	t.Setenv("ANTHROPIC_API_KEY", "model-secret-canary")
	t.Setenv("HTTP_PROXY", "http://proxy:proxy-secret-canary@example")
	t.Setenv("DYLD_INSERT_LIBRARIES", "injected-library-canary")
	env := strings.Join(screenshotEnvironment("/tmp/private-renderer"), "\n")
	for _, bad := range []string{"RIVET_", "ANTHROPIC_", "HTTP_PROXY", "DYLD_", "canary"} {
		if strings.Contains(env, bad) {
			t.Fatalf("browser inherited %s", bad)
		}
	}
}
