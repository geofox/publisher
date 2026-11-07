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

// page reads an embedded HTML page.
func page(name string) []byte {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		panic(err) // build-time guarantee: the page is embedded
	}
	return b
}

// SignInPage is the local sign-in page, served at any app address to a
// browser without a session.
func SignInPage() []byte { return page("signin.html") }

// SetupPage is the first-run setup page.
func SetupPage() []byte { return page("setup.html") }

// SetupDonePage says setup is done (410).
func SetupDonePage() []byte { return page("setup-done.html") }
