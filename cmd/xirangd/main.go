package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/accounting"
	"github.com/xirang-grid/xirang/internal/alerting"
	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/backup"
	"github.com/xirang-grid/xirang/internal/branding"
	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/clusterops"
	"github.com/xirang-grid/xirang/internal/controlstream"
	"github.com/xirang-grid/xirang/internal/discovery"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/flow"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"github.com/xirang-grid/xirang/internal/networkhealth"
	"github.com/xirang-grid/xirang/internal/notification"
	"github.com/xirang-grid/xirang/internal/pki"
	"github.com/xirang-grid/xirang/internal/scheduler"
	"github.com/xirang-grid/xirang/internal/server"
	alertbolt "github.com/xirang-grid/xirang/internal/store/alertbolt"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	externalstore "github.com/xirang-grid/xirang/internal/store/external"
	flowbolt "github.com/xirang-grid/xirang/internal/store/flowbolt"
	telemetrybolt "github.com/xirang-grid/xirang/internal/store/telemetrybolt"
	topologybolt "github.com/xirang-grid/xirang/internal/store/topologybolt"
	"github.com/xirang-grid/xirang/internal/telemetry"
	"github.com/xirang-grid/xirang/internal/terminal"
	"github.com/xirang-grid/xirang/internal/topology"
	"github.com/xirang-grid/xirang/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// agentServerOptions are the transport limits applied to the Agent control
// plane. They are separated from the listener so an interoperability test can
// build a server that matches production exactly.
func agentServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.MaxRecvMsgSize(1 << 20), grpc.MaxSendMsgSize(1 << 20),
		// An Agent holds one bidirectional control stream. The cap sits well
		// above that and bounds what a single misbehaving connection can make
		// the controller allocate.
		grpc.MaxConcurrentStreams(16),
		// Agents configure no keepalive of their own, so this rejects only a
		// client pinging faster than any legitimate one would, and refuses
		// pings from a connection carrying no stream at all.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: false,
		}),
		// Reclaim streams whose peer vanished without closing the connection,
		// which TCP alone can leave established for a long time.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    60 * time.Second,
			Timeout: 20 * time.Second,
		}),
	}
}

func main() {
	for {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		if !runController() {
			return
		}
	}
}

