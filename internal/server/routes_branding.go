package server

import (
	"net/http"

	"github.com/xirang-grid/xirang/internal/branding"
	"github.com/xirang-grid/xirang/internal/domain"
)

func registerBrandingRoutes(mux *http.ServeMux, service *branding.Service, adminToken string) {
	mux.HandleFunc("GET /api/v1/cluster/branding", func(writer http.ResponseWriter, request *http.Request) {
		value, err := service.Get(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, value)
	})
	mux.HandleFunc("PUT /api/v1/cluster/branding", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload domain.ClusterBranding
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		value, err := service.Update(request.Context(), payload, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, value)
	}))
}
