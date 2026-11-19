package consoleapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/GerardSmit/multirunner/internal/backup"
)

func backupListHandler(reader BackupReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := parseListPage(r, 100, 500)
		if err != nil {
			writeValidationError(w, err)
			return
		}
		items, err := reader.List(r.Context(), 500)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "backups_unavailable", "backups are unavailable")
			return
		}
		start := min(page.Offset, len(items))
		end := min(start+page.Limit+1, len(items))
		writeJSON(w, http.StatusOK, pagedResponse(items[start:end], page, map[string]any{}))
	}
}

func backupDownloadHandler(reader BackupReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		metadata, file, err := reader.Open(r.Context(), r.PathValue("id"))
		if errors.Is(err, backup.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "backup was not found")
			return
		}
		if errors.Is(err, backup.ErrIntegrity) {
			writeError(w, http.StatusConflict, "integrity_failed", "backup integrity verification failed")
			return
		}
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "backup_unavailable", "backup is unavailable")
			return
		}
		defer file.Close()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, metadata.FileName))
		w.Header().Set("X-Content-SHA256", metadata.SHA256)
		http.ServeContent(w, r, metadata.FileName, metadata.CompletedAt, file)
	}
}
