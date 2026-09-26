package bundle_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	bundle "github.com/Elagoht/collage-bundle"
	"github.com/Elagoht/collage/pkg/collage"
)

// sources writes files into a fresh directory and returns it.
func sources(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		write(t, filepath.Join(dir, name), content)
	}
	return dir
}

func write(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// A later mtime than anything written before it, whatever the file
	// system's timestamp resolution.
	later := time.Now().Add(time.Duration(len(content)+1) * time.Second)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
}

const layout = `<html><head><link rel="stylesheet" href="{{bundle "app.css"}}"></head>` +
	`<body><script src="{{bundle "app.js"}}"></script></body></html>`

func app(t *testing.T, dev bool, tmpl string, p *bundle.Plugin) *collage.App {
	t.Helper()
	a, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		DevMode:  dev,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(tmpl)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterPage(collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Static().Build()); err != nil {
		t.Fatal(err)
	}
	return a
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

var linked = regexp.MustCompile(`(?:href|src)="(/_bundle/[^"]+)"`)

// links returns the bundle URLs a page links, in order.
func links(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, m := range linked.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestBuildsAndServesHashedNames(t *testing.T) {
	dir := sources(t, map[string]string{
		"app.ts":    "import { greet } from './greet'\nconst who: string = 'world'\nconsole.log(greet(who))\n",
		"greet.ts":  "export function greet(name: string): string { return 'hello ' + name }\n",
		"app.css":   "@import './base.css';\n.a { color: red }\n",
		"base.css":  "body { margin: 0 }\n",
		"notes.txt": "not an entry",
	})
	h := app(t, false, layout, bundle.New(bundle.Options{Dir: dir, Entries: []string{"app.ts", "app.css"}})).Handler()
	page := get(h, "/")
	if page.Code != http.StatusOK {
		t.Fatalf("page = %d %s", page.Code, page.Body)
	}
	urls := links(t, page.Body.String())
	if len(urls) != 2 || !regexp.MustCompile(`^/_bundle/app-[A-Z0-9]+\.css$`).MatchString(urls[0]) ||
		!regexp.MustCompile(`^/_bundle/app-[A-Z0-9]+\.js$`).MatchString(urls[1]) {
		t.Fatalf("links = %v", urls)
	}

	js := get(h, urls[1])
	if js.Code != http.StatusOK {
		t.Fatalf("script = %d", js.Code)
	}
	body := js.Body.String()
	if !strings.Contains(body, "hello ") || strings.Contains(body, ": string") || strings.Contains(body, "\n  ") {
		t.Errorf("script is not the bundled, minified TypeScript: %q", body)
	}
	if cc := js.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if ct := js.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	css := get(h, urls[0]).Body.String()
	if !strings.Contains(css, "margin:0") || !strings.Contains(css, "color:red") {
		t.Errorf("stylesheet = %q", css)
	}
	if rec := get(h, "/_bundle/app.js"); rec.Code != http.StatusNotFound {
		t.Errorf("an unhashed name = %d, want 404", rec.Code)
	}
}

// {{bundle}} also takes the entry's own name, and a script importing CSS has a
// stylesheet linked by the script's name.
func TestEntryNamesAndImportedCSS(t *testing.T) {
	dir := sources(t, map[string]string{
		"js/main.tsx": "import './main.css'\nexport const n: number = 1\nconsole.log(n)\n",
		"js/main.css": ".b { color: blue }\n",
	})
	tmpl := `<link href="{{bundle "js/main.css"}}"><script src="{{bundle "js/main.tsx"}}"></script>`
	h := app(t, false, tmpl, bundle.New(bundle.Options{Dir: dir, Entries: []string{"js/main.tsx"}})).Handler()
	urls := links(t, get(h, "/").Body.String())
	if len(urls) != 2 || !strings.HasPrefix(urls[0], "/_bundle/js/main-") || !strings.HasSuffix(urls[0], ".css") ||
		!strings.HasSuffix(urls[1], ".js") {
		t.Fatalf("links = %v", urls)
	}
	if css := get(h, urls[0]).Body.String(); !strings.Contains(css, "color:#00f") && !strings.Contains(css, "color:blue") {
		t.Errorf("stylesheet = %q", css)
	}
}

// A URL a stylesheet refers to is copied beside the bundle and linked under the
// prefix.
func TestReferencedFiles(t *testing.T) {
	dir := sources(t, map[string]string{
		"app.css":   ".c { background: url(./dot.png) }\n",
		"dot.png":   "\x89PNG fake",
		"unused.js": "x",
	})
	h := app(t, false, `<link href="{{bundle "app.css"}}">`, bundle.New(bundle.Options{Dir: dir, Entries: []string{"app.css"}})).Handler()
	css := get(h, links(t, get(h, "/").Body.String())[0]).Body.String()
	m := regexp.MustCompile(`url\("?(/_bundle/assets/dot-[A-Z0-9]+\.png)"?\)`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("stylesheet = %q", css)
	}
	if rec := get(h, m[1]); rec.Code != http.StatusOK || rec.Body.String() != "\x89PNG fake" {
		t.Errorf("image = %d %q", rec.Code, rec.Body.String())
	}
}

func TestUnknownNameFailsTheRender(t *testing.T) {
	dir := sources(t, map[string]string{"app.js": "console.log(1)\n"})
	h := app(t, false, `<script src="{{bundle "nope.js"}}"></script>`, bundle.New(bundle.Options{Dir: dir, Entries: []string{"app.js"}})).Handler()
	if rec := get(h, "/"); rec.Code != http.StatusInternalServerError {
		t.Errorf("page = %d, want 500", rec.Code)
	}
}

// A production server with a bundle that does not build, or a configuration
// that is wrong, does not start.
func TestRefusals(t *testing.T) {
	good := sources(t, map[string]string{"app.ts": "console.log(1)\n", "app.js": "x", "readme.md": "x"})
	broken := sources(t, map[string]string{"app.ts": "const x = ;\n"})
	for name, opts := range map[string]bundle.Options{
		"build error":     {Dir: broken, Entries: []string{"app.ts"}},
		"missing import":  {Dir: sources(t, map[string]string{"app.ts": "import './gone'\n"}), Entries: []string{"app.ts"}},
		"no dir":          {Entries: []string{"app.ts"}},
		"dir not there":   {Dir: filepath.Join(good, "nope"), Entries: []string{"app.ts"}},
		"no entries":      {Dir: good},
		"entry not there": {Dir: good, Entries: []string{"other.ts"}},
		"entry escapes":   {Dir: good, Entries: []string{"../app.ts"}},
		"not a source":    {Dir: good, Entries: []string{"readme.md"}},
		"same output":     {Dir: good, Entries: []string{"app.ts", "app.js"}},
		"bad target":      {Dir: good, Entries: []string{"app.ts"}, Target: "es3"},
		"bad prefix":      {Dir: good, Entries: []string{"app.ts"}, Prefix: "bundle"},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := collage.New(&collage.Config{
				Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
				Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`x`)}}, Root: "t"},
				Plugins:  []collage.Plugin{bundle.New(opts)},
			})
			if err != nil {
				return // refused already, in Configure
			}
			if rec := get(a.Handler(), "/"); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("started anyway: %d", rec.Code)
			}
		})
	}
}

