package dashboard

import (
	"embed"
	"net/http"
)

//go:embed assets/htmx.min.js assets/app.css
var assetsFS embed.FS

// serveAsset serves one embedded asset. Assets are same-origin under the
// dashboard's CSP; no CORS headers are ever added. ServeFileFS derives the
// content type from the extension and handles Range/conditional requests.
func (h *Handler) serveAsset(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assetsFS, name)
	}
}
