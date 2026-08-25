// Package server implements the XIRANG management and enrollment HTTP API.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/accounting"
	"github.com/xirang-grid/xirang/internal/alerting"
	"github.com/xirang-grid/xirang/internal/backup"
	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/clusterops"
	"github.com/xirang-grid/xirang/internal/discovery"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/flow"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/jobspec"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	"github.com/xirang-grid/xirang/internal/networkhealth"
	"github.com/xirang-grid/xirang/internal/notification"
	"github.com/xirang-grid/xirang/internal/pki"
	"github.com/xirang-grid/xirang/internal/scheduler"
	"github.com/xirang-grid/xirang/internal/telemetry"
	"github.com/xirang-grid/xirang/internal/terminal"
	"github.com/xirang-grid/xirang/internal/topology"
	"github.com/xirang-grid/xirang/pkg/version"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type CommandQueue interface {
	Enqueue(string, *xirangv1.ControlMessage) error
}

type limitedTextWriter struct {
	builder strings.Builder
	limit   int
}

func (writer *limitedTextWriter) Write(value []byte) (int, error) {
	if len(value) > writer.limit-writer.builder.Len() {
		return 0, fmt.Errorf("payload exceeds %d bytes", writer.limit)
	}
	return writer.builder.Write(value)
}

func (writer *limitedTextWriter) String() string { return writer.builder.String() }

type TelemetryReader interface {
	GetInventory(context.Context, string) (domain.NodeInventorySnapshot, error)
	ListObservations(context.Context, string, int) ([]domain.NodeResourceObservation, error)
}

type TopologyReader interface {
	LatestSnapshot(context.Context) (domain.TopologySnapshot, error)
	GetSnapshot(context.Context, uint64) (domain.TopologySnapshot, error)
}

type FlowReader interface {
	List(context.Context, string, int) ([]domain.NodeFlowObservation, error)
	ListLatest(context.Context) ([]domain.NodeFlowObservation, error)
}

type AlertManager interface {
	List(context.Context, bool) ([]domain.Alert, error)
	Acknowledge(context.Context, string, string) (domain.Alert, error)
	Subscribe(context.Context) <-chan domain.Alert
}

type NotificationManager interface {
	Status() domain.AlertWebhookStatus
	List(context.Context, int) ([]domain.AlertDelivery, error)
	Retry(context.Context, string) (domain.AlertDelivery, error)
}

type JobManager interface {
	Submit(context.Context, scheduler.SubmitRequest) (domain.Job, error)
	Get(context.Context, string) (domain.Job, error)
	List(context.Context, int) ([]domain.Job, error)
	Cancel(context.Context, string, scheduler.CancelRequest) (domain.Job, error)
}

type StorageQuotaUpdater interface {
	UpdateGroupStorageQuota(context.Context, string, int64) error
}

type UserStorageQuotaUpdater interface {
	UpdateUserStorageQuota(context.Context, string, int64) error
}

type GroupDisableUpdater interface {
	SetGroupDisabled(context.Context, string, bool) error
}

type JobTemplateManager interface {
	List(context.Context) ([]domain.JobTemplate, error)
	Get(context.Context, string) (domain.JobTemplate, error)
	Create(context.Context, string, string, string, domain.Principal) (domain.JobTemplate, error)
	Update(context.Context, string, uint64, string, string, string, domain.Principal) (domain.JobTemplate, error)
	Delete(context.Context, string, uint64, domain.Principal) error
}

type QueueManager interface {
	PutQueuePolicy(context.Context, domain.QueuePolicy) (domain.QueuePolicy, error)
	ListQueuePolicies(context.Context) ([]domain.QueuePolicy, error)
	Elasticity(context.Context) (domain.ElasticitySnapshot, error)
}

type AccountingManager interface {
	Summary(context.Context, string, string, string) (domain.UsageSummary, error)
	UserSummaries(context.Context, string, string) ([]domain.UsageSummary, error)
	Rates(context.Context) ([]domain.BillingRate, error)
	CreateRate(context.Context, domain.BillingRate, domain.Principal) (domain.BillingRate, error)
}

type DiscoveryManager interface {
	Scan(context.Context, []string) (domain.DiscoveryScan, error)
	ListScans(context.Context, int) ([]domain.DiscoveryScan, error)
	ListCandidates(context.Context) ([]domain.DiscoveryCandidate, error)
	Approve(context.Context, string, string) (domain.DiscoveryBootstrap, error)
	Reject(context.Context, string) (domain.DiscoveryCandidate, error)
}

type BuildManager interface {
	Submit(context.Context, domain.BuildRecipe, domain.Principal) (domain.Build, error)
	Get(context.Context, string) (domain.Build, error)
	List(context.Context, int) ([]domain.Build, error)
	Cancel(context.Context, string, domain.Principal) (domain.Build, error)
	Delete(context.Context, string, domain.Principal) (domain.BuildCleanupResult, error)
	Cleanup(context.Context, domain.Principal) (domain.BuildCleanupResult, error)
	RetentionPolicy() domain.BuildRetentionPolicy
	UpdateRetentionPolicy(context.Context, domain.BuildRetentionPolicy, domain.Principal) (domain.BuildRetentionPolicy, error)
	Workers() []domain.BuildWorker
	SigningKey() domain.BuildSigningKey
	RotateSigningKey(context.Context, domain.Principal) (domain.BuildSigningKey, error)
}

type applicationBuildResolver interface {
	ResolveApplication(context.Context, string) ([]domain.NativeArtifactVariant, error)
}

type buildRuntimeTargetUpdater interface {
	UpdateRuntimeTargets(context.Context, string, []string, domain.Principal) (domain.Build, error)
}

type batchJobPlanner interface {
	PlanBatch(context.Context, domain.TaskSpec, int, int) ([]string, error)
}

type BackupManager interface {
	Create(context.Context) (backup.Manifest, error)
	List(context.Context) ([]backup.Manifest, error)
	Verify(context.Context, string) (backup.Manifest, error)
}

type SharedSpaceManager interface {
	List(context.Context) ([]domain.SharedSpace, error)
	Apply(context.Context, domain.SharedSpace) (domain.SharedSpace, error)
	Delete(context.Context, string) error
	ListFiles(context.Context, string, string, domain.Principal) ([]domain.SharedFile, error)
	DownloadFile(context.Context, string, string, domain.Principal, io.Writer) error
	UploadFile(context.Context, string, string, domain.Principal, io.Reader) error
	CreateDirectory(context.Context, string, string, domain.Principal) error
	RenameFile(context.Context, string, string, string, domain.Principal) error
	DeleteFile(context.Context, string, string, domain.Principal) error
}

type TelemetryIntervalManager interface {
	TelemetryInterval() time.Duration
	UpdateTelemetryInterval(context.Context, time.Duration) error
}

type Config struct {
	Address            string
	Logger             *slog.Logger
	Enrollment         *enrollment.Service
	JoinTokens         *enrollment.JoinTokenManager
	AdminToken         string
	TLSConfig          *tls.Config
	Commands           CommandQueue
	NodeEvents         *NodeEventBroker
	Telemetry          TelemetryReader
	Topology           TopologyReader
	Flows              FlowReader
	Alerts             AlertManager
	Notifications      NotificationManager
	Jobs               JobManager
	JobTemplates       JobTemplateManager
	Queues             QueueManager
	Accounting         AccountingManager
	Discovery          DiscoveryManager
	Builds             BuildManager
	Access             *identity.AccessService
	Backups            BackupManager
	SharedSpaces       SharedSpaceManager
	Terminal           *terminal.Service
	TelemetryIntervals TelemetryIntervalManager
	ExternalState      bool
	ArtifactRoot       string
	WebRoot            string
}

