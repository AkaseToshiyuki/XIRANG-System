package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/identity"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type healthNodeReader interface {
	GetNode(context.Context, string) (domain.Node, error)
}

func registerHostHealthRoutes(mux *http.ServeMux, service *hosthealth.Service, nodes healthNodeReader, commands CommandQueue, adminToken string) {
	admin := func(next http.HandlerFunc) http.HandlerFunc {
		return requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
			if requestPrincipal(request).Role != "admin" {
				writeError(writer, identity.ErrForbidden)
				return
			}
			if !serviceAvailable(nodes) {
				writeError(writer, hosthealth.ErrUnavailable)
				return
			}
			if _, err := nodes.GetNode(request.Context(), request.PathValue("id")); err != nil {
				writeError(writer, err)
				return
			}
			next(writer, request)
		})
	}
	mux.HandleFunc("GET /api/v1/nodes/{id}/health", admin(func(writer http.ResponseWriter, request *http.Request) {
		value, err := service.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, value)
	}))
	mux.HandleFunc("GET /api/v1/nodes/{id}/health/events", admin(func(writer http.ResponseWriter, request *http.Request) {
		limit := 100
		if raw := request.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil {
				writeError(writer, hosthealth.ErrInvalid)
				return
			}
		}
		items, err := service.Events(request.Context(), request.PathValue("id"), request.URL.Query().Get("kind"), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items, "retentionDays": 30, "maximumPerNode": hosthealth.RetainedEvents})
	}))
	mux.HandleFunc("POST /api/v1/nodes/{id}/health/notes", admin(func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Message        string `json:"message"`
			RelatedEventID string `json:"relatedEventId"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		value, err := service.AddNote(request.Context(), request.PathValue("id"), input.Message, input.RelatedEventID, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, value)
	}))
	mux.HandleFunc("POST /api/v1/nodes/{id}/health/actions", admin(func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Action string `json:"action"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		nodeID := request.PathValue("id")
		node, err := nodes.GetNode(request.Context(), nodeID)
		if err != nil {
			writeError(writer, err)
			return
		}
		snapshot, err := service.Get(request.Context(), nodeID)
		if err != nil || !snapshot.IdentifySupported || time.Since(snapshot.ReceivedAt) > 2*time.Minute || !containsString(node.Capabilities, hosthealth.IdentifyCapability) || !serviceAvailable(commands) || (node.Status != domain.NodeStatusReady && node.Status != domain.NodeStatusCordoned && node.Status != domain.NodeStatusDraining) {
			writeError(writer, hosthealth.ErrUnavailable)
			return
		}
		actor := requestPrincipal(request)
		action, err := hosthealth.NewAction(nodeID, input.Action, actor)
		if err != nil {
			writeError(writer, err)
			return
		}
		if err := service.RecordAction(request.Context(), action, actor); err != nil {
			writeError(writer, err)
			return
		}
		message := &xirangv1.ControlMessage{MessageId: action.ID, SentAt: timestamppb.Now(), Payload: &xirangv1.ControlMessage_SetBmcIdentify{SetBmcIdentify: &xirangv1.SetBMCIdentify{Enabled: input.Action == "identify-on", ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Second))}}}
		if err := commands.Enqueue(nodeID, message); err != nil {
			action.Status, action.Message = "failed", "Agent command queue unavailable"
			now := time.Now().UTC()
			action.CompletedAt = &now
			if saveErr := service.RecordAction(request.Context(), action, actor); saveErr != nil {
				writeError(writer, saveErr)
				return
			}
			writeError(writer, hosthealth.ErrUnavailable)
			return
		}
		writeJSON(writer, http.StatusAccepted, action)
	}))
	mux.HandleFunc("GET /api/v1/nodes/{id}/health/actions/{actionID}", admin(func(writer http.ResponseWriter, request *http.Request) {
		value, err := service.Action(request.Context(), request.PathValue("actionID"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if value.NodeID != request.PathValue("id") {
			writeError(writer, hosthealth.ErrNotFound)
			return
		}
		if value.Status == "queued" && time.Since(value.CreatedAt) > time.Minute {
			value.Message = "No acknowledgement received; outcome is unknown"
		}
		writeJSON(writer, http.StatusOK, value)
	}))
}
