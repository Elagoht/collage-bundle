// Package bundle is a collage plugin that bundles an application's JavaScript,
// TypeScript and CSS with esbuild, and serves the output under content-hashed
// names.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{bundle.New(bundle.Options{
//			Dir:     "assets",
//			Entries: []string{"app.ts", "app.css"},
//		})},
//	})
//
// and the layout links what was built:
//
//	<link rel="stylesheet" href="{{bundle "app.css"}}">
//	<script src="{{bundle "app.js"}}" defer></script>
//
// The bundle is built once, into memory, when the application starts, and served
// from a mount at /_bundle/. Each file's name carries a hash of its content, so it
// is served to be kept for a year: an edit is a new name, and the old one is never
// asked for again. A static build copies the mount like any other.
//
// A build error stops a production server from starting. In development it is
// logged and served in place of the bundle — as a console.error in a script, a
// comment in a stylesheet — and every request checks whether a source changed, so
// an edit is rebuilt before the page that links it renders.
package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
	"github.com/evanw/esbuild/pkg/api"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/bundle"

// Options configures the plugin.
type Options struct {
	// Dir is the directory the sources are in, relative to the working
	// directory: "assets". Required.
	Dir string `json:"dir"`
	// Entries are the files to bundle, relative to Dir: "app.ts", "app.css".
	// Each is bundled with everything it imports. Required.
	Entries []string `json:"entries"`
	// Prefix is the URL prefix the output is served under. Default "/_bundle/".
	Prefix string `json:"prefix"`
	// Minify minifies the output. Default: on, except in development.
	Minify *bool `json:"minify"`
	// Sourcemap writes a source map beside each script and stylesheet, and links
	// it. Default: on in development only.
	Sourcemap *bool `json:"sourcemap"`
	// Target is the language level the output must run on: "es2018", "es2020",
	// "esnext". Default "esnext", which leaves the syntax as it was written.
	Target string `json:"target"`
}

// Plugin builds and serves the bundle.
type Plugin struct {
	opts       Options
	dev        bool
	configured bool
	logger     *slog.Logger

	dir    string // Dir, absolute
	minify bool
	maps   bool
	target api.Target

	// mu serialises builds; current is read without it, by every request.
	mu      sync.Mutex
	build   api.BuildContext
	stamp   string   // the sources' fingerprint the current output was built from
	inputs  []string // what the last successful build read, relative to dir
	current atomic.Pointer[output]
}

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string    { return Name }
func (p *Plugin) Version() string { return "0.1.2" }

// Shutdown releases esbuild's build context, which a development server keeps
// for its rebuilds.
func (p *Plugin) Shutdown(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.build != nil {
		p.build.Dispose()
		p.build = nil
	}
	return nil
}

var (
	_ collage.Plugin     = (*Plugin)(nil)
	_ collage.Configurer = (*Plugin)(nil)
)

// targets are the language levels Target accepts.
var targets = map[string]api.Target{
	"es2015": api.ES2015, "es2016": api.ES2016, "es2017": api.ES2017,
	"es2018": api.ES2018, "es2019": api.ES2019, "es2020": api.ES2020,
	"es2021": api.ES2021, "es2022": api.ES2022, "es2023": api.ES2023,
	"es2024": api.ES2024, "es2025": api.ES2025, "esnext": api.ESNext,
}

// sourceExts are the entries esbuild is given; anything else is refused at
// startup rather than handed to a loader that would guess.
var sourceExts = map[string]string{
	".js": ".js", ".mjs": ".js", ".cjs": ".js", ".jsx": ".js",
	".ts": ".js", ".mts": ".js", ".cts": ".js", ".tsx": ".js",
	".css": ".css",
}

// Configure reads and checks the configuration, and adds {{bundle}}. The build
// itself waits for Init: a template function has to exist before templates are
// parsed, but nothing needs the bundle before the application starts.
func (p *Plugin) Configure(_ context.Context, host collage.ConfigHost) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	p.dev = host.DevMode()
	p.logger = host.Logger()
	if err := p.check(); err != nil {
		return err
	}
	p.configured = true
	return host.AddTemplateFunc("bundle", p.URL)
}

