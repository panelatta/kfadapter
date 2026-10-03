package web

import (
	"bytes"
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// staticFiles holds the frontend's production build under static/dist. Assets
// are packaged with the binary; the service never fetches fonts, scripts,
// images, telemetry, or updates from the network. static/README keeps the
// directory embeddable in a fresh clone before the console has been built.
//
//go:embed all:static
var staticFiles embed.FS

const staticRoot = "static/dist/"

func (a *API) serveAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		a.writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method Not Allowed", "")
		return
	}
	name, spaFallback := staticAssetName(r.URL.Path)
	content, err := fs.ReadFile(staticFiles, staticRoot+name)
	if err != nil && spaFallback {
		name = "index.html"
		content, err = fs.ReadFile(staticFiles, staticRoot+name)
		if errors.Is(err, fs.ErrNotExist) {
			setNoStore(w.Header())
			a.writeProblem(w, http.StatusServiceUnavailable, "console_not_built", "Console not built", "This binary was built without the web console. Run `npm ci && npm run build` in web/ and rebuild.")
			return
		}
	}
	if err != nil {
		setNoStore(w.Header())
		a.writeProblem(w, http.StatusNotFound, "not_found", "Not Found", "")
		return
	}
	if strings.HasPrefix(name, "assets/") {
		// Vite content-hashes every file under assets/, so a URL never changes
		// meaning and browsers may keep it for good.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		setNoStore(w.Header())
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
}

func staticAssetName(requestPath string) (name string, spaFallback bool) {
	clean := path.Clean("/" + requestPath)
	name = strings.TrimPrefix(clean, "/")
	if name == "" || name == "." {
		return "index.html", true
	}
	if strings.Contains(name, "\\") || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, !strings.Contains(path.Base(name), ".")
}
