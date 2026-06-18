package api

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// handleIndex serves the minimal status/usage UI at the root path.
func (h *Handler) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}