type Server struct {
	httpServer *http.Server
}

var groupQuotaMu sync.Mutex

func New(config Config) *Server {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	features := []string{
		"health", "version", "native-host-inventory", "node-enrollment", "mtls-node-identity",
		"node-heartbeat", "certificate-rotation", "certificate-revocation", "audit-events", "agent-control-stream",
		"bounded-command-queue", "idempotent-command-acknowledgement", "node-event-stream",
		"native-assignment-transport", "agent-assignment-ledger", "control-stream-certificate-rotation",
		"inventory-snapshot-v1", "resource-observation-v1", "bounded-telemetry-history",
		"topology-observation-v1", "versioned-topology-snapshots", "interface-health-counters-v1",
		"topology-event-stream", "live-topology-health-overlay",
		"duration-gated-interface-alerts", "alert-acknowledgement", "alert-event-stream", "signed-alert-webhook-outbox",
		"optional-ebpf-flow-observation-v1", "bounded-flow-history", "authenticated-flow-api",
		"persistent-job-state", "priority-job-queue", "architecture-resource-scheduler",
		"transactional-resource-reservations", "restart-safe-assignment-replay",
		"cgroup-v2-cpu-memory-enforcement", "dedicated-task-identity", "persistent-job-cancellation",
		"assignment-log-snapshot-v1", "authenticated-job-log-stream", "orphaned-job-reconciliation",
		"durable-queue-policy", "weighted-fair-share", "project-resource-quota", "backfill-scheduling",
		"device-inventory-v1", "device-capacity-accounting", "device-executor-capability-gate",
		"node-taints-and-tolerations", "topology-placement-constraints", "network-capacity-aware-placement",
		"graceful-cordon-and-drain", "numa-topology-inventory", "cpuset-affinity-v1",
		"device-resource-executor-v1", "per-resource-device-capability", "cgroup-device-ebpf-authorization",
		"device-numa-locality", "durable-controlled-preemption", "advisory-elasticity",
		"cumulative-assignment-accounting", "versioned-billing-rates",
	}
	if config.Discovery != nil {
		features = append(features, "allowlisted-node-discovery", "candidate-approval-bootstrap")
	}
	if buildWorkerAvailable(config.Builds) {
		features = append(features, "isolated-native-builds", "multi-step-native-builds", "signed-build-artifacts", "spdx-build-sbom", "rotatable-build-signing-key", "application-build-variant-routing")
	}
	if config.Access != nil {
		features = append(features, "oidc-rbac", "revocable-api-tokens", "local-first-admin",
			"registration-approval", "owned-job-isolation", "user-group-resource-quota")
	}
	if config.JoinTokens != nil {
		features = append(features, "managed-cluster-join-token")
	}
	if config.Backups != nil {
		features = append(features, "verified-online-backups", "offline-restore")
	}
	if serviceAvailable(config.SharedSpaces) {
		features = append(features, "shared-task-spaces", "scoped-node-mounts")
		if config.Jobs != nil {
			features = append(features, "weighted-shared-manifest-batches")
		}
	}
	if config.Terminal != nil {
		features = append(features, "administrator-node-terminal")
	}
	if config.TelemetryIntervals != nil {
		features = append(features, "dynamic-agent-telemetry-interval")
	}
	if config.JobTemplates != nil {
		features = append(features, "administrator-job-templates")
	}
	if config.ArtifactRoot != "" {
		features = append(features, "native-artifact-serving")
	}
	if config.ExternalState {
		features = append(features, "postgresql-external-state-leadership")
	} else {
		features = append(features, "bbolt-single-controller-state")
	}
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /api/v1/version", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, version.Current())
	})
	mux.HandleFunc("GET /api/v1/capabilities", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{
			"phase":    "phase-7-multi-user",
			"features": features,
		})
	})
	if config.Enrollment != nil {
		registerEnrollmentRoutes(mux, config.Enrollment, config.JoinTokens, config.AdminToken, config.Commands, config.NodeEvents)
	}
	if config.Telemetry != nil {
		registerTelemetryRoutes(mux, config.Telemetry, config.AdminToken)
	}
	if config.Topology != nil {
		registerTopologyRoutes(mux, config.Topology, config.Telemetry, config.AdminToken)
	}
	if config.Flows != nil {
		registerFlowRoutes(mux, config.Flows, config.AdminToken)
	}
	if config.Alerts != nil {
		registerAlertRoutes(mux, config.Alerts, config.AdminToken)
	}
	if config.Notifications != nil {
		registerNotificationRoutes(mux, config.Notifications, config.AdminToken)
	}
	if config.Jobs != nil {
		registerJobRoutes(mux, config.Jobs, config.Builds, config.SharedSpaces, config.Access, config.AdminToken)
	}
	if config.JobTemplates != nil {
		registerJobTemplateRoutes(mux, config.JobTemplates, config.AdminToken)
	}
	if config.Queues != nil {
		registerQueueRoutes(mux, config.Queues, config.AdminToken)
	}
	if config.Accounting != nil {
		registerAccountingRoutes(mux, config.Accounting, config.AdminToken)
	}
	if config.Discovery != nil {
		registerDiscoveryRoutes(mux, config.Discovery, config.AdminToken)
	}
	if serviceAvailable(config.Builds) {
		registerBuildRoutes(mux, config.Builds, config.AdminToken)
	}
	if config.Access != nil {
		registerAccessRoutes(mux, config.Access, config.Jobs, config.Terminal, config.AdminToken)
	}
	if config.Backups != nil {
		registerBackupRoutes(mux, config.Backups, config.AdminToken)
	}
	if serviceAvailable(config.SharedSpaces) {
		registerSharedSpaceRoutes(mux, config.SharedSpaces, config.AdminToken)
	}
	if config.Terminal != nil {
		registerTerminalRoutes(mux, config.Terminal, config.AdminToken)
	}
	if config.TelemetryIntervals != nil {
		registerTelemetryIntervalRoutes(mux, config.TelemetryIntervals, config.AdminToken)
	}
	if config.ArtifactRoot != "" {
		registerArtifactRoutes(mux, config.ArtifactRoot, config.Enrollment, config.AdminToken)
	}
	if config.WebRoot != "" {
		registerWebRoutes(mux, config.WebRoot)
	}

	boundedStreams := streamLimitMiddleware(256, 8, 30*time.Minute, mux)
	handler := requestIDMiddleware(securityHeadersMiddleware(rateLimitMiddleware(20, 40, loggingMiddleware(logger, accessMiddleware(config.Access, config.AdminToken, boundedStreams)))))
	return &Server{httpServer: &http.Server{
		Addr:              config.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig:         config.TLSConfig,
	}}
}

// serviceAvailable rejects interfaces containing typed nil pointers. Optional
// services are commonly constructed as pointers before being assigned to Config;
// a disabled service must not become a registered handler that panics on use.
func serviceAvailable(service any) bool {
	if service == nil {
		return false
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

func buildWorkerAvailable(manager BuildManager) bool {
	if !serviceAvailable(manager) {
		return false
	}
	for _, worker := range manager.Workers() {
		if worker.Ready {
			return true
		}
	}
	return false
}

// registerWebRoutes exposes the compiled management console from the same
// origin as the API. Unknown browser routes fall back to index.html so the
// client-side router remains usable after a refresh.
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

func registerBackupRoutes(mux *http.ServeMux, manager BackupManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/system/backups", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		backups, err := manager.List(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": backups})
	}))
	mux.HandleFunc("POST /api/v1/system/backups", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		manifest, err := manager.Create(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, manifest)
	}))
	mux.HandleFunc("POST /api/v1/system/backups/{id}/verify", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		manifest, err := manager.Verify(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, manifest)
	}))
}

