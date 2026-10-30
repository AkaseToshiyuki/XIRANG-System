package server

import (
	"net/http"

	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/domain"
)

func registerAlertPolicyRoutes(mux *http.ServeMux, service *alertpolicy.Service, adminToken string) {
	mux.HandleFunc("GET /api/v1/alert-policy", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		value, err := service.Get(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, value)
	}))
	mux.HandleFunc("PUT /api/v1/alert-policy", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var value domain.AlertPolicy
		if err := decodeJSON(writer, request, &value); err != nil {
			writeError(writer, err)
			return
		}
		saved, err := service.Update(request.Context(), value, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, saved)
	}))
}