func (p *Plugin) check() error {
	o := &p.opts
	if o.Dir == "" {
		return errors.New("bundle: Dir is required: the directory the sources are in")
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return fmt.Errorf("bundle: Dir: %w", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("bundle: Dir %q is not a directory", o.Dir)
	}
	p.dir = dir
	if len(o.Entries) == 0 {
		return errors.New("bundle: Entries is required: the files to bundle, relative to Dir")
	}
	seen := make(map[string]string)
	for _, entry := range o.Entries {
		clean := path.Clean(filepath.ToSlash(entry))
		if entry == "" || !fs.ValidPath(clean) {
			return fmt.Errorf("bundle: entry %q must be a path inside Dir", entry)
		}
		ext := sourceExts[path.Ext(clean)]
		if ext == "" {
			return fmt.Errorf("bundle: entry %q is not JavaScript, TypeScript or CSS", entry)
		}
		// Two entries that would produce one output name — app.ts and app.js —
		// would leave {{bundle "app.js"}} meaning whichever was built last.
		out := strings.TrimSuffix(clean, path.Ext(clean)) + ext
		if other, ok := seen[out]; ok {
			return fmt.Errorf("bundle: entries %q and %q would both be %q", other, entry, out)
		}
		seen[out] = entry
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(clean))); err != nil || info.IsDir() {
			return fmt.Errorf("bundle: entry %q is not a file in %q", entry, o.Dir)
		}
	}
	if o.Prefix == "" {
		o.Prefix = "/_bundle/"
	}
	if !strings.HasPrefix(o.Prefix, "/") || !strings.HasSuffix(o.Prefix, "/") || o.Prefix == "/" {
		return fmt.Errorf("bundle: Prefix %q must begin and end with / and not be / alone", o.Prefix)
	}
	p.minify = !p.dev
	if o.Minify != nil {
		p.minify = *o.Minify
	}
	p.maps = p.dev
	if o.Sourcemap != nil {
		p.maps = *o.Sourcemap
	}
	target := strings.ToLower(o.Target)
	if target == "" {
		target = "esnext"
	}
	t, ok := targets[target]
	if !ok {
		return fmt.Errorf("bundle: Target %q is none of es2015 to es2025, esnext", o.Target)
	}
	p.target = t
	return nil
}

// Init builds the bundle and mounts it. A production server with a bundle that
// does not build does not start.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if !p.configured {
		return errors.New("bundle: register the plugin in Config.Plugins, where Configure runs; {{bundle}} needs it")
	}
	ctx, cerr := api.Context(p.buildOptions())
	if cerr != nil {
		return fmt.Errorf("bundle: %w", cerr)
	}
	p.mu.Lock()
	p.build = ctx
	p.stamp = p.fingerprint(nil)
	err := p.rebuild()
	if !p.dev {
		// Nothing rebuilds outside development, so esbuild's context has
		// nothing left to do.
		p.build.Dispose()
		p.build = nil
	}
	p.mu.Unlock()
	if err != nil && !p.dev {
		return err
	}
	return host.Mount(p.opts.Prefix, &memFS{plugin: p},
		collage.WithCacheControl("public, max-age=31536000, immutable"))
}

