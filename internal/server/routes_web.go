package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/identity"
)

func registerWebRoutes(mux *http.ServeMux, root string) {
	root = filepath.Clean(root)
	index := filepath.Join(root, "index.html")
	mux.HandleFunc("GET /", func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			http.NotFound(writer, request)
			return
		}
		relative := strings.TrimPrefix(filepath.Clean("/"+request.URL.Path), "/")
		candidate := filepath.Join(root, relative)
		if relative == "" {
			candidate = index
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			candidate = index
			if _, indexErr := os.Stat(candidate); indexErr != nil {
				http.NotFound(writer, request)
				return
			}
		}
		writer.Header().Set("Cache-Control", "no-store")
		if strings.Contains(candidate, string(filepath.Separator)+"assets"+string(filepath.Separator)) {
			writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeFile(writer, request, candidate)
	})
}

func registerArtifactRoutes(mux *http.ServeMux, root string, nodes *enrollment.Service, adminToken string) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return
	}
	if resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot); err == nil {
		absoluteRoot = resolvedRoot
	}
	mux.HandleFunc("GET /artifacts/{path...}", func(writer http.ResponseWriter, request *http.Request) {
		if !artifactRequestAuthorized(request, nodes, adminToken) {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "artifact authentication required"})
			return
		}
		relative := filepath.Clean(request.PathValue("path"))
		if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			http.NotFound(writer, request)
			return
		}
		candidate := filepath.Join(absoluteRoot, relative)
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		contained, err := filepath.Rel(absoluteRoot, resolved)
		if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
			http.NotFound(writer, request)
			return
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(writer, request, resolved)
	})
}

func artifactRequestAuthorized(request *http.Request, nodes *enrollment.Service, adminToken string) bool {
	if _, ok := identity.PrincipalFromContext(request.Context()); ok || adminToken != "" && identity.AuthenticateBearer(request.Header.Get("Authorization"), adminToken) {
		return true
	}
	nodeID, certificate, ok := authenticatedNodeCertificate(request)
	if !ok || nodes == nil {
		return false
	}
	node, err := nodes.GetNode(request.Context(), nodeID)
	return err == nil && node.CertificateRevokedAt == nil && node.CertificateSerial == certificate.SerialNumber.Text(16) &&
		node.Status != domain.NodeStatusRejected && node.Status != domain.NodeStatusPendingApproval && node.Status != domain.NodeStatusDiscovered
}
