package server

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"

	"github.com/xirang-grid/xirang/internal/discovery"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/pki"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func registerDiscoveryRoutes(mux *http.ServeMux, manager DiscoveryManager, adminToken string) {
	mux.HandleFunc("POST /api/v1/discovery/scans", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			CIDRs []string `json:"cidrs"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		scan, err := manager.Scan(request.Context(), payload.CIDRs)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, scan)
	}))
	mux.HandleFunc("GET /api/v1/discovery/scans", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 50
		if raw := request.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeError(writer, discovery.ErrInvalidRequest)
				return
			}
			limit = parsed
		}
		scans, err := manager.ListScans(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": scans})
	}))
	mux.HandleFunc("GET /api/v1/discovery/candidates", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		candidates, err := manager.ListCandidates(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": candidates})
	}))
	mux.HandleFunc("POST /api/v1/discovery/candidates/{id}/approve", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			PairingCode string `json:"pairingCode"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		bootstrap, err := manager.Approve(request.Context(), request.PathValue("id"), payload.PairingCode)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, bootstrap)
	}))
	mux.HandleFunc("POST /api/v1/discovery/candidates/{id}/reject", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		candidate, err := manager.Reject(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, candidate)
	}))
}

func registerEnrollmentRoutes(mux *http.ServeMux, service *enrollment.Service, joinTokens *enrollment.JoinTokenManager, adminToken string, commands CommandQueue, events *NodeEventBroker) {
	mux.HandleFunc("GET /api/v1/enrollment/ca", func(writer http.ResponseWriter, _ *http.Request) {
		caPEM, err := service.CACertificatePEM()
		if err != nil {
			writeError(writer, err)
			return
		}
		block, _ := pem.Decode([]byte(caPEM))
		if block == nil || block.Type != "CERTIFICATE" {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		digest := sha256.Sum256(block.Bytes)
		writeJSON(writer, http.StatusOK, map[string]string{
			"caCertificatePem": caPEM, "fingerprint": hex.EncodeToString(digest[:]),
		})
	})
	if joinTokens != nil {
		mux.HandleFunc("GET /api/v1/enrollment/join-token", requireAuthenticated(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, http.StatusOK, joinTokens.View())
		}))
		mux.HandleFunc("POST /api/v1/enrollment/join-token/reset", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
			created, err := joinTokens.Reset(request.Context())
			if err != nil {
				writeError(writer, err)
				return
			}
			writeJSON(writer, http.StatusCreated, created)
		}))
	}
	mux.HandleFunc("POST /api/v1/enrollment/tokens", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input createTokenRequest
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		created, err := service.CreateToken(request.Context(), enrollment.TokenOptions{
			Name: input.Name, TTL: time.Duration(input.TTLSeconds) * time.Second,
			MaxUses: input.MaxUses, RequireApproval: input.RequireApproval,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, created)
	}))

	mux.HandleFunc("POST /api/v1/enrollment/register", func(writer http.ResponseWriter, request *http.Request) {
		var input registerNodeRequest
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		result, err := service.Register(request.Context(), input.Token, input.CSRPEM, input.Inventory)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, result)
	})

	mux.HandleFunc("GET /api/v1/nodes", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		nodes, err := service.ListNodes(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": nodes})
	}))

	mux.HandleFunc("GET /api/v1/nodes/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		node, err := service.GetNode(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, node)
	}))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", requireAuthenticated(adminToken, nodeAction(service.RemoveNode)))

	mux.HandleFunc("POST /api/v1/nodes/{id}/approve", requireAuthenticated(adminToken, nodeAction(service.Approve)))
	mux.HandleFunc("POST /api/v1/nodes/{id}/reject", requireAuthenticated(adminToken, nodeAction(service.Reject)))
	mux.HandleFunc("PUT /api/v1/nodes/{id}/taints", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Taints []domain.Taint `json:"taints"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		node, err := service.SetNodeTaints(request.Context(), request.PathValue("id"), payload.Taints)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, node)
	}))
	mux.HandleFunc("PUT /api/v1/nodes/{id}/configuration", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Roles             []domain.NodeRole `json:"roles"`
			ManagementAddress string            `json:"managementAddress"`
			SSHUser           string            `json:"sshUser"`
			Group             string            `json:"group"`
			ComputePriority   int32             `json:"computePriority"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		nodeID := request.PathValue("id")
		current, err := service.GetNode(request.Context(), nodeID)
		if err != nil {
			writeError(writer, err)
			return
		}
		managementEnabled := false
		for _, role := range payload.Roles {
			managementEnabled = managementEnabled || role == domain.NodeRoleManagement
		}
		managementChanged := current.HasRole(domain.NodeRoleManagement) != managementEnabled
		if managementChanged && commands == nil {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		if !managementChanged {
			node, err := service.SetNodeConfiguration(request.Context(), nodeID, payload.Roles, payload.ManagementAddress, payload.SSHUser, payload.Group, payload.ComputePriority)
			if err != nil {
				writeError(writer, err)
				return
			}
			writeJSON(writer, http.StatusOK, node)
			return
		}
		commandID := "command_" + randomIdentifier()
		node, err := service.RequestNodeConfiguration(request.Context(), nodeID, payload.Roles, payload.ManagementAddress, payload.SSHUser, payload.Group, payload.ComputePriority, commandID, managementEnabled)
		if err != nil {
			writeError(writer, err)
			return
		}
		message := &xirangv1.ControlMessage{
			MessageId: commandID, SentAt: timestamppb.Now(),
			Payload: &xirangv1.ControlMessage_SetManagementPanel{SetManagementPanel: &xirangv1.SetManagementPanel{Enabled: managementEnabled}},
		}
		if err := commands.Enqueue(node.ID, message); err != nil {
			_, _ = service.ConfirmManagementPanel(request.Context(), node.ID, commandID, false, "command enqueue failed")
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		writeJSON(writer, http.StatusAccepted, node)
	}))
	mux.HandleFunc("POST /api/v1/nodes/{id}/mode", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if commands == nil {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		var input nodeModeRequest
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		desired, protocolMode, ok := parseNodeMode(input.Mode)
		if !ok {
			writeError(writer, fmt.Errorf("%w: mode must be ready, cordoned, or draining", enrollment.ErrInvalidRequest))
			return
		}
		commandID := "command_" + randomIdentifier()
		node, err := service.RequestNodeMode(request.Context(), request.PathValue("id"), desired, commandID)
		if err != nil {
			writeError(writer, err)
			return
		}
		message := &xirangv1.ControlMessage{
			MessageId: commandID, SentAt: timestamppb.Now(),
			Payload: &xirangv1.ControlMessage_SetNodeMode{SetNodeMode: &xirangv1.SetNodeMode{Mode: protocolMode, Reason: input.Reason}},
		}
		if err := commands.Enqueue(node.ID, message); err != nil {
			_, _ = service.ConfirmNodeMode(request.Context(), node.ID, commandID, false, "command enqueue failed")
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"commandId": commandID, "node": node})
	}))
	mux.HandleFunc("POST /api/v1/nodes/{id}/certificate/revoke", requireAuthenticated(adminToken, nodeAction(service.RevokeCertificate)))
	mux.HandleFunc("POST /api/v1/nodes/{id}/certificate/rotate-command", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if commands == nil {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		commandID := "command_" + randomIdentifier()
		rotationID := "rotation_" + randomIdentifier()
		node, err := service.RequestCertificateRotation(request.Context(), request.PathValue("id"), commandID, rotationID)
		if err != nil {
			writeError(writer, err)
			return
		}
		message := &xirangv1.ControlMessage{
			MessageId: commandID, SentAt: timestamppb.Now(),
			Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{RotationId: rotationID}},
		}
		if err := commands.Enqueue(node.ID, message); err != nil {
			_, _ = service.ConfirmCertificateRotation(request.Context(), node.ID, commandID, false, "command enqueue failed")
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"commandId": commandID, "rotationId": rotationID, "node": node})
	}))
	mux.HandleFunc("POST /api/v1/nodes/{id}/certificate/rotate", func(writer http.ResponseWriter, request *http.Request) {
		requestedNodeID := request.PathValue("id")
		certificateNodeID, certificate, ok := authenticatedNodeCertificate(request)
		if !ok || certificateNodeID != requestedNodeID {
			writeError(writer, enrollment.ErrUnauthorized)
			return
		}
		var input rotateCertificateRequest
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		result, err := service.RotateCertificate(request.Context(), requestedNodeID, certificate.SerialNumber.Text(16), input.CSRPEM)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/v1/nodes/{id}/heartbeat", func(writer http.ResponseWriter, request *http.Request) {
		requestedNodeID := request.PathValue("id")
		certificateNodeID, certificate, ok := authenticatedNodeCertificate(request)
		if !ok || certificateNodeID != requestedNodeID {
			writeError(writer, enrollment.ErrUnauthorized)
			return
		}
		node, err := service.Heartbeat(request.Context(), requestedNodeID, certificate.SerialNumber.Text(16))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, node)
	})
	mux.HandleFunc("GET /api/v1/audit", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		events, err := service.ListAuditEvents(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": events})
	}))
	mux.HandleFunc("GET /api/v1/assignments", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		reports, err := service.ListAssignmentReports(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": reports})
	}))
	if events != nil {
		mux.HandleFunc("GET /api/v1/events/nodes", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
			updates := events.Subscribe(request.Context())
			keepalive := time.NewTicker(15 * time.Second)
			defer keepalive.Stop()
			for {
				select {
				case <-request.Context().Done():
					return
				case node, ok := <-updates:
					if !ok {
						return
					}
					encoded, err := json.Marshal(node)
					if err != nil {
						return
					}
					if _, err := fmt.Fprintf(writer, "event: node\ndata: %s\n\n", encoded); err != nil {
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
}

func authenticatedNodeCertificate(request *http.Request) (string, *x509.Certificate, bool) {
	if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 || len(request.TLS.VerifiedChains) == 0 {
		return "", nil, false
	}
	certificate := request.TLS.PeerCertificates[0]
	nodeID, ok := pki.NodeIDFromCertificate(certificate)
	return nodeID, certificate, ok
}

func parseNodeMode(value string) (domain.NodeStatus, xirangv1.NodeMode, bool) {
	switch value {
	case "ready":
		return domain.NodeStatusReady, xirangv1.NodeMode_NODE_MODE_READY, true
	case "cordoned":
		return domain.NodeStatusCordoned, xirangv1.NodeMode_NODE_MODE_CORDONED, true
	case "draining":
		return domain.NodeStatusDraining, xirangv1.NodeMode_NODE_MODE_DRAINING, true
	default:
		return "", xirangv1.NodeMode_NODE_MODE_UNSPECIFIED, false
	}
}
