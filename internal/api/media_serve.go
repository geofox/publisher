package api

import (
	"net/http"
	"strings"
)

// mediaCSP replaces the app's policy on served media: nothing in a file
// runs or loads anything, even opened on its own.
const mediaCSP = "default-src 'none'; sandbox"

// serveMedia serves the media kept on this server's disk (spec §7.6):
// GET and HEAD of /media/<sha256>.<ext>, without sign-in, like a Blossom
// server (the names are content hashes). Images and videos are served as
// their type; anything else as an attachment.
func (a *API) serveMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/media/")
	if a.DiskMedia == nil {
		http.NotFound(w, r)
		return
	}
	f, mime, attach, err := a.DiskMedia.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", mediaCSP)
	h.Set("Content-Type", mime)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+strings.SplitN(name, ".", 2)[0]+`"`)
	// Other sites show it (a Nostr client's page, the Threads crawler).
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Access-Control-Allow-Origin", "*")
	if attach {
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}
