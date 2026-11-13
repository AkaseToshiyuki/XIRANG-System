package server

import (
	"net/http"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/networkhealth"
)

func registerNetworkPathRoutes(mux *http.ServeMux, resolver *networkhealth.Resolver, nodes *enrollment.Service, adminToken string) {
	mux.HandleFunc("GET /api/v1/network-paths", func(w http.ResponseWriter, r *http.Request) {
		source := r.URL.Query().Get("source")
		_, principal := identity.PrincipalFromContext(r.Context())
		administrator := principal || adminToken != "" && identity.AuthenticateBearer(r.Header.Get("Authorization"), adminToken)
		if !administrator {
			id, certificate, ok := authenticatedNodeCertificate(r)
			if !ok || nodes == nil || source != "" && source != id {
				writeError(w, enrollment.ErrUnauthorized)
				return
			}
			node, err := nodes.GetNode(r.Context(), id)
			if err != nil || node.CertificateRevokedAt != nil || node.RemovedAt != nil || node.CertificateSerial != certificate.SerialNumber.Text(16) ||
				node.Status == domain.NodeStatusRejected || node.Status == domain.NodeStatusPendingApproval || node.Status == domain.NodeStatusDiscovered {
				writeError(w, enrollment.ErrUnauthorized)
				return
			}
			source = id
		}
		local, err := resolver.LocalNodeID(r.Context())
		if err != nil {
			writeError(w, enrollment.ErrUnavailable)
			return
		}
		if source == "" {
			source = local
		}
		target := r.URL.Query().Get("target")
		if target == "" {
			target = local
		}
		paths, err := resolver.Paths(r.Context(), source, target)
		if err != nil {
			writeError(w, enrollment.ErrUnavailable)
			return
		}
		if paths == nil {
			paths = []networkhealth.Path{}
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"sourceNodeId": source, "targetNodeId": target, "paths": paths})
	})
}