func runController() bool {
	if len(os.Args) > 1 && os.Args[1] == "__xirang-build-exec" {
		if err := buildmanager.RunExecutorLauncher(os.Args[2:]); err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(126)
		}
		return false
	}
	address := flag.String("listen", "0.0.0.0:8899", "HTTPS management and Web console listen address")
	agentAddress := flag.String("agent-listen", "127.0.0.1:8444", "mTLS Agent gRPC listen address")
	agentHeartbeatInterval := flag.Duration("agent-heartbeat-interval", 10*time.Second, "negotiated Agent heartbeat interval")
	unreachableAfter := flag.Duration("node-unreachable-after", 30*time.Second, "heartbeat age before marking a node unreachable")
	offlineAfter := flag.Duration("node-offline-after", 2*time.Minute, "heartbeat age before marking a node offline")
	reconcileInterval := flag.Duration("node-reconcile-interval", 5*time.Second, "node heartbeat reconciliation interval")
	certificateRotateBefore := flag.Duration("node-certificate-rotate-before", 7*24*time.Hour, "automatically rotate node certificates this long before expiry")
	dataDirectory := flag.String("data-dir", ".data/xirang", "control-plane state directory")
	webRoot := flag.String("web-root", "/usr/local/share/xirang/web", "compiled Web console directory (empty disables Web serving)")
	defaultSharedSpace := flag.Bool("default-shared-space", true, "automatically create and protect the built-in XirangShare space")
	jobArtifactRoot := flag.String("job-artifact-root", "/srv/xirang/XirangShare/.xirang/artifacts", "versioned system Job artifact directory on shared storage")
	terminalSSHKey := flag.String("terminal-ssh-key", "", "SSH key for administrator terminals and shared mounts (defaults inside data-dir)")
	terminalKnownHosts := flag.String("terminal-known-hosts", "", "SSH known-hosts file (defaults inside data-dir)")
	controllerRole := flag.String("controller-role", "", "preferred controller role: primary or backup; requires PostgreSQL")
	controllerNodeID := flag.String("controller-node-id", "", "enrolled node ID assigned to this controller")
	failbackDelay := flag.Duration("controller-failback-delay", 15*time.Second, "continuous primary availability before automatic handback")
	standby := flag.Bool("standby", false, "wait for the exclusive state lock and take over after the active controller exits")
	backupDirectory := flag.String("backup-dir", "", "online consistent backup directory (defaults inside data-dir)")
	externalStateDSN := flag.String("external-state-dsn", "", "optional PostgreSQL DSN for write-through control state and leader election")
	externalStateNamespace := flag.String("external-state-namespace", "default", "independent cluster namespace in PostgreSQL")
	telemetrySamplesPerNode := flag.Int("telemetry-samples-per-node", 720, "maximum resource observations retained per node")
	topologySnapshots := flag.Int("topology-snapshots", 120, "maximum versioned topology snapshots retained")
	flowSamplesPerNode := flag.Int("flow-samples-per-node", 120, "maximum eBPF flow observations retained per node")
	alertHistory := flag.Int("alert-history", 1000, "target retained alert occurrences; active alerts are never pruned")
	alertDeliveryHistory := flag.Int("alert-delivery-history", 2000, "target retained webhook delivery records; pending deliveries are never pruned")
	alertTriggerAfter := flag.Duration("alert-trigger-after", 30*time.Second, "interface issue duration before firing an alert")
	alertResolveAfter := flag.Duration("alert-resolve-after", 30*time.Second, "healthy duration before resolving an alert")
	alertEvaluationInterval := flag.Duration("alert-evaluation-interval", 5*time.Second, "interface alert evaluation interval")
	alertWebhookURL := flag.String("alert-webhook-url", "", "HTTPS endpoint for signed alert transition notifications")
	alertWebhookSecretFile := flag.String("alert-webhook-secret-file", "", "mode 0600 file containing at least 32 bytes for webhook HMAC signing")
	alertWebhookTimeout := flag.Duration("alert-webhook-timeout", 5*time.Second, "timeout for one alert webhook request")
	alertWebhookMaxAttempts := flag.Int("alert-webhook-max-attempts", 8, "attempts before a webhook delivery requires manual retry")
	adminTokenFile := flag.String("admin-token-file", "", "bootstrap administrator token file (defaults inside data-dir)")
	var tlsHosts stringListFlag
	flag.Var(&tlsHosts, "tls-host", "DNS name or IP for the server certificate; repeat as needed")
	var discoveryCIDRs stringListFlag
	flag.Var(&discoveryCIDRs, "discovery-allow-cidr", "CIDR inside which explicit administrator scans are allowed; repeat as needed")
	discoveryMaxHosts := flag.Int("discovery-max-hosts", 256, "maximum unique addresses in one discovery scan")
	discoveryAgentPort := flag.Int("discovery-agent-port", 8844, "TLS bootstrap port exposed by unenrolled Agents")
	discoveryTimeout := flag.Duration("discovery-connect-timeout", 2*time.Second, "per-address Agent discovery timeout")
	discoveryControllerURL := flag.String("discovery-controller-url", "", "public HTTPS controller URL embedded in approved candidate bootstrap instructions")
	buildEnabled := flag.Bool("build-enabled", false, "enable the local architecture native build worker")
	remoteBuildEnabled := flag.Bool("remote-build-enabled", true, "accept isolated native build workers reported by enrolled Agents")
	buildSourceRoot := flag.String("build-source-root", "/srv/xirang/XirangShare/.xirang/builds/sources", "allowlisted root containing checksum-pinned build sources")
	buildArtifactRoot := flag.String("build-artifact-root", "/srv/xirang/XirangShare/.xirang/builds/artifacts", "signed native artifact and SBOM output root")
	buildWorkspaceRoot := flag.String("build-workspace-root", "/srv/xirang/XirangShare/.xirang/builds/work", "temporary native build workspace root")
	buildCgroupRoot := flag.String("build-cgroup-root", "", "delegated cgroup v2 root for native builds")
	buildUser := flag.String("build-user", "xirang-build", "dedicated non-root native build identity")
	buildArtifactBaseURL := flag.String("build-artifact-base-url", "", "optional HTTPS base URL corresponding to build-artifact-root")
	oidcIssuer := flag.String("oidc-issuer", "", "optional exact HTTPS OIDC issuer")
	oidcAudience := flag.String("oidc-audience", "", "OIDC audience/client ID")
	oidcJWKSURL := flag.String("oidc-jwks-url", "", "HTTPS OIDC JSON Web Key Set URL")
	oidcGroupsClaim := flag.String("oidc-groups-claim", "groups", "OIDC claim containing role-mapped groups")
	var oidcGroupRoles stringListFlag
	flag.Var(&oidcGroupRoles, "oidc-group-role", "OIDC group to built-in role mapping as group=viewer|builder|operator|admin; repeat as needed")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		info := version.Current()
		_, _ = os.Stdout.WriteString(info.Name + " " + info.Version + "\n")
		return false
	}
	if *agentHeartbeatInterval < time.Second || *unreachableAfter <= 2**agentHeartbeatInterval ||
		*offlineAfter <= *unreachableAfter || *reconcileInterval <= 0 || *certificateRotateBefore <= 0 {
		_, _ = os.Stderr.WriteString("heartbeat must be at least 1s, unreachable-after must exceed two heartbeats, offline-after must exceed unreachable-after, and certificate rotation window must be positive\n")
		os.Exit(2)
	}
	if *telemetrySamplesPerNode < 1 || *telemetrySamplesPerNode > 100_000 {
		_, _ = os.Stderr.WriteString("telemetry-samples-per-node must be between 1 and 100000\n")
		os.Exit(2)
	}
	if *topologySnapshots < 1 || *topologySnapshots > 10_000 {
		_, _ = os.Stderr.WriteString("topology-snapshots must be between 1 and 10000\n")
		os.Exit(2)
	}
	if *flowSamplesPerNode < 1 || *flowSamplesPerNode > 10_000 {
		_, _ = os.Stderr.WriteString("flow-samples-per-node must be between 1 and 10000\n")
		os.Exit(2)
	}
	if *alertHistory < 1 || *alertHistory > 100_000 || *alertDeliveryHistory < 1 || *alertDeliveryHistory > 100_000 ||
		*alertTriggerAfter <= 0 || *alertResolveAfter <= 0 || *alertEvaluationInterval <= 0 || *alertWebhookTimeout <= 0 ||
		*alertWebhookMaxAttempts < 1 || *alertWebhookMaxAttempts > 100 {
		_, _ = os.Stderr.WriteString("alert histories must be between 1 and 100000, durations must be positive, and webhook attempts must be between 1 and 100\n")
		os.Exit(2)
	}
	if (*alertWebhookURL == "") != (*alertWebhookSecretFile == "") {
		_, _ = os.Stderr.WriteString("alert-webhook-url and alert-webhook-secret-file must be configured together\n")
		os.Exit(2)
	}

	if (*controllerRole != "" && (*externalStateDSN == "" || *controllerNodeID == "" || (*controllerRole != "primary" && *controllerRole != "backup"))) || (*controllerRole == "" && *controllerNodeID != "") || *failbackDelay < time.Second {
		_, _ = os.Stderr.WriteString("controller pair requires PostgreSQL, a node ID, primary/backup role and a positive failback delay\n")
		os.Exit(2)
	}
	lifecycle, stopLifecycle := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopLifecycle()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	resolvedData, resolvedBackups, err := resolveStateDirectories(*dataDirectory, *backupDirectory)
	if err != nil {
		logger.Error("resolve state directories", "error", err)
		os.Exit(2)
	}
	*dataDirectory, *backupDirectory = resolvedData, resolvedBackups
	if *adminTokenFile == "" {
		*adminTokenFile = filepath.Join(*dataDirectory, "admin.token")
	}
	if *terminalSSHKey == "" {
		*terminalSSHKey = filepath.Join(*dataDirectory, "ssh", "id_ed25519")
	}
	if *terminalKnownHosts == "" {
		*terminalKnownHosts = filepath.Join(*dataDirectory, "ssh", "known_hosts")
	}
	adminToken, created, err := identity.LoadOrCreateAdminToken(*adminTokenFile)
	if err != nil {
		logger.Error("initialize administrator token", "error", err)
		os.Exit(1)
	}
	if created {
		logger.Warn("created bootstrap administrator token", "path", *adminTokenFile)
	}
	authority, err := pki.OpenOrCreate(filepath.Join(*dataDirectory, "pki"), tlsHosts)
	if err != nil {
		logger.Error("initialize control-plane PKI", "error", err)
		os.Exit(1)
	}
	tlsConfig, err := authority.TLSConfig()
	if err != nil {
		logger.Error("initialize TLS configuration", "error", err)
		os.Exit(1)
	}
	trust := sha256.New()
	caBytes, err := os.ReadFile(authority.CAPath())
	if err != nil {
		logger.Error("read controller CA", "error", err)
		os.Exit(1)
	}
	_, _ = trust.Write(caBytes)
	_, _ = trust.Write([]byte("\x00" + adminToken))
	trustDigest := hex.EncodeToString(trust.Sum(nil))

	var state controlStore
	if *externalStateDSN != "" {
		state, err = externalstore.Open(lifecycle, externalstore.Config{
			DSN: *externalStateDSN, Namespace: *externalStateNamespace,
			CachePath: filepath.Join(*dataDirectory, "external-cache", "xirang.db"), Standby: *standby,
			ControllerRole: *controllerRole, ControllerNodeID: *controllerNodeID, TrustDigest: trustDigest, FailbackDelay: *failbackDelay,
		}, func() {
			logger.Info("standby controller waiting for PostgreSQL leadership", "namespace", *externalStateNamespace)
		})
	} else {
		state, err = openControlStore(filepath.Join(*dataDirectory, "xirang.db"), *standby, logger)
	}
	if err != nil {
		if lifecycle.Err() != nil {
			return false
		}
		logger.Error("open control-plane state", "error", err)
		if *controllerRole != "" && !errors.Is(err, externalstore.ErrControllerConfiguration) {
			select {
			case <-lifecycle.Done():
				return false
			case <-time.After(3 * time.Second):
				return true
			}
		}
		os.Exit(1)
	}
	defer state.Close()
	telemetryStore, err := telemetrybolt.Open(filepath.Join(*dataDirectory, "telemetry", "telemetry.db"), *telemetrySamplesPerNode)
	if err != nil {
		logger.Error("open telemetry state", "error", err)
		os.Exit(1)
	}
	defer telemetryStore.Close()
	telemetryService := telemetry.NewService(telemetryStore)
	topologyStore, err := topologybolt.Open(filepath.Join(*dataDirectory, "topology", "topology.db"), *topologySnapshots)
	if err != nil {
		logger.Error("open topology state", "error", err)
		os.Exit(1)
	}
	defer topologyStore.Close()
	topologyService := topology.NewService(topologyStore)
	flowStore, err := flowbolt.Open(filepath.Join(*dataDirectory, "flows", "flows.db"), *flowSamplesPerNode)
	if err != nil {
		logger.Error("open flow state", "error", err)
		os.Exit(1)
	}
	defer flowStore.Close()
	flowService := flow.NewService(flowStore)
	alertStore, err := alertbolt.Open(filepath.Join(*dataDirectory, "alerts", "alerts.db"), *alertHistory, *alertDeliveryHistory)
	if err != nil {
		logger.Error("open alert state", "error", err)
		os.Exit(1)
	}
	defer alertStore.Close()
	backupService, err := backup.New(*backupDirectory, map[string]backup.SnapshotSource{
		"xirang.db": state, "telemetry.db": telemetryStore, "topology.db": topologyStore,
		"flows.db": flowStore, "alerts.db": alertStore,
	})
	if err != nil {
		logger.Error("initialize consistent backups", "error", err)
		os.Exit(1)
	}
	var webhookSecret []byte
	if *alertWebhookSecretFile != "" {
		webhookSecret, err = notification.LoadSecretFile(*alertWebhookSecretFile)
		if err != nil {
			logger.Error("load alert webhook secret", "error", err)
			os.Exit(1)
		}
	}
	alertPolicyService := alertpolicy.New(state, alertpolicy.Defaults(*alertTriggerAfter, *alertResolveAfter))
	notificationService, err := notification.NewService(alertStore, nil, notification.Config{
		URL: *alertWebhookURL, Secret: webhookSecret, Timeout: *alertWebhookTimeout, MaxAttempts: *alertWebhookMaxAttempts,
		Policy: alertPolicyService.Get,
	})
	if err != nil {
		logger.Error("configure alert webhook", "error", err)
		os.Exit(1)
	}
	alertService := alerting.NewService(alertStore, alerting.Config{
		TriggerAfter: *alertTriggerAfter, ResolveAfter: *alertResolveAfter, RecordTransitions: notificationService.Enabled(),
		Policy: alertPolicyService.Get,
	})
	enrollmentService := enrollment.NewService(state, authority)
	if external, ok := state.(*externalstore.Store); ok && *controllerRole != "" {
		members, err := external.ControllerMembers(lifecycle)
		if err != nil {
			logger.Error("read controller pair", "error", err)
			os.Exit(1)
		}
		enrollmentService.SetControllerMembers(members, *controllerNodeID)
		enrollmentService.SetControllerResolver(external.ControllerMembers)
	}
	commandHub := controlstream.NewHub(128)
	mountBroker := mountcontrol.New(commandHub, state.AppendAuditEvent)
	localHostname, _ := os.Hostname()
	liveNetwork := networkhealth.NewLiveSource(topologyService, telemetryService)
	networkPaths := &networkhealth.Resolver{Snapshots: liveNetwork, Nodes: enrollmentService, LocalID: *controllerNodeID, Hostname: localHostname, Telemetry: telemetryService}
	sharedSpaceService := clusterops.New(state, enrollmentService, clusterops.AgentRunner{
		Mounts: mountBroker, Files: clusterops.SSHRunner{KeyPath: *terminalSSHKey, KnownHostsPath: *terminalKnownHosts, Paths: networkPaths},
	})
	if *defaultSharedSpace {
		sharedSpaceService.EnableDefault("/srv/xirang/XirangShare", "/srv/xirang/XirangShare")
	}
	sharedSpaceService.EnableNetworkPaths(networkPaths)
	joinTokens, joinTokenCreated, err := enrollment.OpenJoinTokenManager(
		context.Background(), filepath.Join(*dataDirectory, "join.token"), enrollmentService,
	)
	if err != nil {
		logger.Error("initialize cluster join token", "error", err)
		os.Exit(1)
	}
	if joinTokenCreated {
		logger.Warn("created cluster join token", "path", filepath.Join(*dataDirectory, "join.token"))
	}
	var oidcVerifier identity.OIDCVerifier
	if *oidcIssuer != "" || *oidcAudience != "" || *oidcJWKSURL != "" {
		if *oidcIssuer == "" || *oidcAudience == "" || *oidcJWKSURL == "" {
			logger.Error("OIDC issuer, audience and JWKS URL must be configured together")
			os.Exit(2)
		}
		oidcVerifier, err = identity.NewOIDCVerifier(identity.OIDCConfig{
			Issuer: *oidcIssuer, Audience: *oidcAudience, JWKSURL: *oidcJWKSURL, GroupsClaim: *oidcGroupsClaim,
		})
		if err != nil {
			logger.Error("configure OIDC verifier", "error", err)
			os.Exit(2)
		}
	}
	groupRoles, err := parseGroupRoles(oidcGroupRoles)
	if err != nil {
		logger.Error("configure OIDC group roles", "error", err)
		os.Exit(2)
	}
	accessService := identity.NewAccessService(state, oidcVerifier, groupRoles)
	sharedSpaceService.EnableStorageQuotas(func(ctx context.Context, principal domain.Principal) (int64, error) {
		policy, err := accessService.ResolveUserGroupPolicy(ctx, principal.GroupIDs)
		return policy.StorageQuotaBytes, err
	})
	controllerCAPEM, err := os.ReadFile(authority.CAPath())
	if err != nil {
		logger.Error("read controller CA for Agent bootstrap", "error", err)
		os.Exit(1)
	}
	discoveryService, err := discovery.New(state, enrollmentService, discovery.Config{
		AllowedCIDRs: discoveryCIDRs, AgentPort: *discoveryAgentPort, MaxHosts: *discoveryMaxHosts,
		ConnectTimeout: *discoveryTimeout, ControllerURL: *discoveryControllerURL,
		ControllerCAPEM: string(controllerCAPEM),
	})
	if err != nil {
		logger.Error("configure restricted network discovery", "error", err)
		os.Exit(2)
	}
	remoteBuildPool := buildmanager.NewRemotePool(commandHub)
	var buildService server.BuildManager = buildmanager.NewUnavailableManager(runtime.GOARCH)
	registries := make(buildmanager.CompositeRegistry, 0, 2)
	if *remoteBuildEnabled {
		registries = append(registries, remoteBuildPool)
	}
	if *buildEnabled {
		if *buildWorkspaceRoot == "" {
			*buildWorkspaceRoot = "/srv/xirang/build-work"
		}
		runner, runnerErr := buildmanager.NewNativeRunner(buildmanager.NativeRunnerConfig{
			User: *buildUser, Group: *buildUser, CgroupRoot: *buildCgroupRoot,
		})
		if runnerErr != nil {
			logger.Error("initialize isolated native build worker", "error", runnerErr)
			os.Exit(1)
		}
		registries = append(registries, buildmanager.StaticRegistry{Runners: []buildmanager.Runner{runner}})
	}
	if len(registries) > 0 {
		service, serviceErr := buildmanager.New(state, buildmanager.Config{
			SourceRoot: *buildSourceRoot, WorkspaceRoot: *buildWorkspaceRoot,
			ArtifactRoot: *buildArtifactRoot, ArtifactBaseURL: *buildArtifactBaseURL,
			SigningKeyPath: filepath.Join(*dataDirectory, "builds", "signing.key"), Registry: registries,
			SharedWorkspace:      *remoteBuildEnabled,
			SharedWorkspaceOwner: "xirang-task",
			ResolveSharedPath:    sharedSpaceService.ResolveSharedPath,
			AuthorizeSharedPath:  sharedSpaceService.AuthorizeSharedPath,
		})
		if serviceErr != nil {
			logger.Error("initialize native build manager", "error", serviceErr)
			os.Exit(1)
		}
		buildService = service
	}
	nodeEvents := server.NewNodeEventBroker()
	sharedSpaceReconcile := make(chan struct{}, 1)
	enrollmentService.SetNodeObserver(func(node domain.Node) {
		nodeEvents.Publish(node)
		select {
		case sharedSpaceReconcile <- struct{}{}:
		default:
		}
	})
	terminalBroker := terminal.NewBroker(commandHub)
	terminalService, err := terminal.New(enrollmentService, terminalBroker, state)
	if err != nil {
		logger.Error("initialize administrator Agent PTY", "error", err)
		os.Exit(1)
	}
	persistedNodes, err := enrollmentService.ListNodes(context.Background())
	if err != nil {
		logger.Error("restore pending node mode commands", "error", err)
		os.Exit(1)
	}
	if err := commandHub.RestorePersistentCommands(persistedNodes); err != nil {
		logger.Error("restore pending node commands", "error", err)
		os.Exit(1)
	}
	jobCoordinator := scheduler.New(state, state, commandHub)
	jobCoordinator.SetAlertSink(alertService)
	accountingService := accounting.New(state)
	jobTemplateService := jobtemplate.New(state)
	if err := jobtemplate.EnsureBuiltins(
		context.Background(), jobTemplateService,
		filepath.Join(*dataDirectory, "builtin-job-templates-v1"), *jobArtifactRoot,
	); err != nil {
		logger.Error("install built-in Job templates", "error", err)
	}
	jobCoordinator.SetTopologySource(liveNetwork)
	if err := jobCoordinator.RestoreAssignments(context.Background()); err != nil {
		logger.Error("restore pending job assignments", "error", err)
		os.Exit(1)
	}
	hostHealthService := hosthealth.New(state)
	agentControl := controlstream.New(enrollmentService, *agentHeartbeatInterval, commandHub)
	if err := agentControl.ConfigureTelemetryInterval(filepath.Join(*dataDirectory, "telemetry-interval")); err != nil {
		logger.Error("load Agent telemetry interval", "error", err)
		os.Exit(1)
	}
	agentControl.SetTelemetryReporter(telemetryService)
	agentControl.SetHostHealthReporter(hostHealthService)
	agentControl.SetTopologyReporter(topologyService)
	agentControl.SetNetworkProbeTargetProvider(topology.NewProbeTargetProvider(enrollmentService, telemetryService))
	agentControl.SetFlowReporter(flowService)
	agentControl.SetAssignmentReporter(jobCoordinator)
	agentControl.SetTerminalReporter(terminalBroker)
	agentControl.SetMountReporter(mountBroker)
	if *remoteBuildEnabled {
		agentControl.SetNativeBuildReporter(remoteBuildPool)
	}
	application := server.New(server.Config{
		Address: *address, Logger: logger, AdminToken: adminToken,
		Branding:    branding.New(state),
		AlertPolicy: alertPolicyService,
		Enrollment:  enrollmentService, JoinTokens: joinTokens, TLSConfig: tlsConfig, Commands: commandHub, NodeEvents: nodeEvents,
		Telemetry:          telemetryService,
		HostHealth:         hostHealthService,
		Topology:           topologyService,
		NetworkPaths:       networkPaths,
		Flows:              flowService,
		Alerts:             alertService,
		Notifications:      notificationService,
		Jobs:               jobCoordinator,
		JobTemplates:       jobTemplateService,
		Queues:             jobCoordinator,
		Accounting:         accountingService,
		Discovery:          discoveryService,
		Builds:             buildService,
		Access:             accessService,
		Backups:            backupService,
		SharedSpaces:       sharedSpaceService,
		Terminal:           terminalService,
		TelemetryIntervals: agentControl,
		ExternalState:      *externalStateDSN != "",
		ArtifactRoot:       *buildArtifactRoot,
		WebRoot:            *webRoot,
	})
	var leadershipChanges <-chan error
	if external, ok := state.(*externalstore.Store); ok {
		leadershipChanges = external.WatchLeadership(lifecycle)
		application.ConfigureLifecycle(lifecycle, external.CheckLeadership)
	} else {
		application.ConfigureLifecycle(lifecycle, nil)
	}

	agentListener, err := net.Listen("tcp", *agentAddress)
	if err != nil {
		logger.Error("listen for Agent control streams", "error", err)
		os.Exit(1)
	}
	agentTLS := tlsConfig.Clone()
	agentTLS.ClientAuth = tls.RequireAndVerifyClientCert
	// Certificates is deliberately left empty. crypto/tls consults
	// GetCertificate only when a client sends SNI or when Certificates is
	// empty, and Agents dial the controller by address, so a static entry here
	// would pin the listener to the certificate loaded at startup and defeat
	// renewal.
	agentServer := grpc.NewServer(append(agentServerOptions(),
		grpc.Creds(credentials.NewTLS(agentTLS)),
		grpc.StreamInterceptor(func(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if external, ok := state.(*externalstore.Store); ok {
				stream = &leadershipStream{ServerStream: stream, check: external.CheckLeadership}
			}
			err := handler(service, stream)
			if err != nil && !errors.Is(err, context.Canceled) {
				address := ""
				if connection, ok := peer.FromContext(stream.Context()); ok && connection.Addr != nil {
					address = connection.Addr.String()
				}
				logger.Warn("Agent control stream ended", "method", info.FullMethod, "peer", address, "error", err)
			}
			return err
		}),
	)...)
	xirangv1.RegisterAgentControlServer(agentServer, agentControl)

	httpErrors := make(chan error, 1)
	agentErrors := make(chan error, 1)
	go func() {
		// Empty paths: the certificate comes from the authority through
		// TLSConfig.GetCertificate, which reissues it before it expires.
		httpErrors <- application.ListenAndServeTLS("", "")
	}()
	go func() { agentErrors <- agentServer.Serve(agentListener) }()
	reconcileContext, stopReconciler := context.WithCancel(lifecycle)
	defer stopReconciler()
	var background sync.WaitGroup
	start := func(work func()) { background.Add(1); go func() { defer background.Done(); work() }() }
	start(func() {
		reconcileNodes(reconcileContext, enrollmentService, commandHub, *reconcileInterval, *unreachableAfter, *offlineAfter, *certificateRotateBefore, logger)
	})
	start(func() { reconcileSharedSpaces(reconcileContext, sharedSpaceService, sharedSpaceReconcile, logger) })
	start(func() {
		alerting.RunEvaluator(reconcileContext, topologyService, telemetryService, alertService, *alertEvaluationInterval, func(err error) { logger.Error("evaluate interface alerts", "error", err) })
	})
	start(func() {
		notificationService.Run(reconcileContext, func(err error) { logger.Error("deliver alert webhook", "error", err) })
	})
	start(func() {
		jobCoordinator.Run(reconcileContext, func(err error) { logger.Error("schedule jobs", "error", err) })
	})

	logger.Info("starting XIRANG control plane", "apiAddress", *address, "agentAddress", *agentAddress,
		"version", version.Version, "ca", authority.CAPath())

	var serveErr error
	restart := false
	select {
	case <-lifecycle.Done():
	case reason := <-leadershipChanges:
		logger.Info("relinquishing controller leadership", "reason", reason)
		restart = *controllerRole != ""
		if !restart {
			serveErr = reason
		}
	case err := <-httpErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case err := <-agentErrors:
		if !errors.Is(err, grpc.ErrServerStopped) {
			serveErr = err
		}
	}
	stopReconciler()
	stopLifecycle()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := application.Shutdown(shutdownContext); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP shutdown failed", "error", err)
		_ = application.Close()
	}
	gracefulStopGRPC(shutdownContext, agentServer)
	background.Wait()
	if serveErr != nil {
		logger.Error("control plane stopped", "error", serveErr)
		os.Exit(1)
	}
	return restart
}