func registerTelemetryIntervalRoutes(mux *http.ServeMux, manager TelemetryIntervalManager, adminToken string) {
	writeSettings := func(writer http.ResponseWriter) {
		writeJSON(writer, http.StatusOK, map[string]int64{
			"intervalSeconds": int64(manager.TelemetryInterval() / time.Second),
			"minimumSeconds":  2,
			"maximumSeconds":  60,
		})
	}
	mux.HandleFunc("GET /api/v1/system/telemetry-interval", requireAdmin(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeSettings(writer)
	}))
	mux.HandleFunc("PUT /api/v1/system/telemetry-interval", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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

func registerSharedSpaceRoutes(mux *http.ServeMux, spaces SharedSpaceManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/shared-spaces", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/shared-spaces", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("PUT /api/v1/shared-spaces/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("DELETE /api/v1/shared-spaces/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if err := spaces.Delete(request.Context(), request.PathValue("id")); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/shared-spaces/{id}/unshare", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		if err := spaces.Delete(request.Context(), request.PathValue("id")); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/shared-spaces/{id}/files", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := spaces.ListFiles(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("GET /api/v1/shared-spaces/{id}/files/content", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Query().Get("path")
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(path)))
		if err := spaces.DownloadFile(request.Context(), request.PathValue("id"), path, requestPrincipal(request), writer); err != nil {
			writeError(writer, err)
		}
	}))
	mux.HandleFunc("PUT /api/v1/shared-spaces/{id}/files/content", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		input := http.MaxBytesReader(writer, request.Body, 64<<30)
		if err := spaces.UploadFile(request.Context(), request.PathValue("id"), request.URL.Query().Get("path"), requestPrincipal(request), input); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/shared-spaces/{id}/files/directory", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/shared-spaces/{id}/files/rename", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("DELETE /api/v1/shared-spaces/{id}/files", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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

func registerTerminalRoutes(mux *http.ServeMux, service *terminal.Service, adminToken string) {
	mux.HandleFunc("POST /api/v1/terminal/tickets", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			NodeID string `json:"nodeId"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		ticket, expiresAt, err := service.Issue(request.Context(), input.NodeID, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"ticket": ticket, "expiresAt": expiresAt})
	}))
	mux.HandleFunc("GET /api/v1/terminal/connect", service.Connect)
}

func registerBuildRoutes(mux *http.ServeMux, manager BuildManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/build-retention", requireAdmin(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		policy := manager.RetentionPolicy()
		writeJSON(writer, http.StatusOK, map[string]any{"maximumRecords": policy.MaximumRecords, "maximumAgeSeconds": int64(policy.MaximumAge / time.Second), "preserveSuccessfulBuilds": policy.PreserveSuccessfulBuilds})
	}))
	mux.HandleFunc("PUT /api/v1/build-retention", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/builds/cleanup", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		result, err := manager.Cleanup(request.Context(), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}))
	mux.HandleFunc("GET /api/v1/build-signing-key", requireAdmin(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, manager.SigningKey())
	}))
	mux.HandleFunc("POST /api/v1/build-signing-key/rotate", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		key, err := manager.RotateSigningKey(request.Context(), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, key)
	}))
	mux.HandleFunc("GET /api/v1/build-workers", requireAdmin(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"items": manager.Workers()})
	}))
	mux.HandleFunc("GET /api/v1/builds", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/builds", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Name                string            `json:"name"`
			Application         string            `json:"application"`
			Variant             string            `json:"variant"`
			RuntimeNodeIDs      []string          `json:"runtimeNodeIds"`
			RuntimeEntrypoint   string            `json:"runtimeEntrypoint"`
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
			RuntimeEntrypoint: payload.RuntimeEntrypoint,
			Architecture:      payload.Architecture, Command: payload.Command, Steps: payload.Steps, OutputPath: payload.OutputPath,
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
	mux.HandleFunc("GET /api/v1/builds/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/builds/{id}/cancel", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		item, err := manager.Cancel(request.Context(), request.PathValue("id"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, item)
	}))
	mux.HandleFunc("PUT /api/v1/builds/{id}/runtime-targets", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("DELETE /api/v1/builds/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		result, err := manager.Delete(request.Context(), request.PathValue("id"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, result)
	}))
}

func registerAccessRoutes(mux *http.ServeMux, service *identity.AccessService, jobs JobManager, terminals *terminal.Service, adminToken string) {
	mux.HandleFunc("GET /api/v1/setup/status", func(writer http.ResponseWriter, request *http.Request) {
		required, err := service.SetupRequired(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]bool{"setupRequired": required})
	})
	mux.HandleFunc("POST /api/v1/setup/admin", func(writer http.ResponseWriter, request *http.Request) {
		var payload identity.LocalCredentials
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		user, err := service.SetupAdmin(request.Context(), payload)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, user)
	})
	mux.HandleFunc("POST /api/v1/auth/register", func(writer http.ResponseWriter, request *http.Request) {
		var payload identity.LocalCredentials
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		user, err := service.Register(request.Context(), payload)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, user)
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		session, err := service.Login(request.Context(), payload.Username, payload.Password)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, session)
	})
	mux.HandleFunc("GET /api/v1/access/me", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, requestPrincipal(request))
	}))
	mux.HandleFunc("GET /api/v1/access/roles", requireAdmin(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"items": service.Roles()})
	}))
	mux.HandleFunc("GET /api/v1/access/tokens", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		tokens, err := service.ListTokens(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": tokens})
	}))
	mux.HandleFunc("GET /api/v1/access/users", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		users, err := service.ListUsers(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": users})
	}))
	mux.HandleFunc("PATCH /api/v1/access/users/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Status   domain.UserStatus `json:"status,omitempty"`
			Role     string            `json:"role,omitempty"`
			GroupIDs []string          `json:"groupIds,omitempty"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		user, err := service.UpdateUser(request.Context(), request.PathValue("id"), payload.Status, payload.Role, requestPrincipal(request))
		if err == nil && payload.GroupIDs != nil {
			user, err = service.PutUserGroups(request.Context(), request.PathValue("id"), payload.GroupIDs, requestPrincipal(request))
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, user)
	}))
	mux.HandleFunc("GET /api/v1/access/groups", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		groups, err := service.ListUserGroups(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": groups})
	}))
	mux.HandleFunc("GET /api/v1/access/settings", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		settings, err := service.GetAccessSettings(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, settings)
	}))
	mux.HandleFunc("PUT /api/v1/access/settings", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			DefaultUserGroupID string `json:"defaultUserGroupId"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		settings, err := service.PutAccessSettings(request.Context(), payload.DefaultUserGroupID, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, settings)
	}))
	mux.HandleFunc("POST /api/v1/access/groups", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload domain.UserGroup
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		group, err := service.CreateUserGroup(request.Context(), payload, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, group)
	}))
	mux.HandleFunc("PUT /api/v1/access/groups/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload domain.UserGroup
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		previous, err := service.GetUserGroup(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		actor := requestPrincipal(request)
		group, err := service.UpdateUserGroup(request.Context(), request.PathValue("id"), payload, actor)
		if err == nil {
			if updater, ok := jobs.(UserStorageQuotaUpdater); ok {
				var users []domain.User
				users, err = service.ListUsers(request.Context())
				for _, user := range users {
					if err != nil || !containsString(user.GroupIDs, group.ID) {
						continue
					}
					var policy identity.EffectiveGroupPolicy
					policy, err = service.ResolveUserGroupPolicy(request.Context(), user.GroupIDs)
					if err == nil {
						err = updater.UpdateUserStorageQuota(request.Context(), user.ID, policy.StorageQuotaBytes)
					}
				}
			} else if updater, ok := jobs.(StorageQuotaUpdater); ok {
				err = updater.UpdateGroupStorageQuota(request.Context(), group.ID, group.StorageQuotaBytes)
			}
		}
		if err == nil {
			if updater, ok := jobs.(GroupDisableUpdater); ok {
				if group.Disabled && group.DisableSuspendQueued && (!previous.Disabled || !previous.DisableSuspendQueued) {
					err = updater.SetGroupDisabled(request.Context(), group.ID, true)
				} else if previous.DisableSuspendQueued && (!group.Disabled || !group.DisableSuspendQueued) {
					err = updater.SetGroupDisabled(request.Context(), group.ID, false)
				}
			}
		}
		if err == nil && group.Disabled && group.DisableEndSessions && (!previous.Disabled || !previous.DisableEndSessions) {
			var userIDs []string
			userIDs, _, err = service.RevokeGroupSessions(request.Context(), group.ID, actor)
			if err == nil && terminals != nil {
				terminals.TerminateUsers(userIDs)
			}
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, group)
	}))
	mux.HandleFunc("DELETE /api/v1/access/groups/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		groupID := request.PathValue("id")
		previous, err := service.GetUserGroup(request.Context(), groupID)
		if err != nil {
			writeError(writer, err)
			return
		}
		affected, err := service.DeleteUserGroup(request.Context(), groupID, requestPrincipal(request))
		if err == nil && previous.DisableSuspendQueued {
			if updater, ok := jobs.(GroupDisableUpdater); ok {
				err = updater.SetGroupDisabled(request.Context(), groupID, false)
			}
		}
		if err == nil {
			if updater, ok := jobs.(UserStorageQuotaUpdater); ok {
				for _, user := range affected {
					policy, policyErr := service.ResolveUserGroupPolicy(request.Context(), user.GroupIDs)
					if policyErr != nil {
						err = policyErr
						break
					}
					if err = updater.UpdateUserStorageQuota(request.Context(), user.ID, policy.StorageQuotaBytes); err != nil {
						break
					}
				}
			} else if updater, ok := jobs.(StorageQuotaUpdater); ok {
				err = updater.UpdateGroupStorageQuota(request.Context(), groupID, 0)
			}
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writer.Header().Set("X-Xirang-Detached-Users", strconv.Itoa(len(affected)))
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/access/tokens", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Name       string `json:"name"`
			Role       string `json:"role"`
			TTLSeconds int64  `json:"ttlSeconds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		created, err := service.CreateToken(request.Context(), payload.Name, payload.Role, time.Duration(payload.TTLSeconds)*time.Second, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, created)
	}))
	mux.HandleFunc("POST /api/v1/access/tokens/{id}/revoke", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		token, err := service.RevokeToken(request.Context(), request.PathValue("id"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, token)
	}))
}