func (p *Plugin) buildOptions() api.BuildOptions {
	entries := make([]string, len(p.opts.Entries))
	for i, e := range p.opts.Entries {
		entries[i] = path.Clean(filepath.ToSlash(e))
	}
	opts := api.BuildOptions{
		AbsWorkingDir:     p.dir,
		EntryPoints:       entries,
		Bundle:            true,
		Write:             false,
		Metafile:          true,
		Outdir:            outDir,
		Outbase:           ".",
		EntryNames:        "[dir]/[name]-[hash]",
		AssetNames:        "assets/[name]-[hash]",
		PublicPath:        p.opts.Prefix,
		Platform:          api.PlatformBrowser,
		Target:            p.target,
		MinifyWhitespace:  p.minify,
		MinifyIdentifiers: p.minify,
		MinifySyntax:      p.minify,
		LogLevel:          api.LogLevelSilent,
		// What a stylesheet or a script refers to by url() or import is copied
		// beside the bundle under a hashed name, rather than refused.
		Loader: map[string]api.Loader{
			".png": api.LoaderFile, ".jpg": api.LoaderFile, ".jpeg": api.LoaderFile,
			".gif": api.LoaderFile, ".svg": api.LoaderFile, ".webp": api.LoaderFile,
			".avif": api.LoaderFile, ".ico": api.LoaderFile,
			".woff": api.LoaderFile, ".woff2": api.LoaderFile, ".ttf": api.LoaderFile,
			".otf": api.LoaderFile, ".eot": api.LoaderFile,
		},
	}
	if p.maps {
		opts.Sourcemap = api.SourceMapLinked
	}
	return opts
}

// outDir is where esbuild is told the output goes. Nothing is written there; it
// only anchors the output's names.
const outDir = "out"

// output is one build: the files, and which served name each entry's output has.
type output struct {
	files map[string][]byte // served name → content
	names map[string]string // name as linked, "app.js" → served name
	// failed is set when the build failed in development; the error files then
	// stand in for every name.
	failed  bool
	errJS   string
	errCSS  string
	modTime time.Time
}

// URL is {{bundle}}: the served URL of an output, by the name it would have
// without its hash — "app.js" for app.ts, "app.css" — or by the entry's own name.
func (p *Plugin) URL(name string) (string, error) {
	if p.dev {
		p.refresh()
	}
	out := p.current.Load()
	if out == nil {
		return "", errors.New("bundle: the bundle has not been built; the application has not started")
	}
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	if out.failed {
		if path.Ext(clean) == ".css" {
			return p.opts.Prefix + out.errCSS, nil
		}
		return p.opts.Prefix + out.errJS, nil
	}
	if served, ok := out.names[clean]; ok {
		return p.opts.Prefix + served, nil
	}
	// The entry's own name: "app.ts" is linked as what it was built into.
	if ext, ok := sourceExts[path.Ext(clean)]; ok {
		if served, ok := out.names[strings.TrimSuffix(clean, path.Ext(clean))+ext]; ok {
			return p.opts.Prefix + served, nil
		}
	}
	known := make([]string, 0, len(out.names))
	for n := range out.names {
		known = append(known, n)
	}
	sort.Strings(known)
	return "", fmt.Errorf("bundle: no output named %q; there are %s", name, strings.Join(known, ", "))
}

// refresh rebuilds when a source changed since the last build. It is called on
// every request in development — by {{bundle}}, and by every read of the mount,
// which is also how collage's reload script learns that the page should reload.
func (p *Plugin) refresh() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.build == nil {
		return
	}
	stamp := p.fingerprint(p.inputs)
	if stamp == p.stamp {
		return
	}
	p.stamp = stamp
	_ = p.rebuild() // logged by rebuild; development serves the error instead
}

