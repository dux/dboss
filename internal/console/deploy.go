package console

import (
	"net/http"
	"strings"
)

func (h *Handler) deployPreview(w http.ResponseWriter, r *http.Request) {
	app := strings.TrimSpace(r.URL.Query().Get("app"))
	if app == "" {
		writeError(w, http.StatusBadRequest, "app is required")
		return
	}
	preview, err := h.service.DeployPreview(r.Context(), app)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}