func accessMiddleware(service *identity.AccessService, bootstrapToken string, next http.Handler) http.Handler {
	if service == nil {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if publicIdentityRoute(request.Method, request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		header := request.Header.Get("Authorization")
		if header == "" || (bootstrapToken != "" && identity.AuthenticateBearer(header, bootstrapToken)) {
			next.ServeHTTP(writer, request)
			return
		}
		bearer, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || bearer == "" {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "invalid bearer credential"})
			return
		}
		principal, err := service.Authenticate(request.Context(), bearer)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, identity.ErrForbidden) {
				status = http.StatusForbidden
			}
			writeJSON(writer, status, map[string]string{"error": err.Error()})
			return
		}
		if err := service.Authorize(principal, requiredPermission(request.Method, request.URL.Path)); err != nil {
			writeJSON(writer, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		next.ServeHTTP(writer, request.WithContext(identity.WithPrincipal(request.Context(), principal)))
	})
}

func publicIdentityRoute(method, path string) bool {
	return (method == http.MethodGet && path == "/api/v1/setup/status") ||
		(method == http.MethodPost && (path == "/api/v1/setup/admin" || path == "/api/v1/auth/register" || path == "/api/v1/auth/login"))
}

func requiredPermission(method, path string) string {
	if strings.HasPrefix(path, "/artifacts/") {
		return identity.PermissionRead
	}
	if strings.HasPrefix(path, "/api/v1/shared-spaces/") && strings.Contains(path, "/files") {
		if method != http.MethodGet && method != http.MethodHead {
			return identity.PermissionFilesWrite
		}
		return identity.PermissionRead
	}
	if strings.HasPrefix(path, "/api/v1/terminal") {
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/job-templates") {
		if method == http.MethodGet {
			return identity.PermissionRead
		}
		return identity.PermissionJobTemplates
	}
	if strings.HasPrefix(path, "/api/v1/access/") {
		if path == "/api/v1/access/me" {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/system/") {
		if method == http.MethodGet && path == "/api/v1/system/telemetry-interval" {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/build-signing-key") {
		if method == http.MethodGet {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if path == "/api/v1/build-retention" || path == "/api/v1/builds/cleanup" {
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/audit") {
		return identity.PermissionAuditRead
	}
	if strings.HasPrefix(path, "/api/v1/accounting") {
		if method == http.MethodGet {
			return identity.PermissionRead
		}
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/enrollment/join-token") {
		return identity.PermissionAccessManage
	}
	if strings.HasPrefix(path, "/api/v1/discovery") || path == "/api/v1/assignments" {
		return identity.PermissionNodesManage
	}
	if strings.HasPrefix(path, "/api/v1/build") {
		return identity.PermissionBuildsManage
	}
	if strings.HasPrefix(path, "/api/v1/queues") || path == "/api/v1/elasticity" {
		return identity.PermissionQueuesManage
	}
	if method == http.MethodGet {
		return identity.PermissionRead
	}
	switch {
	case strings.HasPrefix(path, "/api/v1/build"):
		return identity.PermissionBuildsManage
	case strings.HasPrefix(path, "/api/v1/jobs"):
		return identity.PermissionJobsManage
	case strings.HasPrefix(path, "/api/v1/queues"):
		return identity.PermissionQueuesManage
	default:
		return identity.PermissionNodesManage
	}
}

// streamLimitMiddleware bounds long-lived browser event streams separately
// from the short-request rate limiter. Browsers reconnect automatically when
// the finite lifetime expires, preventing stale tabs from retaining resources
// forever while preserving the live-update contract.
func streamLimitMiddleware(globalLimit, perPrincipal int, lifetime time.Duration, next http.Handler) http.Handler {
	if globalLimit < 1 || perPrincipal < 1 || lifetime <= 0 {
		return next
	}
	var mu sync.Mutex
	global := 0
	active := make(map[string]int)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !eventStreamRoute(request.Method, request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		key := request.RemoteAddr
		if principal, ok := identity.PrincipalFromContext(request.Context()); ok {
			key = principalIdentity(principal)
		}
		if host, _, err := net.SplitHostPort(key); err == nil {
			key = host
		}
		mu.Lock()
		if global >= globalLimit || active[key] >= perPrincipal {
			mu.Unlock()
			writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "event stream limit reached"})
			return
		}
		global++
		active[key]++
		mu.Unlock()
		defer func() {
			mu.Lock()
			global--
			active[key]--
			if active[key] == 0 {
				delete(active, key)
			}
			mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(request.Context(), lifetime)
		defer cancel()
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func eventStreamRoute(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	return path == "/api/v1/events/nodes" || path == "/api/v1/events/topology" || path == "/api/v1/events/alerts" ||
		(strings.HasPrefix(path, "/api/v1/jobs/") && strings.HasSuffix(path, "/logs/stream"))
}

func requestPrincipal(request *http.Request) domain.Principal {
	if principal, ok := identity.PrincipalFromContext(request.Context()); ok {
		return principal
	}
	return domain.Principal{
		Subject: "bootstrap-admin", DisplayName: "Bootstrap administrator", Role: "admin",
		Permissions: []string{"*"}, AuthMethod: "bootstrap",
	}
}

func registerDiscoveryRoutes(mux *http.ServeMux, manager DiscoveryManager, adminToken string) {
	mux.HandleFunc("POST /api/v1/discovery/scans", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/discovery/scans", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/discovery/candidates", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		candidates, err := manager.ListCandidates(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": candidates})
	}))
	mux.HandleFunc("POST /api/v1/discovery/candidates/{id}/approve", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/discovery/candidates/{id}/reject", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		candidate, err := manager.Reject(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, candidate)
	}))
}

func registerQueueRoutes(mux *http.ServeMux, queues QueueManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/elasticity", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		snapshot, err := queues.Elasticity(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, snapshot)
	}))
	mux.HandleFunc("GET /api/v1/queues", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		policies, err := queues.ListQueuePolicies(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": policies})
	}))
	mux.HandleFunc("PUT /api/v1/queues/{name}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Weight                  int32                 `json:"weight"`
			MaxConcurrent           int32                 `json:"maxConcurrent"`
			MaxConcurrentPerProject int32                 `json:"maxConcurrentPerProject"`
			ResourceQuota           domain.ResourceVector `json:"resourceQuota"`
			ProjectResourceQuota    domain.ResourceVector `json:"projectResourceQuota"`
			Backfill                *bool                 `json:"backfill"`
			AllowPreemption         bool                  `json:"allowPreemption"`
			Preemptible             bool                  `json:"preemptible"`
			PreemptionGraceSeconds  int32                 `json:"preemptionGraceSeconds"`
			MaxPreemptionsPerJob    int32                 `json:"maxPreemptionsPerJob"`
			MaxRecommendedScaleOut  int32                 `json:"maxRecommendedScaleOut"`
			ElasticityBudget        domain.ResourceVector `json:"elasticityBudget"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		backfill := true
		if payload.Backfill != nil {
			backfill = *payload.Backfill
		}
		policy, err := queues.PutQueuePolicy(request.Context(), domain.QueuePolicy{
			Name: request.PathValue("name"), Weight: payload.Weight, MaxConcurrent: payload.MaxConcurrent,
			MaxConcurrentPerProject: payload.MaxConcurrentPerProject, ResourceQuota: payload.ResourceQuota,
			ProjectResourceQuota: payload.ProjectResourceQuota, Backfill: backfill,
			AllowPreemption: payload.AllowPreemption, Preemptible: payload.Preemptible,
			PreemptionGraceSeconds: payload.PreemptionGraceSeconds, MaxPreemptionsPerJob: payload.MaxPreemptionsPerJob,
			MaxRecommendedScaleOut: payload.MaxRecommendedScaleOut, ElasticityBudget: payload.ElasticityBudget,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, policy)
	}))
}

type submitJobRequest struct {
	ProjectID            string                         `json:"projectId"`
	Queue                string                         `json:"queue"`
	Priority             int32                          `json:"priority"`
	MaxAttempts          int32                          `json:"maxAttempts"`
	Artifact             domain.NativeArtifact          `json:"artifact"`
	Application          string                         `json:"application,omitempty"`
	ArtifactVariants     []domain.NativeArtifactVariant `json:"-"`
	Arguments            []string                       `json:"arguments,omitempty"`
	WorkingDirectory     string                         `json:"workingDirectory,omitempty"`
	Environment          map[string]string              `json:"environment,omitempty"`
	Resources            domain.ResourceVector          `json:"resources"`
	RequiredArchitecture string                         `json:"requiredArchitecture,omitempty"`
	RequiredNodeID       string                         `json:"requiredNodeId,omitempty"`
	RequiredLabels       map[string]string              `json:"requiredLabels,omitempty"`
	RequiredTopology     map[string]string              `json:"requiredTopology,omitempty"`
	ExcludedNodeIDs      []string                       `json:"excludedNodeIds,omitempty"`
	Tolerations          []domain.Toleration            `json:"tolerations,omitempty"`
	PreferredZone        string                         `json:"preferredZone,omitempty"`
	MinimumNetworkBits   int64                          `json:"minimumNetworkBits,omitempty"`
	CPUSet               string                         `json:"cpuSet,omitempty"`
	NUMANodes            string                         `json:"numaNodes,omitempty"`
	Parallel             domain.ParallelSpec            `json:"parallel,omitempty"`
	TimeoutSeconds       int64                          `json:"timeoutSeconds,omitempty"`
	ArrayID              string                         `json:"arrayId,omitempty"`
	ArrayIndex           int32                          `json:"arrayIndex,omitempty"`
	ArraySize            int32                          `json:"arraySize,omitempty"`
	Dependencies         []domain.JobDependency         `json:"dependencies,omitempty"`
}

type jobTemplateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Script      string `json:"script"`
	Revision    uint64 `json:"revision,omitempty"`
}

func registerJobTemplateRoutes(mux *http.ServeMux, templates JobTemplateManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/job-templates", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := templates.List(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("POST /api/v1/job-templates", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload jobTemplateRequest
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		if payload.Revision != 0 {
			writeError(writer, jobtemplate.ErrInvalid)
			return
		}
		template, err := templates.Create(request.Context(), payload.Name, payload.Description, payload.Script, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, template)
	}))
	mux.HandleFunc("GET /api/v1/job-templates/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		template, err := templates.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, template)
	}))
	mux.HandleFunc("PUT /api/v1/job-templates/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload jobTemplateRequest
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		template, err := templates.Update(
			request.Context(), request.PathValue("id"), payload.Revision,
			payload.Name, payload.Description, payload.Script, requestPrincipal(request),
		)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, template)
	}))
	mux.HandleFunc("DELETE /api/v1/job-templates/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		revision, err := strconv.ParseUint(request.URL.Query().Get("revision"), 10, 64)
		if err != nil || revision == 0 {
			writeError(writer, jobtemplate.ErrInvalid)
			return
		}
		if err := templates.Delete(request.Context(), request.PathValue("id"), revision, requestPrincipal(request)); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
}

func registerJobRoutes(mux *http.ServeMux, jobs JobManager, builds BuildManager, shared SharedSpaceManager, access *identity.AccessService, adminToken string) {
	mux.HandleFunc("GET /api/v1/jobs", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 100
		if raw := request.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1000 {
				writeError(writer, fmt.Errorf("%w: limit must be between 1 and 1000", scheduler.ErrInvalidJob))
				return
			}
			limit = parsed
		}
		items, err := jobs.List(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		principal := requestPrincipal(request)
		if principal.Role != "admin" {
			filtered := items[:0]
			for _, job := range items {
				if ownsJob(principal, job) {
					filtered = append(filtered, job)
				}
			}
			items = filtered
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	resolveApplication := func(ctx context.Context, payload *submitJobRequest) error {
		if payload.Application == "" || len(payload.ArtifactVariants) > 0 {
			return nil
		}
		resolver, ok := builds.(applicationBuildResolver)
		if !ok {
			return fmt.Errorf("%w: application build routing is unavailable", scheduler.ErrInvalidJob)
		}
		variants, err := resolver.ResolveApplication(ctx, payload.Application)
		if err != nil {
			return fmt.Errorf("%w: application %q has no runnable build mapping: %v", scheduler.ErrInvalidJob, payload.Application, err)
		}
		payload.Artifact = domain.NativeArtifact{}
		payload.ArtifactVariants = variants
		return nil
	}
	submitOne := func(request *http.Request, payload submitJobRequest) (domain.Job, error) {
		if err := resolveApplication(request.Context(), &payload); err != nil {
			return domain.Job{}, err
		}
		principal := requestPrincipal(request)
		projectID, groupID := payload.ProjectID, ""
		policyGroupIDs := []string(nil)
		var storageQuotaBytes int64
		maxJobTimeoutSeconds := int64(0)
		if principal.Role != "admin" {
			maxJobTimeoutSeconds = domain.DefaultMaxJobTimeoutSeconds
			if len(principal.GroupIDs) > 0 && access != nil {
				groupQuotaMu.Lock()
				defer groupQuotaMu.Unlock()
				policy, groupErr := access.ResolveUserGroupPolicy(request.Context(), principal.GroupIDs)
				if groupErr != nil {
					return domain.Job{}, groupErr
				}
				if policy.Disabled {
					return domain.Job{}, fmt.Errorf("%w: one or more user groups are temporarily disabled", identity.ErrForbidden)
				}
				active, listErr := jobs.List(request.Context(), 10_000)
				if listErr != nil {
					return domain.Job{}, listErr
				}
				if err := enforceGroupQuota(policy, active, principalIdentity(principal), payload.Resources); err != nil {
					return domain.Job{}, err
				}
				groupID, policyGroupIDs = policy.PrimaryGroupID, policy.GroupIDs
				storageQuotaBytes, maxJobTimeoutSeconds = policy.StorageQuotaBytes, policy.MaxJobTimeoutSeconds
			}
			projectID = groupID
			if projectID == "" {
				projectID = "user-" + principalIdentity(principal)
			}
		}
		effectiveTimeout, timeoutErr := effectiveJobTimeoutSeconds(payload.TimeoutSeconds, maxJobTimeoutSeconds, principal.Role == "admin")
		if timeoutErr != nil {
			return domain.Job{}, timeoutErr
		}
		payload.TimeoutSeconds = effectiveTimeout
		job, err := jobs.Submit(request.Context(), scheduler.SubmitRequest{
			OwnerID: principalIdentity(principal), OwnerName: principal.DisplayName, GroupID: groupID, PolicyGroupIDs: policyGroupIDs,
			ProjectID: projectID, Queue: payload.Queue, Priority: payload.Priority, MaxAttempts: payload.MaxAttempts,
			StorageQuotaBytes: storageQuotaBytes,
			ArrayID:           payload.ArrayID, ArrayIndex: payload.ArrayIndex, ArraySize: payload.ArraySize,
			Dependencies: payload.Dependencies, AllowCrossOwnerDependencies: principal.Role == "admin",
			Task: domain.TaskSpec{
				Artifact: payload.Artifact, Application: payload.Application, ArtifactVariants: payload.ArtifactVariants,
				Arguments: payload.Arguments, WorkingDirectory: payload.WorkingDirectory,
				Environment: payload.Environment, Resources: payload.Resources,
				Placement: domain.Placement{
					RequiredArchitecture: payload.RequiredArchitecture, RequiredNodeID: payload.RequiredNodeID, RequiredLabels: payload.RequiredLabels,
					RequiredTopology: payload.RequiredTopology, ExcludedNodeIDs: payload.ExcludedNodeIDs,
					Tolerations: payload.Tolerations, PreferredZone: payload.PreferredZone, MinimumNetworkBits: payload.MinimumNetworkBits,
					CPUSet: payload.CPUSet, NUMANodes: payload.NUMANodes,
				},
				Parallel: payload.Parallel,
				Timeout:  time.Duration(payload.TimeoutSeconds) * time.Second,
			},
		})
		return job, err
	}
	mux.HandleFunc("POST /api/v1/jobs/script", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Script string `json:"script"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		spec, err := jobspec.Parse(strings.NewReader(input.Script))
		if err != nil {
			writeError(writer, fmt.Errorf("%w: %v", scheduler.ErrInvalidJob, err))
			return
		}
		base := submitJobRequest{
			ProjectID: spec.ProjectID, Queue: spec.Queue, Priority: spec.Priority, MaxAttempts: spec.MaxAttempts,
			Artifact: spec.Artifact, Application: spec.Application, Arguments: spec.Arguments, WorkingDirectory: spec.WorkingDirectory,
			Environment: spec.Environment, Resources: spec.Resources, RequiredArchitecture: spec.Artifact.Architecture,
			Parallel: spec.Parallel, TimeoutSeconds: int64(spec.Timeout.Seconds()), Dependencies: spec.Dependencies,
		}
		if err := resolveApplication(request.Context(), &base); err != nil {
			writeError(writer, err)
			return
		}
		indices := spec.ArrayIndices
		items := []string(nil)
		pinnedNodes := []string(nil)
		if spec.BatchPath != "" {
			if shared == nil {
				writeError(writer, fmt.Errorf("%w: shared storage is unavailable", scheduler.ErrInvalidJob))
				return
			}
			content := &limitedTextWriter{limit: 1 << 20}
			if err := shared.DownloadFile(request.Context(), spec.BatchSpaceID, spec.BatchPath, requestPrincipal(request), content); err != nil {
				writeError(writer, err)
				return
			}
			for _, line := range strings.Split(content.String(), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if len(line) > 4096 || strings.ContainsRune(line, '\x00') || len(items) == 1000 {
					writeError(writer, fmt.Errorf("%w: batch manifest must contain at most 1000 items of at most 4096 bytes", scheduler.ErrInvalidJob))
					return
				}
				items = append(items, line)
			}
			if len(items) == 0 {
				writeError(writer, fmt.Errorf("%w: batch manifest has no work items", scheduler.ErrInvalidJob))
				return
			}
			planner, ok := jobs.(batchJobPlanner)
			if !ok {
				writeError(writer, fmt.Errorf("%w: weighted batch planning is unavailable", scheduler.ErrInvalidJob))
				return
			}
			planTask := domain.TaskSpec{Artifact: base.Artifact, Application: base.Application, ArtifactVariants: base.ArtifactVariants,
				Arguments: base.Arguments, WorkingDirectory: base.WorkingDirectory, Environment: base.Environment,
				Resources: base.Resources, Parallel: base.Parallel}
			pinnedNodes, err = planner.PlanBatch(request.Context(), planTask, len(items), spec.BatchNodes)
			if err != nil {
				writeError(writer, err)
				return
			}
			indices = make([]int32, len(items))
			for index := range indices {
				indices[index] = int32(index)
			}
		} else if len(indices) == 0 {
			indices = []int32{0}
		}
		arrayID := ""
		if len(spec.ArrayIndices) > 0 || len(items) > 0 {
			value := make([]byte, 12)
			if _, err := rand.Read(value); err != nil {
				writeError(writer, err)
				return
			}
			prefix := "array_"
			if len(items) > 0 {
				prefix = "batch_"
			}
			arrayID = prefix + hex.EncodeToString(value)
		}
		created := make([]domain.Job, 0, len(indices))
		for _, index := range indices {
			payload := base
			payload.Arguments = append([]string(nil), base.Arguments...)
			payload.ArrayID, payload.ArrayIndex = arrayID, index
			if len(spec.ArrayIndices) > 0 || len(items) > 0 {
				payload.ArraySize = int32(len(indices))
			}
			if len(items) > 0 {
				if spec.BatchArgument != "" {
					payload.Arguments = append(payload.Arguments, spec.BatchArgument)
				}
				payload.Arguments = append(payload.Arguments, items[index])
				payload.RequiredNodeID = pinnedNodes[index]
			}
			job, err := submitOne(request, payload)
			if err != nil {
				writeError(writer, err)
				return
			}
			created = append(created, job)
		}
		if len(spec.ArrayIndices) == 0 && len(items) == 0 {
			writeJSON(writer, http.StatusCreated, created[0])
			return
		}
		response := map[string]any{"arrayId": arrayID, "items": created}
		if len(items) > 0 {
			response["batchId"] = arrayID
			response["nodePlan"] = pinnedNodes
		}
		writeJSON(writer, http.StatusCreated, response)
	}))
	mux.HandleFunc("POST /api/v1/jobs", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload submitJobRequest
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		job, err := submitOne(request, payload)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, job)
	}))
	mux.HandleFunc("GET /api/v1/jobs/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		job, err := jobs.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessJob(requestPrincipal(request), job) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		writeJSON(writer, http.StatusOK, job)
	}))
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs/stream", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		job, err := jobs.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessJob(requestPrincipal(request), job) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		_ = http.NewResponseController(writer).SetWriteDeadline(time.Time{})
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache, no-transform")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		lastUpdate := time.Time{}
		send := func(current domain.Job) bool {
			if !current.UpdatedAt.After(lastUpdate) {
				return true
			}
			encoded, err := json.Marshal(current)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(writer, "event: logs\ndata: %s\n\n", encoded); err != nil {
				return false
			}
			lastUpdate = current.UpdatedAt
			flusher.Flush()
			return true
		}
		if !send(job) {
			return
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-ticker.C:
				current, err := jobs.Get(request.Context(), request.PathValue("id"))
				if err != nil || !send(current) {
					return
				}
			}
		}
	}))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		existing, err := jobs.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessJob(requestPrincipal(request), existing) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		var payload struct {
			Reason             string `json:"reason"`
			GracePeriodSeconds *int64 `json:"gracePeriodSeconds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		graceSeconds := int64(5)
		if payload.GracePeriodSeconds != nil {
			graceSeconds = *payload.GracePeriodSeconds
		}
		if graceSeconds < 0 || graceSeconds > 300 {
			writeError(writer, fmt.Errorf("%w: gracePeriodSeconds must be between 0 and 300", scheduler.ErrInvalidJob))
			return
		}
		job, err := jobs.Cancel(request.Context(), request.PathValue("id"), scheduler.CancelRequest{
			Reason: payload.Reason, GracePeriod: time.Duration(graceSeconds) * time.Second,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, job)
	}))
}

func principalIdentity(principal domain.Principal) string {
	if principal.UserID != "" {
		return principal.UserID
	}
	return principal.Subject
}

func ownsJob(principal domain.Principal, job domain.Job) bool {
	return job.OwnerID != "" && subtle.ConstantTimeCompare([]byte(job.OwnerID), []byte(principalIdentity(principal))) == 1
}

func canAccessJob(principal domain.Principal, job domain.Job) bool {
	return principal.Role == "admin" || ownsJob(principal, job)
}

func canAccessBuild(principal domain.Principal, build domain.Build) bool {
	return principal.Role == "admin" || build.OwnerID != "" && subtle.ConstantTimeCompare([]byte(build.OwnerID), []byte(principalIdentity(principal))) == 1
}

func enforceGroupQuota(policy identity.EffectiveGroupPolicy, jobs []domain.Job, ownerID string, requested domain.ResourceVector) error {
	var used domain.ResourceVector
	var concurrent int32
	for _, job := range jobs {
		if job.OwnerID != ownerID || job.Status.Terminal() {
			continue
		}
		concurrent++
		used = used.Add(job.Task.Resources)
	}
	if policy.MaxConcurrentJobs > 0 && concurrent >= policy.MaxConcurrentJobs {
		return fmt.Errorf("%w: user group concurrent job quota reached", scheduler.ErrQuotaExceeded)
	}
	total := used.Add(requested)
	quota := policy.ResourceQuota
	if (quota.MilliCPU > 0 && total.MilliCPU > quota.MilliCPU) ||
		(quota.MemoryBytes > 0 && total.MemoryBytes > quota.MemoryBytes) {
		return fmt.Errorf("%w: user group active resource quota exceeded", scheduler.ErrQuotaExceeded)
	}
	for name, quantity := range total.Devices {
		if quota.Devices[name] > 0 && quantity > quota.Devices[name] {
			return fmt.Errorf("%w: user group device quota exceeded", scheduler.ErrQuotaExceeded)
		}
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
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
	mux.HandleFunc("GET /api/v1/accounting/summary", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/accounting/users", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/accounting/rates", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := manager.Rates(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("POST /api/v1/accounting/rates", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/flows", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := flows.ListLatest(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("GET /api/v1/nodes/{id}/flows", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/alert-deliveries", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/alert-deliveries/{id}/retry", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		delivery, err := notifications.Retry(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, delivery)
	}))
}

func registerAlertRoutes(mux *http.ServeMux, alerts AlertManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/alerts", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/alerts/{id}/acknowledge", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		alert, err := alerts.Acknowledge(request.Context(), request.PathValue("id"), "bootstrap-admin")
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, alert)
	}))
	mux.HandleFunc("GET /api/v1/events/alerts", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/topology", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/events/topology", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/nodes/{id}/inventory", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		inventory, err := reader.GetInventory(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, inventory)
	}))
	mux.HandleFunc("GET /api/v1/nodes/{id}/observations", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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

func (s *Server) ListenAndServe() error { return s.httpServer.ListenAndServe() }

func (s *Server) ListenAndServeTLS(certificateFile, keyFile string) error {
	return s.httpServer.ListenAndServeTLS(certificateFile, keyFile)
}

func (s *Server) Shutdown(ctx context.Context) error { return s.httpServer.Shutdown(ctx) }

func (s *Server) HTTPHandler() http.Handler { return s.httpServer.Handler }

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestID := request.Header.Get("X-Request-ID")
		if requestID == "" {
			buffer := make([]byte, 16)
			if _, err := rand.Read(buffer); err == nil {
				requestID = hex.EncodeToString(buffer)
			}
		}
		if requestID != "" {
			writer.Header().Set("X-Request-ID", requestID)
		}
		next.ServeHTTP(writer, request)
	})
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		writer.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self' data:; font-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; connect-src 'self' ws: wss:")
		if request.TLS != nil {
			writer.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(writer, request)
	})
}

func rateLimitMiddleware(rate, burst float64, next http.Handler) http.Handler {
	type bucket struct {
		tokens  float64
		updated time.Time
	}
	var mu sync.Mutex
	buckets := map[string]bucket{}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err != nil || host == "" {
			host = request.RemoteAddr
		}
		if host == "" {
			host = "local"
		}
		now := time.Now()
		mu.Lock()
		current := buckets[host]
		if current.updated.IsZero() {
			current.tokens = burst
		} else {
			current.tokens += now.Sub(current.updated).Seconds() * rate
		}
		if current.tokens > burst {
			current.tokens = burst
		}
		current.updated = now
		allowed := current.tokens >= 1
		if allowed {
			current.tokens--
		}
		buckets[host] = current
		if len(buckets) > 10_000 {
			for key, value := range buckets {
				if now.Sub(value.updated) > 10*time.Minute {
					delete(buckets, key)
				}
			}
		}
		mu.Unlock()
		if !allowed {
			writer.Header().Set("Retry-After", "1")
			writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "management API rate limit exceeded"})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(writer, request)
		logger.Info("http request", "method", request.Method, "path", request.URL.Path, "duration", time.Since(started))
	})
}

type createTokenRequest struct {
	Name            string `json:"name"`
	TTLSeconds      int64  `json:"ttlSeconds"`
	MaxUses         uint32 `json:"maxUses"`
	RequireApproval bool   `json:"requireApproval"`
}

type registerNodeRequest struct {
	Token     string                  `json:"token"`
	CSRPEM    string                  `json:"csrPem"`
	Inventory domain.InventorySummary `json:"inventory"`
}

type rotateCertificateRequest struct {
	CSRPEM string `json:"csrPem"`
}

type nodeModeRequest struct {
	Mode   string `json:"mode"`
	Reason string `json:"reason"`
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
		mux.HandleFunc("GET /api/v1/enrollment/join-token", requireAdmin(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, http.StatusOK, joinTokens.View())
		}))
		mux.HandleFunc("POST /api/v1/enrollment/join-token/reset", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
			created, err := joinTokens.Reset(request.Context())
			if err != nil {
				writeError(writer, err)
				return
			}
			writeJSON(writer, http.StatusCreated, created)
		}))
	}
	mux.HandleFunc("POST /api/v1/enrollment/tokens", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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

	mux.HandleFunc("GET /api/v1/nodes", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		nodes, err := service.ListNodes(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": nodes})
	}))

	mux.HandleFunc("GET /api/v1/nodes/{id}", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		node, err := service.GetNode(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, node)
	}))

	mux.HandleFunc("POST /api/v1/nodes/{id}/approve", requireAdmin(adminToken, nodeAction(service.Approve)))
	mux.HandleFunc("POST /api/v1/nodes/{id}/reject", requireAdmin(adminToken, nodeAction(service.Reject)))
	mux.HandleFunc("PUT /api/v1/nodes/{id}/taints", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("PUT /api/v1/nodes/{id}/configuration", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/nodes/{id}/mode", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("POST /api/v1/nodes/{id}/certificate/revoke", requireAdmin(adminToken, nodeAction(service.RevokeCertificate)))
	mux.HandleFunc("POST /api/v1/nodes/{id}/certificate/rotate-command", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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
	mux.HandleFunc("GET /api/v1/audit", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		events, err := service.ListAuditEvents(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": events})
	}))
	mux.HandleFunc("GET /api/v1/assignments", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		reports, err := service.ListAssignmentReports(request.Context(), limit)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": reports})
	}))
	if events != nil {
		mux.HandleFunc("GET /api/v1/events/nodes", requireAdmin(adminToken, func(writer http.ResponseWriter, request *http.Request) {
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

func nodeAction(action func(context.Context, string) (domain.Node, error)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		node, err := action(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, node)
	}
}

func requireAdmin(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		_, hasPrincipal := identity.PrincipalFromContext(request.Context())
		if !hasPrincipal && (token == "" || !identity.AuthenticateBearer(request.Header.Get("Authorization"), token)) {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "administrator authentication required"})
			return
		}
		next(writer, request)
	}
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	defer request.Body.Close()
	reader := http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: malformed JSON: %v", enrollment.ErrInvalidRequest, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: request must contain one JSON value", enrollment.ErrInvalidRequest)
	}
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, scheduler.ErrUnsatisfiable):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, scheduler.ErrInvalidJob):
		status = http.StatusBadRequest
	case errors.Is(err, enrollment.ErrInvalidRequest), errors.Is(err, discovery.ErrInvalidRequest), errors.Is(err, buildmanager.ErrInvalidRequest), errors.Is(err, identity.ErrInvalid), errors.Is(err, backup.ErrInvalid), errors.Is(err, clusterops.ErrInvalid), errors.Is(err, jobtemplate.ErrInvalid), errors.Is(err, accounting.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, flow.ErrInvalidReport):
		status = http.StatusBadRequest
	case errors.Is(err, enrollment.ErrInvalidToken), errors.Is(err, enrollment.ErrUnauthorized), errors.Is(err, identity.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, identity.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, buildmanager.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, clusterops.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, enrollment.ErrNotFound), errors.Is(err, discovery.ErrNotFound), errors.Is(err, buildmanager.ErrNotFound), errors.Is(err, identity.ErrNotFound), errors.Is(err, backup.ErrNotFound), errors.Is(err, jobtemplate.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, telemetry.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, topology.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, alerting.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, notification.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, scheduler.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, clusterops.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, enrollment.ErrConflict), errors.Is(err, enrollment.ErrInvalidState), errors.Is(err, discovery.ErrConflict), errors.Is(err, buildmanager.ErrConflict), errors.Is(err, identity.ErrConflict), errors.Is(err, alerting.ErrInvalidState), errors.Is(err, notification.ErrInvalidState), errors.Is(err, jobtemplate.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, scheduler.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, scheduler.ErrQuotaExceeded):
		status = http.StatusConflict
	case errors.Is(err, enrollment.ErrUnavailable), errors.Is(err, discovery.ErrDisabled), errors.Is(err, buildmanager.ErrUnavailable), errors.Is(err, terminal.ErrUnavailable):
		status = http.StatusServiceUnavailable
	}
	message := http.StatusText(status)
	if status < 500 {
		message = err.Error()
	}
	var unsatisfiable *scheduler.UnsatisfiableError
	if errors.As(err, &unsatisfiable) {
		writeJSON(writer, status, map[string]any{"error": message, "reason": unsatisfiable.Reason})
		return
	}
	writeJSON(writer, status, map[string]string{"error": message})
}

func formatPolicyDuration(seconds int64) string {
	if seconds > 0 && seconds%(24*60*60) == 0 {
		return strconv.FormatInt(seconds/(24*60*60), 10) + "d"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func effectiveJobTimeoutSeconds(requested, maximum int64, administrator bool) (int64, error) {
	if requested < 0 || requested > domain.MaxSupportedJobTimeoutSeconds || maximum < 0 || maximum > domain.MaxSupportedJobTimeoutSeconds {
		return 0, fmt.Errorf("%w: timeoutSeconds is outside the supported duration range", scheduler.ErrInvalidJob)
	}
	if administrator {
		return requested, nil
	}
	if maximum == 0 {
		maximum = domain.DefaultMaxJobTimeoutSeconds
	}
	if requested == 0 {
		return maximum, nil
	}
	if requested > maximum {
		return 0, fmt.Errorf("%w: requested timeout %s exceeds the user group's maximum %s", scheduler.ErrInvalidJob,
			formatPolicyDuration(requested), formatPolicyDuration(maximum))
	}
	return requested, nil
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

func randomIdentifier() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