// In development the output is readable and mapped, a build error is served
// rather than fatal, and an edit is rebuilt before the next render.
func TestDevelopment(t *testing.T) {
	dir := sources(t, map[string]string{"app.ts": "const who: string = 'first'\nconsole.log(who)\n"})
	var logged bytes.Buffer
	p := bundle.New(bundle.Options{Dir: dir, Entries: []string{"app.ts"}})
	a, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		DevMode:  true,
		Logger:   slog.New(slog.NewTextHandler(&logged, nil)),
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<script src="{{bundle "app.js"}}"></script>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterPage(collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Build()); err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	h := a.Handler()

	script := func() (string, *httptest.ResponseRecorder) {
		t.Helper()
		urls := links(t, get(h, "/").Body.String())
		if len(urls) != 1 {
			t.Fatalf("links = %v", urls)
		}
		return urls[0], get(h, urls[0])
	}

	first, rec := script()
	body := rec.Body.String()
	if !strings.Contains(body, `"first"`) || !strings.Contains(body, "\n") {
		t.Errorf("development script is minified or wrong: %q", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("development Cache-Control = %q", cc)
	}
	m := regexp.MustCompile(`sourceMappingURL=(\S+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no source map linked: %q", body)
	}
	if mp := get(h, m[1]); mp.Code != http.StatusOK || !strings.Contains(mp.Body.String(), "app.ts") {
		t.Errorf("source map %s = %d %q", m[1], mp.Code, mp.Body.String())
	}

	write(t, filepath.Join(dir, "app.ts"), "const = ;\n")
	broken, rec := script()
	if broken == first || rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "console.error(") ||
		!strings.Contains(rec.Body.String(), "build failed") {
		t.Errorf("a build error is served as %s %d %q", broken, rec.Code, rec.Body.String())
	}
	if !strings.Contains(logged.String(), "the build failed") {
		t.Errorf("the build error was not logged: %s", logged.String())
	}

	write(t, filepath.Join(dir, "app.ts"), "const who: string = 'second, fixed'\nconsole.log(who)\n")
	fixed, rec := script()
	if fixed == first || fixed == broken || !strings.Contains(rec.Body.String(), `"second, fixed"`) {
		t.Errorf("the edit was not rebuilt: %s %q", fixed, rec.Body.String())
	}
	if old := get(h, first); old.Code != http.StatusNotFound {
		t.Errorf("the first build's name = %d, want 404", old.Code)
	}
}

// The development reload script watches the mount; reading it after an edit is
// what rebuilds, so the page reloads with the edit in it.
func TestDevelopmentMountSeesEdits(t *testing.T) {
	dir := sources(t, map[string]string{"app.js": "console.log('one')\n"})
	p := bundle.New(bundle.Options{Dir: dir, Entries: []string{"app.js"}})
	a := app(t, true, `<script src="{{bundle "app.js"}}"></script>`, p)
	defer p.Shutdown(context.Background())
	a.Handler()
	var fsys fs.FS
	for _, m := range a.Mounts() {
		if m.Prefix() == "/_bundle/" {
			fsys = m.FS()
		}
	}
	if fsys == nil {
		t.Fatal("no mount at /_bundle/")
	}
	names := func() string {
		var out []string
		err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
			out = append(out, name)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, ",")
	}
	before := names()
	write(t, filepath.Join(dir, "app.js"), "console.log('two, longer')\n")
	if after := names(); after == before {
		t.Errorf("the mount did not change after an edit: %v", after)
	}
}

// A static build copies the bundle into its output, where the built pages link
// it.
func TestStaticBuild(t *testing.T) {
	dir := sources(t, map[string]string{"app.ts": "console.log('built')\n", "app.css": "p{color:red}\n"})
	a := app(t, false, layout, bundle.New(bundle.Options{Dir: dir, Entries: []string{"app.ts", "app.css"}}))
	out := t.TempDir()
	b, err := collage.NewBuilder(a, collage.BuildOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	urls := links(t, string(html))
	if len(urls) != 2 {
		t.Fatalf("links = %v", urls)
	}
	for _, u := range urls {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(u))); err != nil {
			t.Errorf("%s was not copied: %v", u, err)
		}
	}
}

// Configuration overlays the options, as any plugin's does.
func TestConfiguration(t *testing.T) {
	dir := sources(t, map[string]string{"app.ts": "const long_name_here: number = 1\nconsole.log(long_name_here)\n"})
	conf, err := json.Marshal(struct {
		Dir    string `json:"dir"`
		Minify bool   `json:"minify"`
		Prefix string `json:"prefix"`
		Target string `json:"target"`
	}{dir, false, "/js/", "es2015"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := collage.New(&collage.Config{
		Server:       collage.ServerConfig{Host: "localhost", Port: 3000},
		Template:     collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<script src="{{bundle "app.js"}}"></script>`)}}, Root: "t"},
		Plugins:      []collage.Plugin{bundle.New(bundle.Options{Entries: []string{"app.ts"}})},
		PluginConfig: map[string]json.RawMessage{bundle.Name: conf},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterPage(collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Build()); err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	m := regexp.MustCompile(`src="(/js/app-[A-Z0-9]+\.js)"`).FindStringSubmatch(get(h, "/").Body.String())
	if m == nil {
		t.Fatal("the script is not under the configured prefix")
	}
	body := get(h, m[1]).Body.String()
	if !strings.Contains(body, "long_name_here") || !strings.Contains(body, "var ") {
		t.Errorf("minify off and es2015 were not applied: %q", body)
	}
}
