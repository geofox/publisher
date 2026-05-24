package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assets embed.FS

// Handler serves the embedded SPA (index.html, app.css, ES modules) as static files.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err) // build-time guarantee: the assets dir is embedded
	}
	return http.FileServer(http.FS(sub))
}
