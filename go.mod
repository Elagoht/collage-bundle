// A collage plugin that bundles an application's JavaScript, TypeScript and CSS
// with esbuild when it starts, serves the output under content-hashed names, and
// rebuilds on an edit in development.
module github.com/Elagoht/collage-bundle

go 1.26

require github.com/Elagoht/collage v0.23.0

require (
	github.com/evanw/esbuild v0.28.2
	golang.org/x/sys v0.0.0-20220715151400-c0bba94af5f8 // indirect
)
