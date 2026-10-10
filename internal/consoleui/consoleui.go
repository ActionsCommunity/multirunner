// Package consoleui serves the embedded Multirunner Operations Console.
package consoleui

import (
	"embed"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

var (
	//go:embed dist
	embeddedFiles embed.FS
)

// Handler serves production console assets with single-page application
// fallback for routes owned by the frontend.
type Handler struct {
	files fs.FS
}

// New creates a console UI handler from the embedded production build.
func New() (*Handler, error) {
	files, err := fs.Sub(embeddedFiles, "dist")
	if err != nil {
		return nil, errors.New("open embedded console assets")
	}
	return &Handler{files: files}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if requested == "." || requested == "" {
		requested = "index.html"
	}
	content, err := fs.ReadFile(h.files, requested)
	if err != nil {
		if strings.HasPrefix(r.URL.Path, "/api/") || path.Ext(requested) != "" {
			http.NotFound(w, r)
			return
		}
		requested = "index.html"
		content, err = fs.ReadFile(h.files, requested)
		if err != nil {
			http.Error(w, "console assets unavailable", http.StatusServiceUnavailable)
			return
		}
	}

	if requested == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if contentType := mime.TypeByExtension(path.Ext(requested)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(content)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(content)
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
