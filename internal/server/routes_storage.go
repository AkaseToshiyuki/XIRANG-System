package server

import (
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/xirang-grid/xirang/internal/domain"
)

func registerBackupRoutes(mux *http.ServeMux, manager BackupManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/system/backups", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		backups, err := manager.List(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": backups})
	}))
	mux.HandleFunc("POST /api/v1/system/backups", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		manifest, err := manager.Create(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, manifest)
	}))
	mux.HandleFunc("POST /api/v1/system/backups/{id}/verify", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		manifest, err := manager.Verify(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, manifest)
	}))
}

func registerSharedSpaceRoutes(mux *http.ServeMux, spaces SharedSpaceManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/shared-spaces", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := spaces.List(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		public := make([]domain.SharedSpace, 0, len(items))
		for _, item := range items {
			public = append(public, publicSharedSpace(item))
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": public})
	}))
	mux.HandleFunc("POST /api/v1/shared-spaces", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input domain.SharedSpace
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		space, err := spaces.Apply(request.Context(), input)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, publicSharedSpace(space))
	}))
	mux.HandleFunc("PUT /api/v1/shared-spaces/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input domain.SharedSpace
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		input.ID = request.PathValue("id")
		space, err := spaces.Apply(request.Context(), input)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, publicSharedSpace(space))
	}))
	mux.HandleFunc("DELETE /api/v1/shared-spaces/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if err := spaces.Delete(request.Context(), request.PathValue("id")); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/shared-spaces/{id}/unshare", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if err := spaces.Delete(request.Context(), request.PathValue("id")); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/shared-spaces/{id}/files", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := spaces.ListFiles(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("GET /api/v1/shared-spaces/{id}/files/content", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Query().Get("path")
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(path)))
		if err := spaces.DownloadFile(request.Context(), request.PathValue("id"), path, requestPrincipal(request), writer); err != nil {
			writeError(writer, err)
		}
	}))
	mux.HandleFunc("PUT /api/v1/shared-spaces/{id}/files/content", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		input := http.MaxBytesReader(writer, request.Body, 64<<30)
		if err := spaces.UploadFile(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"), requestPrincipal(request), input); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/shared-spaces/{id}/files/directory", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		if err := spaces.CreateDirectory(request.Context(), request.PathValue("id"), input.Path, requestPrincipal(request)); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/shared-spaces/{id}/files/rename", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			OldPath string `json:"oldPath"`
			NewPath string `json:"newPath"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		if err := spaces.RenameFile(request.Context(), request.PathValue("id"), input.OldPath, input.NewPath, requestPrincipal(request)); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("DELETE /api/v1/shared-spaces/{id}/files", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if err := spaces.DeleteFile(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"), requestPrincipal(request)); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
}

func publicSharedSpace(space domain.SharedSpace) domain.SharedSpace {
	space.FileOwners = nil
	return space
}
