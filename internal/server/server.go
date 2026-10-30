// Package server implements the XIRANG management and enrollment HTTP API.
package server

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/backup"
	"github.com/xirang-grid/xirang/internal/branding"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/scheduler"
	"github.com/xirang-grid/xirang/internal/terminal"
	"github.com/xirang-grid/xirang/pkg/version"
)

type CommandQueue interface {
	Enqueue(string, *xirangv1.ControlMessage) error
}

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
	ListForOwner(context.Context, string, int) ([]domain.Job, error)
	ListActiveForOwner(context.Context, string) ([]domain.Job, error)
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
	HostHealth         *hosthealth.Service
	Branding           *branding.Service
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
	if config.HostHealth != nil {
		registerHostHealthRoutes(mux, config.HostHealth, config.Enrollment, config.Commands, config.AdminToken)
	}
	if config.Branding != nil {
		registerBrandingRoutes(mux, config.Branding, config.AdminToken)
	}
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
		"artifact-runtime-contract-v1", "directed-reciprocal-network-capacity-v1", "budgeted-gang-search-reporting-v1",
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
		registerArtifactRoutes(mux, config.ArtifactRoot, config.Enrollment, config.Builds, config.AdminToken)
	}
	if config.WebRoot != "" {
		registerWebRoutes(mux, config.WebRoot)
	}

	boundedStreams := streamLimitMiddleware(256, 8, 30*time.Minute, mux)
	// The password endpoints sit behind a second, much tighter bucket: five
	// attempts then one every five seconds, which is unremarkable for a person
	// signing in and ruinous for a guessing loop.
	credentialLimited := throttleMiddleware(0.2, 5, "too many credential attempts", credentialThrottleRoute,
		loggingMiddleware(logger, accessMiddleware(config.Access, config.AdminToken, boundedStreams)))
	handler := requestIDMiddleware(securityHeadersMiddleware(rateLimitMiddleware(20, 40, credentialLimited)))
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

func (s *Server) ListenAndServe() error { return s.httpServer.ListenAndServe() }

func (s *Server) ListenAndServeTLS(certificateFile, keyFile string) error {
	return s.httpServer.ListenAndServeTLS(certificateFile, keyFile)
}

func (s *Server) Shutdown(ctx context.Context) error { return s.httpServer.Shutdown(ctx) }

func (s *Server) HTTPHandler() http.Handler { return s.httpServer.Handler }

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