// rebuild runs esbuild and publishes what it produced. It must be called with mu
// held.
func (p *Plugin) rebuild() error {
	result := p.build.Rebuild()
	now := time.Now().Truncate(time.Second)
	if len(result.Errors) > 0 {
		msgs := api.FormatMessages(result.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
		text := strings.TrimSpace(strings.Join(msgs, "\n"))
		err := fmt.Errorf("bundle: the build failed:\n%s", text)
		if p.dev {
			p.logger.Error("bundle: the build failed", "error", text)
			p.current.Store(failedOutput(text, now))
			p.inputs = nil
		}
		return err
	}
	out, inputs, err := collect(result)
	if err != nil {
		if p.dev {
			p.logger.Error("bundle: the build failed", "error", err)
			p.current.Store(failedOutput(err.Error(), now))
		}
		return err
	}
	out.modTime = now
	p.inputs = inputs
	// The fingerprint taken before the build did not know what the build would
	// read from outside Dir; take it again now that it does.
	p.stamp = p.fingerprint(inputs)
	p.current.Store(out)
	return nil
}

// metafile is the part of esbuild's metafile the plugin reads.
type metafile struct {
	Inputs  map[string]json.RawMessage `json:"inputs"` // json.RawMessage: only the keys are read
	Outputs map[string]struct {
		EntryPoint string `json:"entryPoint"`
		CSSBundle  string `json:"cssBundle"`
	} `json:"outputs"`
}

// collect turns esbuild's output into served names, and names each entry's
// output by what it would be called without a hash.
func collect(result api.BuildResult) (*output, []string, error) {
	var meta metafile
	if err := json.Unmarshal([]byte(result.Metafile), &meta); err != nil {
		return nil, nil, fmt.Errorf("bundle: esbuild's metafile: %w", err)
	}
	out := &output{files: make(map[string][]byte), names: make(map[string]string)}
	for _, f := range result.OutputFiles {
		// esbuild reports absolute paths; what is served is the path under outDir.
		idx := strings.LastIndex(filepath.ToSlash(f.Path), "/"+outDir+"/")
		if idx < 0 {
			return nil, nil, fmt.Errorf("bundle: esbuild wrote %q outside its output directory", f.Path)
		}
		out.files[filepath.ToSlash(f.Path)[idx+len(outDir)+2:]] = f.Contents
	}
	served := func(metaPath string) string { return strings.TrimPrefix(metaPath, outDir+"/") }
	for outPath, o := range meta.Outputs {
		if o.EntryPoint == "" {
			continue
		}
		stem := strings.TrimSuffix(o.EntryPoint, path.Ext(o.EntryPoint))
		out.names[stem+path.Ext(outPath)] = served(outPath)
		if o.CSSBundle != "" {
			// A script that imports CSS gets a stylesheet beside it, linked as
			// the script's own name with .css.
			if other, ok := out.names[stem+".css"]; ok && other != served(o.CSSBundle) {
				return nil, nil, fmt.Errorf("bundle: %q imports CSS, and another entry is also %q", o.EntryPoint, stem+".css")
			}
			out.names[stem+".css"] = served(o.CSSBundle)
		}
	}
	inputs := make([]string, 0, len(meta.Inputs))
	for in := range meta.Inputs {
		inputs = append(inputs, in)
	}
	sort.Strings(inputs)
	return out, inputs, nil
}

// failedOutput is what development serves while the build is broken: a script
// that reports the error in the console, and a stylesheet that shows it over the
// page, for every name a template links.
func failedOutput(text string, now time.Time) *output {
	message := "collage-bundle: the build failed:\n" + text
	quoted, _ := json.Marshal(message)
	js := []byte("console.error(" + string(quoted) + ");\n")
	// A CSS string cannot hold a raw newline; \A is its line break.
	cssText := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\A `, "*/", "* /").Replace(message)
	css := []byte("/* " + strings.ReplaceAll(message, "*/", "* /") + " */\n" +
		`html::before{content:"` + cssText + `";white-space:pre-wrap;display:block;` +
		"padding:1em;background:#fee;color:#900;font:14px/1.4 monospace}\n")
	sum := sha256.Sum256([]byte(message))
	hash := hex.EncodeToString(sum[:])[:16]
	out := &output{
		files:   make(map[string][]byte),
		names:   map[string]string{},
		failed:  true,
		errJS:   "error-" + hash + ".js",
		errCSS:  "error-" + hash + ".css",
		modTime: now,
	}
	out.files[out.errJS] = js
	out.files[out.errCSS] = css
	return out
}

// fingerprint summarises the sources by name, size and modification time: every
// file under Dir, and the inputs the last build read from elsewhere — a package
// in node_modules. Must be called with mu held.
func (p *Plugin) fingerprint(inputs []string) string {
	h := sha256.New()
	_ = filepath.WalkDir(p.dir, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			fmt.Fprintf(h, "%s:%d:%d\n", file, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	for _, in := range inputs {
		file := filepath.Join(p.dir, filepath.FromSlash(in))
		if strings.HasPrefix(file, p.dir+string(filepath.Separator)) && !strings.Contains(file, "node_modules") {
			continue // walked above
		}
		if info, err := os.Stat(file); err == nil {
			fmt.Fprintf(h, "%s:%d:%d\n", file, info.Size(), info.ModTime().UnixNano())
		} else {
			fmt.Fprintf(h, "%s:gone\n", file)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- the served file system ------------------------------------------------------

// memFS serves the current output. A development rebuild replaces the output as a
// whole, so a request reads one build or the next, never half of each.
type memFS struct{ plugin *Plugin }

var (
	_ fs.FS        = (*memFS)(nil)
	_ fs.ReadDirFS = (*memFS)(nil)
)

func (m *memFS) load() *output {
	if m.plugin.dev {
		m.plugin.refresh()
	}
	if out := m.plugin.current.Load(); out != nil {
		return out
	}
	return &output{files: map[string][]byte{}}
}

// Open opens a file, or a directory for a static build's walk.
func (m *memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	out := m.load()
	if data, ok := out.files[name]; ok {
		return &memFile{info: fileInfo{name: path.Base(name), size: int64(len(data)), modTime: out.modTime}, r: bytes.NewReader(data)}, nil
	}
	entries, ok := out.readDir(name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memDir{info: fileInfo{name: path.Base(name), dir: true, modTime: out.modTime}, entries: entries}, nil
}

// ReadDir lists a directory of the output.
func (m *memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	entries, ok := m.load().readDir(name)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return entries, nil
}

// readDir lists the files and directories directly inside dir, sorted by name.
func (o *output) readDir(dir string) ([]fs.DirEntry, bool) {
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	found := dir == "."
	seen := make(map[string]bool)
	var entries []fs.DirEntry
	for name, data := range o.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		found = true
		rest := name[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			sub := rest[:i]
			if !seen[sub] {
				seen[sub] = true
				entries = append(entries, fs.FileInfoToDirEntry(fileInfo{name: sub, dir: true, modTime: o.modTime}))
			}
			continue
		}
		entries = append(entries, fs.FileInfoToDirEntry(fileInfo{name: rest, size: int64(len(data)), modTime: o.modTime}))
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, found
}

type fileInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (f fileInfo) Name() string       { return f.name }
func (f fileInfo) Size() int64        { return f.size }
func (f fileInfo) ModTime() time.Time { return f.modTime }
func (f fileInfo) IsDir() bool        { return f.dir }
func (f fileInfo) Sys() any           { return nil } // any: fs.FileInfo's own signature
func (f fileInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}

// memFile is an open file: a reader over the build's bytes, seekable, which is
// what collage's mount needs for Range.
type memFile struct {
	info fileInfo
	r    *bytes.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error)                   { return f.info, nil }
func (f *memFile) Read(b []byte) (int, error)                   { return f.r.Read(b) }
func (f *memFile) Seek(offset int64, whence int) (int64, error) { return f.r.Seek(offset, whence) }
func (f *memFile) Close() error                                 { return nil }

type memDir struct {
	info    fileInfo
	entries []fs.DirEntry
	offset  int
}

func (d *memDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: errors.New("is a directory")}
}
func (d *memDir) Close() error { return nil }

// ReadDir follows fs.ReadDirFile's contract: n <= 0 returns the rest at once.
func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.offset:]
	if n <= 0 {
		d.offset = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if n > len(rest) {
		n = len(rest)
	}
	d.offset += n
	return rest[:n], nil
}
