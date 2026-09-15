package server

import (
	"net/http"
	"strconv"
	"time"

	buildmanager "github.com/xirang-grid/xirang/internal/build"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
)

func registerBuildRoutes(mux *http.ServeMux, manager BuildManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/build-retention", requireAuthenticated(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		policy := manager.RetentionPolicy()
		writeJSON(writer, http.StatusOK, map[string]any{"maximumRecords": policy.MaximumRecords, "maximumAgeSeconds": int64(policy.MaximumAge / time.Second), "preserveSuccessfulBuilds": policy.PreserveSuccessfulBuilds})
	}))
	mux.HandleFunc("PUT /api/v1/build-retention", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			MaximumRecords           int   `json:"maximumRecords"`
			MaximumAgeSeconds        int64 `json:"maximumAgeSeconds"`
			PreserveSuccessfulBuilds bool  `json:"preserveSuccessfulBuilds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		policy, err := manager.UpdateRetentionPolicy(request.Context(), domain.BuildRetentionPolicy{MaximumRecords: payload.MaximumRecords, MaximumAge: time.Duration(payload.MaximumAgeSeconds) * time.Second, PreserveSuccessfulBuilds: payload.PreserveSuccessfulBuilds}, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"maximumRecords": policy.MaximumRecords, "maximumAgeSeconds": int64(policy.MaximumAge / time.Second), "preserveSuccessfulBuilds": policy.PreserveSuccessfulBuilds})
	}))
	mux.HandleFunc("POST /api/v1/builds/cleanup", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		result, err := manager.Cleanup(request.Context(), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}))
	mux.HandleFunc("GET /api/v1/build-signing-key", requireAuthenticated(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, manager.SigningKey())
	}))
	mux.HandleFunc("POST /api/v1/build-signing-key/rotate", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		key, err := manager.RotateSigningKey(request.Context(), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, key)
	}))
	mux.HandleFunc("GET /api/v1/build-workers", requireAuthenticated(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"items": manager.Workers()})
	}))
	mux.HandleFunc("GET /api/v1/builds", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 100
		if raw := request.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeError(writer, buildmanager.ErrInvalidRequest)
				return
			}
			limit = parsed
		}
		builds, err := manager.List(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		principal := requestPrincipal(request)
		if principal.Role != "admin" {
			visible := builds[:0]
			for _, item := range builds {
				if canAccessBuild(principal, item) {
					visible = append(visible, item)
				}
			}
			builds = visible
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": builds})
	}))
	mux.HandleFunc("POST /api/v1/builds", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Name                string            `json:"name"`
			Application         string            `json:"application"`
			Variant             string            `json:"variant"`
			RuntimeNodeIDs      []string          `json:"runtimeNodeIds"`
			RuntimeEntrypoint   string            `json:"runtimeEntrypoint"`
			RuntimeRequirements map[string]string `json:"runtimeRequirements"`
			SourcePath          string            `json:"sourcePath"`
			SourceSHA256        string            `json:"sourceSha256"`
			Architecture        string            `json:"architecture"`
			BuilderArchitecture string            `json:"builderArchitecture"`
			WorkerID            string            `json:"workerId"`
			CrossCompile        bool              `json:"crossCompile"`
			SourceSpaceID       string            `json:"sourceSpaceId"`
			OutputSpaceID       string            `json:"outputSpaceId"`
			PublishPath         string            `json:"publishPath"`
			Command             []string          `json:"command"`
			Steps               [][]string        `json:"steps"`
			OutputPath          string            `json:"outputPath"`
			Toolchain           string            `json:"toolchain"`
			Environment         map[string]string `json:"environment"`
			MilliCPU            int64             `json:"milliCpu"`
			MemoryBytes         int64             `json:"memoryBytes"`
			TimeoutSeconds      int64             `json:"timeoutSeconds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		build, err := manager.Submit(request.Context(), domain.BuildRecipe{
			Name: payload.Name, SourcePath: payload.SourcePath, SourceSHA256: payload.SourceSHA256,
			Application: payload.Application, Variant: payload.Variant, RuntimeNodeIDs: payload.RuntimeNodeIDs,
			RuntimeEntrypoint:   payload.RuntimeEntrypoint,
			RuntimeRequirements: payload.RuntimeRequirements,
			Architecture:        payload.Architecture, Command: payload.Command, Steps: payload.Steps, OutputPath: payload.OutputPath,
			BuilderArchitecture: payload.BuilderArchitecture, WorkerID: payload.WorkerID, CrossCompile: payload.CrossCompile,
			SourceSpaceID: payload.SourceSpaceID, OutputSpaceID: payload.OutputSpaceID,
			PublishPath: payload.PublishPath,
			Toolchain:   payload.Toolchain, Environment: payload.Environment, MilliCPU: payload.MilliCPU,
			MemoryBytes: payload.MemoryBytes, Timeout: time.Duration(payload.TimeoutSeconds) * time.Second,
		}, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, build)
	}))
	mux.HandleFunc("GET /api/v1/builds/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		build, err := manager.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessBuild(requestPrincipal(request), build) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		writeJSON(writer, http.StatusOK, build)
	}))
	mux.HandleFunc("POST /api/v1/builds/{id}/cancel", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		item, err := manager.Cancel(request.Context(), request.PathValue("id"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, item)
	}))
	mux.HandleFunc("PUT /api/v1/builds/{id}/runtime-targets", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		updater, ok := manager.(buildRuntimeTargetUpdater)
		if !ok {
			writeError(writer, buildmanager.ErrUnavailable)
			return
		}
		var payload struct {
			NodeIDs []string `json:"nodeIds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		item, err := updater.UpdateRuntimeTargets(request.Context(), request.PathValue("id"), payload.NodeIDs, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, item)
	}))
	mux.HandleFunc("DELETE /api/v1/builds/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		result, err := manager.Delete(request.Context(), request.PathValue("id"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}))
}