type leadershipStream struct {
	grpc.ServerStream
	check func(context.Context) error
}

func (s *leadershipStream) verify() error {
	ctx, cancel := context.WithTimeout(s.Context(), 2*time.Second)
	defer cancel()
	return s.check(ctx)
}
func (s *leadershipStream) SendMsg(message any) error {
	if err := s.verify(); err != nil {
		return err
	}
	return s.ServerStream.SendMsg(message)
}
func (s *leadershipStream) RecvMsg(message any) error {
	if err := s.ServerStream.RecvMsg(message); err != nil {
		return err
	}
	return s.verify()
}

func reconcileSharedSpaces(ctx context.Context, service *clusterops.Service, requested <-chan struct{}, logger *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	run := func() {
		reconcileCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := service.Reconcile(reconcileCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("reconcile shared spaces", "error", err)
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		case <-requested:
			timer := time.NewTimer(350 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			run()
		}
	}
}

func reconcileNodes(ctx context.Context, service *enrollment.Service, commands *controlstream.Hub, interval, unreachableAfter, offlineAfter, certificateRotateBefore time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	reconcile := func() {
		if err := service.RefreshControllerMembers(ctx); err != nil {
			logger.Error("refresh controller pair", "error", err)
			return
		}
		changed, err := service.ReconcileStaleNodes(ctx, unreachableAfter, offlineAfter)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("reconcile stale nodes", "error", err)
			return
		}
		for _, node := range changed {
			logger.Warn("node heartbeat state changed", "node", node.ID, "status", node.Status, "lastSeenAt", node.LastSeenAt)
		}
		rotations, err := service.RequestExpiringCertificateRotations(ctx, certificateRotateBefore)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("request expiring node certificate rotations", "error", err)
			return
		}
		for _, node := range rotations {
			message := &xirangv1.ControlMessage{
				MessageId: node.PendingCertificateCommandID, SentAt: timestamppb.Now(),
				Payload: &xirangv1.ControlMessage_RotateCertificate{RotateCertificate: &xirangv1.RotateCertificate{
					RotationId: node.PendingCertificateRotationID,
				}},
			}
			if err := commands.Enqueue(node.ID, message); err != nil {
				_, _ = service.ConfirmCertificateRotation(ctx, node.ID, node.PendingCertificateCommandID, false, "automatic command enqueue failed")
				logger.Error("enqueue automatic certificate rotation", "node", node.ID, "error", err)
				continue
			}
			logger.Info("requested automatic node certificate rotation", "node", node.ID, "notAfter", node.CertificateNotAfter)
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func gracefulStopGRPC(ctx context.Context, server *grpc.Server) {
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		server.Stop()
	}
}

type controlStore interface {
	hosthealth.Repository
	branding.Repository
	alertpolicy.Repository
	enrollment.Repository
	scheduler.Repository
	discovery.Repository
	buildmanager.Repository
	identity.AccessRepository
	jobtemplate.Repository
	clusterops.Repository
	accounting.Repository
	backup.SnapshotSource
}

func openControlStore(path string, standby bool, logger *slog.Logger) (*boltstore.Store, error) {
	for {
		store, err := boltstore.Open(path)
		if err == nil {
			return store, nil
		}
		if !standby || !boltstore.IsDatabaseLocked(err) {
			return nil, err
		}
		logger.Info("standby controller waiting for active state lock", "path", path)
		time.Sleep(5 * time.Second)
	}
}

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }
func (values *stringListFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("repeated flag value cannot be empty")
	}
	*values = append(*values, strings.TrimSpace(value))
	return nil
}

func parseGroupRoles(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		group, role, ok := strings.Cut(value, "=")
		group, role = strings.TrimSpace(group), strings.TrimSpace(role)
		if !ok || group == "" || result[group] != "" {
			return nil, errors.New("OIDC group-role mappings must be unique group=role values")
		}
		switch role {
		case "viewer", "builder", "operator", "admin":
			result[group] = role
		default:
			return nil, errors.New("OIDC role must be viewer, builder, operator, or admin")
		}
	}
	return result, nil
}
