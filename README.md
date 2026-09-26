# elagoht/bundle

A collage plugin that bundles an application's JavaScript, TypeScript and CSS
with [esbuild](https://esbuild.github.io) when it starts, serves the output under
content-hashed names, and rebuilds on an edit in development.

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{bundle.New(bundle.Options{
		Dir:     "assets",
		Entries: []string{"app.ts", "app.css"},
	})},
})
```

```html
<link rel="stylesheet" href="{{bundle "app.css"}}">
<script src="{{bundle "app.js"}}" defer></script>
```

Requires collage v0.23.0 or later. Register it in `Config.Plugins`: it adds a
template function, which only a plugin registered there can.

## What it does

Each entry is bundled with everything it imports — TypeScript and JSX compiled,
`@import`ed stylesheets inlined — into memory, when the application starts. The
output is served from a mount at `Prefix` (`/_bundle/`), each file under a name
carrying esbuild's hash of its content:

```html
<script src="/_bundle/app-5KQ3ZJ2M.js" defer></script>
```

and served with `Cache-Control: public, max-age=31536000, immutable`. The hash is
what makes that honest: an edit is a new name, the page links it, and the old name
is never asked for again. The unhashed name is not served at all, so nothing can
link a file that cannot be cached.

`{{bundle}}` takes the name the output would have without its hash — `"app.js"`
for `app.ts`, `"app.css"` — or the entry's own name, `"app.ts"`. A name the build
did not produce fails the render, as `{{asset}}` does for a missing file: a page
linking a script that 404s is broken while reporting itself as fine.

A script that imports CSS gets a stylesheet beside it, linked by the script's
name with `.css`: `app.ts` importing `./app.css` is `{{bundle "app.css"}}`. Images
and fonts a stylesheet or a script refers to — `url(./logo.png)` — are copied under
`Prefix` + `assets/`, hashed the same way, and the reference rewritten to them.

A static build copies the mount like any other, so a built site holds the bundle
its pages link.

## Build errors

A production server whose bundle does not build does not start: `Handler()`
answers `503` and the error is logged, `ListenAndServe` returns it. A broken build
is found when it is deployed, not by the first visitor.

In development the error is logged and served in place of the bundle: every
script `{{bundle}}` links reports it with `console.error`, and every stylesheet
shows it over the page. The server keeps running, so the fix is one save away.

## Development

With `DevMode` on, every request checks whether a source changed — every file
under `Dir`, and anything the last build read from outside it, a package in
`node_modules` — by name, size and modification time, and rebuilds before the page
renders if one did, with esbuild's incremental rebuild. collage's live-reload
script watches the mount, so the page reloads itself with the edit in it.

The output is not minified in development and carries linked source maps, so the
browser's developer tools show the sources as written.

## Options

| Option | Default | |
| --- | --- | --- |
| `Dir` | required | The directory the sources are in, relative to the working directory |
| `Entries` | required | The files to bundle, relative to `Dir`: `.js`, `.mjs`, `.jsx`, `.ts`, `.tsx`, `.css` and the like |
| `Prefix` | `/_bundle/` | The URL prefix the output is served under |
| `Minify` | on, off in development | Minify whitespace, identifiers and syntax |
| `Sourcemap` | on in development only | Write and link a source map beside each output |
| `Target` | `esnext` | The language level the output must run on: `es2015` to `es2025`, `esnext` |

Startup is refused for a `Dir` that is not a directory, an entry that is not a
file in it or escapes it, an entry that is not a script or a stylesheet, two
entries that would produce one name (`app.ts` and `app.js`), an unknown `Target`,
and a `Prefix` that does not begin and end with `/`.

## Configuration

```json
{
  "elagoht/bundle": {
    "dir": "assets",
    "entries": ["app.ts", "app.css"],
    "prefix": "/_bundle/",
    "minify": true,
    "sourcemap": false,
    "target": "es2020"
  }
}
```

## Limitations

- **The sources are read from disk.** `Dir` is a directory the process can read
  when it starts; a binary run from somewhere else, or built with the sources
  embedded, has nothing to bundle. esbuild's Go API reads the file system, not an
  `fs.FS`. A static build avoids the question: its output holds the bundle.
- **One output format.** Scripts are bundled for the browser as esbuild's default,
  an immediately-invoked function; there is no ES module output and no code
  splitting, so one entry is one script.
- **No esbuild plugins, no `tsconfig` paths beyond what esbuild reads itself, no
  PostCSS.** What esbuild does on its own is what is done.
- **Source maps are public when they are on.** In production, turning `Sourcemap`
  on serves the sources to anyone who asks.
- **A rebuild in development checks on request.** Nothing watches the directory
  between requests; collage's reload script, reading the mount every few hundred
  milliseconds while a page is open, is what makes an edit visible without a
  manual reload.
