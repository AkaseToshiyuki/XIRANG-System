package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/accounting"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/networkhealth"
	"github.com/xirang-grid/xirang/internal/topology"
)

func registerTelemetryIntervalRoutes(mux *http.ServeMux, manager TelemetryIntervalManager, adminToken string) {
	writeSettings := func(writer http.ResponseWriter) {
		writeJSON(writer, http.StatusOK, map[string]int64{
			"intervalSeconds": int64(manager.TelemetryInterval() / time.Second),
			"minimumSeconds":  2,
			"maximumSeconds":  60,
		})
	}
	mux.HandleFunc("GET /api/v1/system/telemetry-interval", requireAuthenticated(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeSettings(writer)
	}))
	mux.HandleFunc("PUT /api/v1/system/telemetry-interval", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			IntervalSeconds int64 `json:"intervalSeconds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		if payload.IntervalSeconds < 2 || payload.IntervalSeconds > 60 {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "telemetry interval must be between 2 and 60 seconds"})
			return
		}
		if err := manager.UpdateTelemetryInterval(request.Context(), time.Duration(payload.IntervalSeconds)*time.Second); err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeSettings(writer)
	}))
}

func registerAccountingRoutes(mux *http.ServeMux, manager AccountingManager, adminToken string) {
	dateRange := func(request *http.Request) (string, string, error) {
		to := request.URL.Query().Get("to")
		from := request.URL.Query().Get("from")
		if to == "" {
			to = time.Now().UTC().Format("2006-01-02")
		}
		if from == "" {
			end, err := time.Parse("2006-01-02", to)
			if err != nil {
				return "", "", accounting.ErrInvalid
			}
			from = end.AddDate(0, 0, -29).Format("2006-01-02")
		}
		return from, to, nil
	}
	mux.HandleFunc("GET /api/v1/accounting/summary", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		from, to, err := dateRange(request)
		if err != nil {
			writeError(writer, err)
			return
		}
		principal := requestPrincipal(request)
		ownerID := principalIdentity(principal)
		if principal.Role == "admin" && strings.TrimSpace(request.URL.Query().Get("ownerId")) != "" {
			ownerID = strings.TrimSpace(request.URL.Query().Get("ownerId"))
		}
		summary, err := manager.Summary(request.Context(), from, to, ownerID)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, summary)
	}))
	mux.HandleFunc("GET /api/v1/accounting/users", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if requestPrincipal(request).Role != "admin" {
			writeError(writer, identity.ErrForbidden)
			return
		}
		from, to, err := dateRange(request)
		if err != nil {
			writeError(writer, err)
			return
		}
		items, err := manager.UserSummaries(request.Context(), from, to)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("GET /api/v1/accounting/rates", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := manager.Rates(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("POST /api/v1/accounting/rates", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if requestPrincipal(request).Role != "admin" {
			writeError(writer, identity.ErrForbidden)
			return
		}
		var payload domain.BillingRate
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		rate, err := manager.CreateRate(request.Context(), payload, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, rate)
	}))
}

func registerFlowRoutes(mux *http.ServeMux, flows FlowReader, adminToken string) {
	mux.HandleFunc("GET /api/v1/flows", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := flows.ListLatest(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("GET /api/v1/nodes/{id}/flows", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 60
		if raw := request.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1000 {
				writeError(writer, fmt.Errorf("%w: limit must be between 1 and 1000", enrollment.ErrInvalidRequest))
				return
			}
			limit = parsed
		}
		items, err := flows.List(request.Context(), request.PathValue("id"), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
}

func registerNotificationRoutes(mux *http.ServeMux, notifications NotificationManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/alert-deliveries", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 100
		if raw := request.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1000 {
				writeError(writer, fmt.Errorf("%w: limit must be between 1 and 1000", enrollment.ErrInvalidRequest))
				return
			}
			limit = parsed
		}
		items, err := notifications.List(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		if items == nil {
			items = []domain.AlertDelivery{}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"webhook": notifications.Status(), "items": items})
	}))
	mux.HandleFunc("POST /api/v1/alert-deliveries/{id}/retry", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		delivery, err := notifications.Retry(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, delivery)
	}))
}

func registerAlertRoutes(mux *http.ServeMux, alerts AlertManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/alerts", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		includeResolved := false
		if raw := request.URL.Query().Get("includeResolved"); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(writer, fmt.Errorf("%w: includeResolved must be true or false", enrollment.ErrInvalidRequest))
				return
			}
			includeResolved = parsed
		}
		items, err := alerts.List(request.Context(), includeResolved)
		if err != nil {
			writeError(writer, err)
			return
		}
		if items == nil {
			items = []domain.Alert{}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("POST /api/v1/alerts/{id}/acknowledge", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		alert, err := alerts.Acknowledge(request.Context(), request.PathValue("id"), "bootstrap-admin")
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, alert)
	}))
	mux.HandleFunc("GET /api/v1/events/alerts", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		_ = http.NewResponseController(writer).SetWriteDeadline(time.Time{})
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache, no-transform")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, ": connected\n\n")
		flusher.Flush()
		updates := alerts.Subscribe(request.Context())
		keepalive := time.NewTicker(15 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case alert, ok := <-updates:
				if !ok {
					return
				}
				encoded, err := json.Marshal(alert)
				if err != nil {
					return
				}
				if _, err := fmt.Fprintf(writer, "event: alert\ndata: %s\n\n", encoded); err != nil {
					return
				}
				flusher.Flush()
			case <-keepalive.C:
				if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}))
}

func registerTopologyRoutes(mux *http.ServeMux, reader TopologyReader, telemetryReader TelemetryReader, adminToken string) {
	mux.HandleFunc("GET /api/v1/topology", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		versionText := request.URL.Query().Get("version")
		var snapshot domain.TopologySnapshot
		var err error
		if versionText == "" {
			snapshot, err = reader.LatestSnapshot(request.Context())
			if err == nil && telemetryReader != nil {
				snapshot = enrichTopologySnapshot(request.Context(), snapshot, telemetryReader)
			}
		} else {
			version, parseErr := strconv.ParseUint(versionText, 10, 64)
			if parseErr != nil || version == 0 {
				writeError(writer, fmt.Errorf("%w: version must be a positive integer", enrollment.ErrInvalidRequest))
				return
			}
			snapshot, err = reader.GetSnapshot(request.Context(), version)
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, snapshot)
	}))
	mux.HandleFunc("GET /api/v1/events/topology", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		_ = http.NewResponseController(writer).SetWriteDeadline(time.Time{})
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache, no-transform")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, ": connected\n\n")
		flusher.Flush()
		send := func() bool {
			snapshot, err := reader.LatestSnapshot(request.Context())
			if errors.Is(err, topology.ErrNotFound) {
				return true
			}
			if err != nil {
				return false
			}
			if telemetryReader != nil {
				snapshot = enrichTopologySnapshot(request.Context(), snapshot, telemetryReader)
			}
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(writer, "event: topology\ndata: %s\n\n", encoded); err != nil {
				return false
			}
			flusher.Flush()
			return true
		}
		if !send() {
			return
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-ticker.C:
				if !send() {
					return
				}
			}
		}
	}))
}

func enrichTopologySnapshot(ctx context.Context, snapshot domain.TopologySnapshot, telemetryReader TelemetryReader) domain.TopologySnapshot {
	return networkhealth.Enrich(ctx, snapshot, telemetryReader)
}

func registerTelemetryRoutes(mux *http.ServeMux, reader TelemetryReader, adminToken string) {
	mux.HandleFunc("GET /api/v1/nodes/{id}/inventory", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		inventory, err := reader.GetInventory(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, inventory)
	}))
	mux.HandleFunc("GET /api/v1/nodes/{id}/observations", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 100
		if rawLimit := request.URL.Query().Get("limit"); rawLimit != "" {
			parsed, err := strconv.Atoi(rawLimit)
			if err != nil || parsed < 1 || parsed > 1000 {
				writeError(writer, fmt.Errorf("%w: limit must be between 1 and 1000", enrollment.ErrInvalidRequest))
				return
			}
			limit = parsed
		}
		observations, err := reader.ListObservations(request.Context(), request.PathValue("id"), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": observations})
	}))
}
