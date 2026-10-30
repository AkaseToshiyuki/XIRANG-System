package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"github.com/xirang-grid/xirang/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultAgentPort  = "8444"
	maxControlMessage = 1 << 20
	// controlDialTimeout bounds one attempt to reach the control plane so a
	// controller that accepts packets without answering cannot stall the retry
	// loop instead of feeding its backoff.
	controlDialTimeout = 10 * time.Second
	// controlDialCauseTimeout bounds the short request used only to recover why
	// a connection never became ready.
	controlDialCauseTimeout       = 2 * time.Second
	inventorySnapshotCapability   = "inventory-snapshot-v1"
	resourceObservationCapability = "resource-observation-v1"
	interfaceHealthCapability     = "interface-health-counters-v1"
	topologyObservationCapability = "topology-observation-v1"
	flowObservationCapability     = "ebpf-flow-observation-v1"
	cgroupExecutorCapability      = "cgroup-v2-executor-v1"
	assignmentLogCapability       = "assignment-log-snapshot-v1"
	deviceInventoryCapability     = "device-inventory-v1"
	deviceExecutorCapability      = "device-resource-executor-v1"
	cpusetExecutorCapability      = "cpuset-affinity-v1"
	telemetryIntervalCapability   = "telemetry-interval-v1"
	networkPathProbeCapability    = "network-path-probe-v1"
	gangAssignmentCapability      = "gang-assignment-v1"
	mpiLaunchCapability           = "mpi-launch-v1"
	agentPTYCapability            = "agent-pty-v1"
	managementPanelCapability     = "management-panel-relay-v1"
	defaultTopologyInterval       = 30 * time.Second
)

type ControlLoopConfig struct {
	Address            string
	StateDirectory     string
	MinBackoff         time.Duration
	MaxBackoff         time.Duration
	OnEvent            func(string)
	FlowMonitoring     FlowMonitoringConfig
	ExecutorIsolation  ExecutorIsolationConfig
	ArtifactTrustKeys  []ed25519.PublicKey
	NativeBuild        NativeBuildConfig
	TerminalUser       string
	SharedMountControl bool
	mountExecution     mountExecution
	terminalIdentity   *terminalIdentity
	healthWorker       *healthWorker
}

func DefaultControlAddress(state State) (string, error) {
	parsed, err := url.Parse(state.ServerURL)
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("the Agent state has an invalid control-plane URL")
	}
	return net.JoinHostPort(parsed.Hostname(), defaultAgentPort), nil
}

func RunControlLoop(ctx context.Context, state State, config ControlLoopConfig) error {
	if config.SharedMountControl {
		run, err := configuredMountExecution()
		if err != nil {
			return err
		}
		config.mountExecution = run
	}
	if config.Address == "" {
		address, err := DefaultControlAddress(state)
		if err != nil {
			return err
		}
		config.Address = address
	}
	if config.StateDirectory == "" {
		return errors.New("an Agent state directory is required")
	}
	if _, _, err := net.SplitHostPort(config.Address); err != nil {
		return fmt.Errorf("invalid control address: %w", err)
	}
	if config.MinBackoff <= 0 {
		config.MinBackoff = time.Second
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = 30 * time.Second
	}
	if config.MaxBackoff < config.MinBackoff {
		config.MaxBackoff = config.MinBackoff
	}
	if strings.TrimSpace(config.TerminalUser) != "" {
		identity, err := resolveTerminalIdentity(config.TerminalUser)
		if err != nil {
			return fmt.Errorf("configure administrator terminal: %w", err)
		}
		config.terminalIdentity = &identity
	}
	var executor *NativeExecutor
	var err error
	if config.ExecutorIsolation.TaskUser != "" || config.ExecutorIsolation.CgroupRoot != "" {
		executor, err = OpenIsolatedNativeExecutor(config.StateDirectory, config.ExecutorIsolation)
	} else {
		executor, err = OpenNativeExecutor(config.StateDirectory)
	}
	if err != nil {
		return fmt.Errorf("open native executor: %w", err)
	}
	executor.SetArtifactTrustKeys(config.ArtifactTrustKeys)
	if config.NativeBuild.Enabled && config.NativeBuild.CgroupRoot == "auto" {
		if executor.cgroups == nil {
			return errors.New("remote native build auto cgroup requires the isolated task executor")
		}
		config.NativeBuild.CgroupRoot = executor.cgroups.root
	}
	buildManager, err := OpenNativeBuildManager(config.NativeBuild)
	if err != nil {
		return fmt.Errorf("open remote native build worker: %w", err)
	}
	managementPanel := NewManagementPanel(config.StateDirectory, state)
	defer managementPanel.Close()
	commandState, err := loadCommandState(config.StateDirectory)
	if err != nil {
		return fmt.Errorf("load Agent command state: %w", err)
	}
	if commandState.ManagementPanelEnabled {
		if accepted, reason := managementPanel.SetEnabled(true); !accepted {
			emitControlEvent(config, fmt.Sprintf("restore management panel failed: %s", reason))
		}
	}
	var flowCollector *EBPFFlowCollector
	if config.FlowMonitoring.Enabled {
		flowCollector, err = NewEBPFFlowCollector(config.FlowMonitoring)
		if err != nil {
			emitControlEvent(config, fmt.Sprintf("eBPF flow monitoring unavailable; continuing without it: %v", err))
		} else {
			defer flowCollector.Close()
			emitControlEvent(config, fmt.Sprintf("eBPF flow monitoring active cgroup=%s", flowCollector.config.CgroupPath))
		}
	}
	outbox, err := openHealthOutbox(config.StateDirectory, state.NodeID)
	if err != nil {
		return fmt.Errorf("open host event outbox: %w", err)
	}
	workerContext, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	config.healthWorker = &healthWorker{collector: configuredHostHealth(), outbox: outbox, updates: make(chan struct{}, 1)}
	go func() {
		defer close(workerDone)
		config.healthWorker.run(workerContext, time.Minute, func(message string) { emitControlEvent(config, message) })
	}()
	defer func() { stopWorker(); <-workerDone; outbox.db.Close() }()
	backoff := config.MinBackoff
	for {
		currentState, err := LoadState(config.StateDirectory)
		if err != nil {
			return fmt.Errorf("reload Agent identity: %w", err)
		}
		if currentState.NodeID != state.NodeID {
			return errors.New("agent node identity changed while the event outbox was open")
		}
		connected, err := runControlSession(ctx, currentState, config, executor, flowCollector, buildManager, managementPanel)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = config.MinBackoff
		}
		emitControlEvent(config, fmt.Sprintf("control stream disconnected: %v; retrying in %s", err, backoff))
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > config.MaxBackoff {
			backoff = config.MaxBackoff
		}
	}
}

// dialControlPlane returns a connection that is already usable. grpc.NewClient
// connects lazily, so the readiness that the retired WithBlock option used to
// guarantee is established here: the caller's retry loop distinguishes a
// controller that is unreachable from a session that ended, and it can only do
// that if reaching the controller is what fails.
func dialControlPlane(ctx context.Context, address string, timeout time.Duration, options ...grpc.DialOption) (*grpc.ClientConn, error) {
	connection, err := grpc.NewClient(address, options...)
	if err != nil {
		return nil, fmt.Errorf("prepare control stream client: %w", err)
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection.Connect()
	for {
		state := connection.GetState()
		if state == connectivity.Ready {
			return connection, nil
		}
		if state == connectivity.Shutdown {
			_ = connection.Close()
			return nil, errors.New("connect control stream: client is shut down")
		}
		if connection.WaitForStateChange(waitContext, state) {
			continue
		}
		cause := controlDialCause(ctx, connection, state, timeout)
		_ = connection.Close()
		return nil, fmt.Errorf("connect control stream: %w", cause)
	}
}

// controlDialCause recovers why a connection never became ready. The
// connectivity API reports only a state, which cannot tell a refused port from
// a rejected certificate, so a short request is issued: a fail-fast RPC against
// a failed connection carries the dial error an operator can act on.
func controlDialCause(ctx context.Context, connection *grpc.ClientConn, last connectivity.State, timeout time.Duration) error {
	unreachable := fmt.Errorf("control plane did not become ready within %s (last state %s)", timeout, last)
	// An Agent that is shutting down should not spend time explaining a dial it
	// no longer needs.
	if ctx.Err() != nil {
		return unreachable
	}
	causeContext, cancel := context.WithTimeout(ctx, controlDialCauseTimeout)
	defer cancel()
	stream, err := xirangv1.NewAgentControlClient(connection).Connect(causeContext)
	if err != nil {
		return err
	}
	_ = stream.CloseSend()
	return unreachable
}

func runControlSession(ctx context.Context, state State, config ControlLoopConfig, executor *NativeExecutor, flowCollector *EBPFFlowCollector, buildManager *NativeBuildManager, managementPanel *ManagementPanel) (bool, error) {
	tlsConfig, err := authenticatedTLSConfig(config.StateDirectory, state)
	if err != nil {
		return false, err
	}
	artifactClient, err := authenticatedHTTPClient(config.StateDirectory, state)
	if err != nil {
		return false, fmt.Errorf("prepare authenticated artifact client: %w", err)
	}
	if err := executor.SetArtifactHTTPClient(state.ServerURL, artifactClient); err != nil {
		return false, err
	}
	tlsConfig.ServerName, _, err = net.SplitHostPort(config.Address)
	if err != nil {
		return false, fmt.Errorf("invalid control address: %w", err)
	}
	connection, err := dialControlPlane(ctx, config.Address, controlDialTimeout,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxControlMessage), grpc.MaxCallSendMsgSize(maxControlMessage)))
	if err != nil {
		return false, err
	}
	defer connection.Close()
	streamContext, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	stream, err := xirangv1.NewAgentControlClient(connection).Connect(streamContext)
	if err != nil {
		return false, err
	}
	healthCollector := configuredHostHealth()
	capabilities := []string{
		hosthealth.Capability,
		"heartbeat-v1", "node-mode-v1", "native-executor-v1", "certificate-rotation-v1",
		inventorySnapshotCapability, resourceObservationCapability, interfaceHealthCapability, topologyObservationCapability,
		assignmentLogCapability, deviceInventoryCapability, telemetryIntervalCapability, networkPathProbeCapability,
		gangAssignmentCapability, mpiLaunchCapability, managementPanelCapability,
	}
	if config.healthWorker != nil {
		capabilities = append(capabilities, hosthealth.OutboxCapability)
	}
	if config.mountExecution != nil {
		capabilities = append(capabilities, mountcontrol.Capability)
	}
	if healthCollector.identify {
		capabilities = append(capabilities, hosthealth.IdentifyCapability)
	}
	if config.terminalIdentity != nil {
		capabilities = append(capabilities, agentPTYCapability)
	}
	capabilities = append(capabilities, detectedMPILauncherCapabilities()...)
	if buildManager != nil {
		capabilities = append(capabilities, remoteNativeBuildCapability)
	}
	if flowCollector != nil {
		capabilities = append(capabilities, flowObservationCapability)
	}
	if executor.cgroups != nil {
		capabilities = append(capabilities, cgroupExecutorCapability)
	}
	if executor.SupportsCPUSet() {
		capabilities = append(capabilities, cpusetExecutorCapability)
	}
	if executor.SupportsDeviceEnforcement() {
		capabilities = append(capabilities, deviceExecutorCapability)
		for _, resource := range executor.DeviceResources() {
			capabilities = append(capabilities, deviceExecutorCapability+"/"+resource)
		}
	}
	if err := stream.Send(&xirangv1.AgentMessage{
		MessageId: agentMessageID(), SentAt: timestamppb.Now(),
		Payload: &xirangv1.AgentMessage_Hello{Hello: &xirangv1.AgentHello{
			NodeId: state.NodeID, AgentVersion: version.Version, ProtocolVersion: version.ProtocolVersion,
			Os: version.Current().OS, Architecture: version.Current().Architecture,
			Capabilities: capabilities,
		}},
	}); err != nil {
		return false, err
	}
	acceptedMessage, err := stream.Recv()
	if err != nil {
		return false, err
	}
	accepted := acceptedMessage.GetHelloAccepted()
	if accepted == nil || accepted.NodeId != state.NodeID {
		return false, errors.New("control plane returned an invalid hello acknowledgement")
	}
	if accepted.HeartbeatInterval == nil {
		return false, errors.New("control plane omitted the heartbeat interval")
	}
	if !containsCapability(accepted.NegotiatedCapabilities, "heartbeat-v1") {
		return false, errors.New("control plane did not negotiate heartbeat-v1")
	}
	if buildManager != nil {
		if !containsCapability(accepted.NegotiatedCapabilities, remoteNativeBuildCapability) {
			return false, errors.New("control plane did not negotiate remote-native-build-v1")
		}
		if err := stream.Send(&xirangv1.AgentMessage{
			MessageId: agentMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_NativeBuildWorker{NativeBuildWorker: buildManager.WorkerReport()},
		}); err != nil {
			return false, err
		}
	}
	interval := accepted.HeartbeatInterval.AsDuration()
	if interval < time.Second || interval > 5*time.Minute {
		return false, fmt.Errorf("control plane returned invalid heartbeat interval %s", interval)
	}
	telemetryInterval := interval
	if accepted.TelemetryInterval != nil {
		telemetryInterval = accepted.TelemetryInterval.AsDuration()
	}
	if telemetryInterval < 2*time.Second || telemetryInterval > time.Minute {
		return false, fmt.Errorf("control plane returned invalid telemetry interval %s", telemetryInterval)
	}
	emitControlEvent(config, fmt.Sprintf("control stream connected node=%s heartbeat=%s telemetry=%s", state.NodeID, interval, telemetryInterval))
	commands, err := loadCommandState(config.StateDirectory)
	if err != nil {
		return true, err
	}
	var inventory Inventory
	if containsCapability(accepted.NegotiatedCapabilities, inventorySnapshotCapability) {
		inventory, err = CollectInventory()
		if err != nil {
			return true, fmt.Errorf("collect inventory: %w", err)
		}
		if err := stream.Send(&xirangv1.AgentMessage{
			MessageId: agentMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Inventory{Inventory: inventorySnapshot(inventory)},
		}); err != nil {
			return true, err
		}
	}
	var sampler *ResourceSampler
	if containsCapability(accepted.NegotiatedCapabilities, resourceObservationCapability) {
		sampler, err = NewResourceSampler()
		if err != nil {
			return true, fmt.Errorf("initialize resource sampler: %w", err)
		}
	}
	var topologyDigest [32]byte
	var haveTopologyDigest bool
	var probeTargets []NetworkProbeTarget
	if containsCapability(accepted.NegotiatedCapabilities, networkPathProbeCapability) {
		probeTargets, err = validatedNetworkProbeTargets(state.NodeID, accepted.NetworkProbeTargets)
		if err != nil {
			return true, fmt.Errorf("control plane returned invalid network probe targets: %w", err)
		}
	}
	sendTopology := func(force bool, probes []PathProbeObservation) error {
		observation, err := CollectTopology(streamContext)
		if err != nil {
			emitControlEvent(config, fmt.Sprintf("topology collection failed: %v", err))
			return nil
		}
		observation.PathProbes = probes
		digest := topologyFingerprint(observation)
		if !force && haveTopologyDigest && digest == topologyDigest {
			return nil
		}
		if err := stream.Send(&xirangv1.AgentMessage{
			MessageId: agentMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Topology{Topology: topologySnapshot(observation)},
		}); err != nil {
			return err
		}
		topologyDigest, haveTopologyDigest = digest, true
		return nil
	}
	type networkProbeResult struct {
		generation uint64
		probes     []PathProbeObservation
	}
	probeResults := make(chan networkProbeResult, 1)
	probeRunning := false
	var probeGeneration uint64 = 1
	startNetworkProbes := func() {
		if probeRunning || len(inventory.NetworkInterfaces) == 0 || len(probeTargets) == 0 {
			return
		}
		probeRunning = true
		localInterfaces := append([]NetworkInterfaceInventory(nil), inventory.NetworkInterfaces...)
		targets := append([]NetworkProbeTarget(nil), probeTargets...)
		generation := probeGeneration
		go func() {
			probes := CollectNetworkPathProbes(streamContext, localInterfaces, targets)
			select {
			case probeResults <- networkProbeResult{generation: generation, probes: probes}:
			case <-streamContext.Done():
			}
		}()
	}
	var topologyTicks <-chan time.Time
	var topologyTicker *time.Ticker
	if containsCapability(accepted.NegotiatedCapabilities, topologyObservationCapability) {
		// Publish the inexpensive kernel topology immediately, then probe all
		// address pairs in the background so a large or unreachable network
		// cannot delay heartbeats, telemetry, or control commands.
		if err := sendTopology(true, nil); err != nil {
			return true, err
		}
		startNetworkProbes()
		topologyTicker = time.NewTicker(defaultTopologyInterval)
		defer topologyTicker.Stop()
		topologyTicks = topologyTicker.C
	}
	var flowTicks <-chan time.Time
	var flowTicker *time.Ticker
	if flowCollector != nil && containsCapability(accepted.NegotiatedCapabilities, flowObservationCapability) {
		flowTicker = time.NewTicker(flowCollector.config.Interval)
		defer flowTicker.Stop()
		flowTicks = flowTicker.C
	}
	sendAssignmentUpdate := func(update *xirangv1.AssignmentUpdate) error {
		return stream.Send(&xirangv1.AgentMessage{
			MessageId: agentMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_AssignmentUpdate{AssignmentUpdate: update},
		})
	}
	var assignmentUpdates <-chan *xirangv1.AssignmentUpdate
	var assignmentLogs <-chan *xirangv1.AssignmentLogSnapshot
	var assignmentUsage <-chan *xirangv1.AssignmentUsageSnapshot
	if containsCapability(accepted.NegotiatedCapabilities, "native-executor-v1") {
		assignmentUpdates = executor.Updates()
		assignmentUsage = executor.Usage()
		for _, update := range executor.Snapshots() {
			if err := sendAssignmentUpdate(update); err != nil {
				return true, err
			}
		}
	}
	if containsCapability(accepted.NegotiatedCapabilities, assignmentLogCapability) {
		assignmentLogs = executor.Logs()
	}
	var healthReports <-chan *xirangv1.HostHealthReport
	var healthUpdates <-chan struct{}
	durableHealth := config.healthWorker != nil && containsCapability(accepted.NegotiatedCapabilities, hosthealth.OutboxCapability)
	if containsCapability(accepted.NegotiatedCapabilities, hosthealth.Capability) {
		if config.healthWorker != nil {
			healthUpdates = config.healthWorker.updates
		} else {
			healthReports = healthCollector.reports(streamContext)
		}
	}
	var pendingHealth *pendingHealthBatch
	var healthTicks <-chan time.Time
	if durableHealth {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		healthTicks = ticker.C
	}
	sendHealthBatch := func() error {
		if !durableHealth {
			return nil
		}
		if pendingHealth != nil {
			if time.Since(pendingHealth.sentAt) > 30*time.Second {
				return errors.New("host event commit acknowledgement timed out")
			}
			return nil
		}
		batch, err := config.healthWorker.outbox.batch(time.Now())
		if err != nil {
			return fmt.Errorf("read host event outbox: %w", err)
		}
		if batch == nil {
			return nil
		}
		batch.sentAt = time.Now()
		pendingHealth = batch
		return stream.Send(&xirangv1.AgentMessage{MessageId: agentMessageID(), SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_HostEvents{HostEvents: batch.message}})
	}
	sendHealthSnapshot := func() error {
		if config.healthWorker == nil || !containsCapability(accepted.NegotiatedCapabilities, hosthealth.Capability) {
			return nil
		}
		report := config.healthWorker.snapshot(durableHealth)
		if report == nil {
			return nil
		}
		return stream.Send(&xirangv1.AgentMessage{MessageId: agentMessageID(), SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_HostHealth{HostHealth: report}})
	}
	var terminalEvents <-chan *xirangv1.TerminalEvent
	var mountResults <-chan *xirangv1.SharedMountResult
	var mounts *sharedMountManager
	if containsCapability(accepted.NegotiatedCapabilities, mountcontrol.Capability) {
		if config.mountExecution == nil {
			return true, errors.New("shared mount capability was not enabled locally")
		}
		mounts = newSharedMountManager(streamContext, config.mountExecution)
		mountResults = mounts.results
	}
	var terminalManager *TerminalManager
	if containsCapability(accepted.NegotiatedCapabilities, agentPTYCapability) {
		if config.terminalIdentity == nil {
			return true, errors.New("controller negotiated a terminal capability without a non-root terminal identity")
		}
		terminalManager, err = newTerminalManager(*config.terminalIdentity, []string{"/proc/self/exe", "__xirang-terminal-exec", "/bin/sh"})
		if err != nil {
			return true, err
		}
		terminalEvents = terminalManager.Events()
		defer terminalManager.CloseAll("Agent control stream disconnected")
	}

	type receivedMessage struct {
		message *xirangv1.ControlMessage
		err     error
	}
	received := make(chan receivedMessage, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case received <- receivedMessage{message: message, err: err}:
			case <-streamContext.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	heartbeatTicker := time.NewTicker(interval)
	defer heartbeatTicker.Stop()
	var telemetryTicks <-chan time.Time
	var telemetryTicker *time.Ticker
	if sampler != nil {
		telemetryTicker = time.NewTicker(telemetryInterval)
		defer telemetryTicker.Stop()
		telemetryTicks = telemetryTicker.C
	}
	var sequence uint64
	sendHeartbeat := func() error {
		sequence++
		panelEnabled, panelStatus := managementPanel.State()
		return stream.Send(&xirangv1.AgentMessage{
			MessageId: agentMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Heartbeat{Heartbeat: &xirangv1.Heartbeat{
				Sequence: sequence, Mode: commandStateMode(commands), RunningAssignments: executor.RunningCount(),
				ManagementPanelEnabled: panelEnabled, ManagementPanelStatus: panelStatus,
			}},
		})
	}
	if err := sendHeartbeat(); err != nil {
		return true, err
	}
	if err := sendHealthSnapshot(); err != nil {
		return true, err
	}
	if err := sendHealthBatch(); err != nil {
		return true, err
	}
	sendObservation := func() error {
		if sampler == nil {
			return nil
		}
		observation, err := sampler.Sample()
		if err != nil {
			return fmt.Errorf("sample node resources: %w", err)
		}
		return stream.Send(&xirangv1.AgentMessage{
			MessageId: agentMessageID(), SentAt: timestamppb.Now(),
			Payload: &xirangv1.AgentMessage_Observation{Observation: &xirangv1.ResourceObservation{
				UsedMilliCpu: observation.UsedMilliCPU, UsedMemoryBytes: observation.UsedMemoryBytes,
				ReceivedBytes: observation.ReceivedBytes, TransmittedBytes: observation.TransmittedBytes,
				InterfaceCounters: interfaceCounterSnapshots(observation.InterfaceCounters),
				CpuTemperatureC:   observation.CPUTemperatureC, GpuTemperatureC: observation.GPUTemperatureC,
				CpuTemperatureAvailable:  observation.CPUTemperatureAvailable,
				CpuTemperatureSource:     observation.CPUTemperatureSource,
				CpuTemperatureConfidence: observation.CPUTemperatureConfidence,
				GpuUtilizationPercent:    observation.GPUUtilization,
				GpuUtilizationAvailable:  observation.GPUUtilizationAvailable,
				GpuTemperatureAvailable:  observation.GPUTemperatureAvailable,
				AcceleratorMetrics:       acceleratorMetricSnapshots(observation.Accelerators),
			}},
		})
	}
	handlers := commandHandlers{
		mounts:          mounts,
		setBMCIdentify:  healthCollector.identifyCommand,
		executor:        executor,
		terminal:        terminalManager,
		nativeBuild:     buildManager,
		currentIdentity: func() string { return state.Identity },
		rotateCertificate: func(commandContext context.Context) (string, error) {
			rotated, _, rotateErr := RotateCertificate(commandContext, state, config.StateDirectory)
			if rotated.Identity != "" {
				state = rotated
			}
			return rotated.Identity, rotateErr
		},
		setTelemetryInterval: func(updated time.Duration) (bool, string) {
			if updated < 2*time.Second || updated > time.Minute {
				return false, "telemetry interval must be between 2s and 60s"
			}
			if telemetryTicker == nil {
				return false, "resource telemetry is unavailable"
			}
			telemetryInterval = updated
			telemetryTicker.Reset(updated)
			emitControlEvent(config, fmt.Sprintf("telemetry interval updated to %s", updated))
			return true, ""
		},
		setNetworkTargets: func(targets []*xirangv1.NetworkProbeTarget) (bool, string) {
			updated, validationErr := validatedNetworkProbeTargets(state.NodeID, targets)
			if validationErr != nil {
				return false, validationErr.Error()
			}
			probeTargets = updated
			probeGeneration++
			haveTopologyDigest = false
			startNetworkProbes()
			emitControlEvent(config, fmt.Sprintf("network probe target set updated targets=%d", len(updated)))
			return true, ""
		},
		setManagementPanel: managementPanel.SetEnabled,
	}
	for {
		select {
		case <-ctx.Done():
			_ = stream.CloseSend()
			return true, nil
		case incoming := <-received:
			if errors.Is(incoming.err, io.EOF) {
				return true, errors.New("control plane closed the stream")
			}
			if incoming.err != nil {
				return true, incoming.err
			}
			message := incoming.message
			if message.MessageId == "" || message.SentAt == nil {
				return true, errors.New("control command requires an ID and timestamp")
			}
			if committed := message.GetHostEventsCommitted(); committed != nil {
				if !durableHealth {
					return true, errors.New("unexpected host event acknowledgement capability")
				}
				if err := config.healthWorker.outbox.acknowledge(pendingHealth, committed.BatchId); err != nil {
					return true, err
				}
				pendingHealth = nil
				if err := sendHealthBatch(); err != nil {
					return true, err
				}
				continue
			}
			var acknowledgement *xirangv1.CommandAcknowledgement
			commands, acknowledgement, err = applyControlCommandWithHandlers(ctx, config.StateDirectory, commands, message, handlers)
			if err != nil {
				return true, err
			}
			if err := stream.Send(&xirangv1.AgentMessage{
				MessageId: agentMessageID(), SentAt: timestamppb.Now(),
				Payload: &xirangv1.AgentMessage_Acknowledgement{Acknowledgement: acknowledgement},
			}); err != nil {
				return true, err
			}
			emitControlEvent(config, fmt.Sprintf("control command acknowledged id=%s accepted=%t", acknowledgement.CommandMessageId, acknowledgement.Accepted))
		case update := <-assignmentUpdates:
			if err := sendAssignmentUpdate(update); err != nil {
				return true, err
			}
		case snapshot := <-assignmentLogs:
			if err := stream.Send(&xirangv1.AgentMessage{
				MessageId: agentMessageID(), SentAt: timestamppb.Now(),
				Payload: &xirangv1.AgentMessage_AssignmentLog{AssignmentLog: snapshot},
			}); err != nil {
				return true, err
			}
		case snapshot := <-assignmentUsage:
			if err := stream.Send(&xirangv1.AgentMessage{
				MessageId: agentMessageID(), SentAt: timestamppb.Now(),
				Payload: &xirangv1.AgentMessage_AssignmentUsage{AssignmentUsage: snapshot},
			}); err != nil {
				return true, err
			}
		case <-healthUpdates:
			if err := sendHealthSnapshot(); err != nil {
				return true, err
			}
			if err := sendHealthBatch(); err != nil {
				return true, err
			}
		case <-healthTicks:
			if err := sendHealthBatch(); err != nil {
				return true, err
			}
		case report, ok := <-healthReports:
			if !ok {
				healthReports = nil
				continue
			}
			if err := stream.Send(&xirangv1.AgentMessage{MessageId: agentMessageID(), SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_HostHealth{HostHealth: report}}); err != nil {
				return true, err
			}
		case event := <-terminalEvents:
			if err := stream.Send(&xirangv1.AgentMessage{
				MessageId: agentMessageID(), SentAt: timestamppb.Now(),
				Payload: &xirangv1.AgentMessage_TerminalEvent{TerminalEvent: event},
			}); err != nil {
				return true, err
			}
		case result := <-mountResults:
			if err := stream.Send(&xirangv1.AgentMessage{MessageId: agentMessageID(), SentAt: timestamppb.Now(), Payload: &xirangv1.AgentMessage_SharedMountResult{SharedMountResult: result}}); err != nil {
				return true, err
			}
		case update := <-buildManagerEvents(buildManager):
			if err := stream.Send(&xirangv1.AgentMessage{
				MessageId: agentMessageID(), SentAt: timestamppb.Now(),
				Payload: &xirangv1.AgentMessage_NativeBuildUpdate{NativeBuildUpdate: update},
			}); err != nil {
				return true, err
			}
		case <-heartbeatTicker.C:
			if err := sendHeartbeat(); err != nil {
				return true, err
			}
		case <-telemetryTicks:
			if err := sendObservation(); err != nil {
				return true, err
			}
		case <-topologyTicks:
			if len(probeTargets) > 0 {
				startNetworkProbes()
			} else {
				if err := sendTopology(false, nil); err != nil {
					return true, err
				}
			}
		case result := <-probeResults:
			probeRunning = false
			if result.generation != probeGeneration {
				startNetworkProbes()
				continue
			}
			if err := sendTopology(false, result.probes); err != nil {
				return true, err
			}
		case <-flowTicks:
			sample, err := flowCollector.Sample()
			if err != nil {
				emitControlEvent(config, fmt.Sprintf("eBPF flow sample failed: %v", err))
				continue
			}
			if err := stream.Send(&xirangv1.AgentMessage{
				MessageId: agentMessageID(), SentAt: timestamppb.Now(),
				Payload: &xirangv1.AgentMessage_Flows{Flows: flowObservationSnapshot(sample)},
			}); err != nil {
				return true, err
			}
		}
	}
}

func buildManagerEvents(manager *NativeBuildManager) <-chan *xirangv1.NativeBuildUpdate {
	if manager == nil {
		return nil
	}
	return manager.Events()
}

func detectedMPILauncherCapabilities() []string {
	result := make([]string, 0, 3)
	for _, launcher := range []string{"openmpi", "mpich", "intelmpi"} {
		path, err := exec.LookPath(mpiAdapterName(launcher))
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		output, probeErr := exec.CommandContext(ctx, path, "--xirang-probe").CombinedOutput()
		cancel()
		if probeErr == nil && strings.TrimSpace(string(output)) == "xirang-mpi-adapter-v1 "+launcher {
			result = append(result, mpiLaunchCapability+"/"+launcher)
		}
	}
	return result
}

func acceleratorMetricSnapshots(metrics []AcceleratorObservation) []*xirangv1.AcceleratorMetric {
	result := make([]*xirangv1.AcceleratorMetric, 0, len(metrics))
	for _, metric := range metrics {
		result = append(result, &xirangv1.AcceleratorMetric{
			Id: metric.ID, Vendor: metric.Vendor, UtilizationPercent: metric.UtilizationPercent,
			TemperatureC: metric.TemperatureC, MemoryUsedBytes: metric.MemoryUsedBytes,
			MemoryTotalBytes: metric.MemoryTotalBytes, UtilizationAvailable: metric.UtilizationAvailable,
			TemperatureAvailable: metric.TemperatureAvailable,
		})
	}
	return result
}

func flowObservationSnapshot(sample FlowSample) *xirangv1.FlowObservation {
	flows := make([]*xirangv1.NetworkFlow, 0, len(sample.Flows))
	for _, flow := range sample.Flows {
		flows = append(flows, &xirangv1.NetworkFlow{
			Family: flow.Family, Protocol: flow.Protocol,
			SourceAddress: flow.SourceAddress, SourcePort: uint32(flow.SourcePort),
			DestinationAddress: flow.DestinationAddress, DestinationPort: uint32(flow.DestinationPort),
			Direction: flow.Direction, Packets: flow.Packets, Bytes: flow.Bytes,
		})
	}
	return &xirangv1.FlowObservation{
		Flows: flows, Truncated: sample.Truncated, MapEntries: sample.MapEntries,
		Interval: durationpb.New(sample.Interval),
	}
}

func interfaceCounterSnapshots(counters []NetworkInterfaceCounters) []*xirangv1.NetworkInterfaceCounters {
	result := make([]*xirangv1.NetworkInterfaceCounters, 0, len(counters))
	for _, counter := range counters {
		result = append(result, &xirangv1.NetworkInterfaceCounters{
			Name: counter.Name, ReceivedBytes: counter.ReceivedBytes, TransmittedBytes: counter.TransmittedBytes,
			ReceivedPackets: counter.ReceivedPackets, TransmittedPackets: counter.TransmittedPackets,
			ReceiveErrors: counter.ReceiveErrors, TransmitErrors: counter.TransmitErrors,
			ReceiveDropped: counter.ReceiveDropped, TransmitDropped: counter.TransmitDropped,
			PacketCountersSupported: counter.PacketCountersSupported,
		})
	}
	return result
}

func topologySnapshot(observation TopologyObservation) *xirangv1.TopologyObservation {
	routes := make([]*xirangv1.Route, 0, len(observation.Routes))
	for _, route := range observation.Routes {
		routes = append(routes, &xirangv1.Route{
			Destination: route.Destination, Gateway: route.Gateway, InterfaceName: route.InterfaceName,
			Metric: route.Metric, Table: route.Table, Family: route.Family,
		})
	}
	neighbors := make([]*xirangv1.Neighbor, 0, len(observation.Neighbors))
	for _, neighbor := range observation.Neighbors {
		neighbors = append(neighbors, &xirangv1.Neighbor{
			Address: neighbor.Address, MacAddress: neighbor.MACAddress, InterfaceName: neighbor.InterfaceName,
			State: neighbor.State, Router: neighbor.Router,
		})
	}
	lldpNeighbors := make([]*xirangv1.LLDPNeighbor, 0, len(observation.LLDPNeighbors))
	for _, neighbor := range observation.LLDPNeighbors {
		lldpNeighbors = append(lldpNeighbors, &xirangv1.LLDPNeighbor{
			LocalInterface: neighbor.LocalInterface, ChassisId: neighbor.ChassisID, PortId: neighbor.PortID,
			SystemName: neighbor.SystemName, SystemDescription: neighbor.SystemDescription,
			ManagementAddresses: append([]string(nil), neighbor.ManagementAddresses...),
		})
	}
	pathProbes := make([]*xirangv1.NetworkPathProbe, 0, len(observation.PathProbes))
	for _, probe := range observation.PathProbes {
		pathProbes = append(pathProbes, &xirangv1.NetworkPathProbe{
			TargetNodeId:    probe.TargetNodeID,
			SourceInterface: probe.SourceInterface, SourceAddress: probe.SourceAddress,
			TargetInterface: probe.TargetInterface, TargetAddress: probe.TargetAddress,
			SourceSpeedBitsPerSecond: probe.SourceSpeedBitsPerSecond,
			TargetSpeedBitsPerSecond: probe.TargetSpeedBitsPerSecond,
			LatencyMicros:            probe.LatencyMicros, Reachable: probe.Reachable, Failure: probe.Failure,
		})
	}
	return &xirangv1.TopologyObservation{
		Routes: routes, Neighbors: neighbors, LldpNeighbors: lldpNeighbors, PathProbes: pathProbes,
	}
}

func inventorySnapshot(inventory Inventory) *xirangv1.InventorySnapshot {
	interfaces := make([]*xirangv1.NetworkInterface, 0, len(inventory.NetworkInterfaces))
	for _, networkInterface := range inventory.NetworkInterfaces {
		mtu := uint32(0)
		if networkInterface.MTU > 0 && uint64(networkInterface.MTU) <= uint64(^uint32(0)) {
			mtu = uint32(networkInterface.MTU)
		}
		interfaces = append(interfaces, &xirangv1.NetworkInterface{
			Name: networkInterface.Name, MacAddress: networkInterface.MACAddress,
			Addresses:          append([]string(nil), networkInterface.Addresses...),
			SpeedBitsPerSecond: networkInterface.SpeedBitsPerSecond, Mtu: mtu,
		})
	}
	devices := make([]*xirangv1.Device, 0, len(inventory.Devices))
	for _, device := range inventory.Devices {
		devices = append(devices, &xirangv1.Device{
			ResourceName: device.ResourceName, Vendor: device.Vendor, Model: device.Model,
			DeviceId: device.DeviceID, Attributes: cloneDeviceAttributes(device.Attributes),
		})
	}
	numaNodes := make([]*xirangv1.NUMANode, 0, len(inventory.NUMATopology))
	for _, node := range inventory.NUMATopology {
		memoryBytes := uint64(0)
		if node.MemoryBytes > 0 {
			memoryBytes = uint64(node.MemoryBytes)
		}
		numaNodes = append(numaNodes, &xirangv1.NUMANode{Id: node.ID, Cpus: node.CPUs, MemoryBytes: memoryBytes})
	}
	memoryBytes := uint64(0)
	if inventory.MemoryBytes > 0 {
		memoryBytes = uint64(inventory.MemoryBytes)
	}
	return &xirangv1.InventorySnapshot{
		Hostname: inventory.Hostname, Distribution: inventory.Distribution,
		KernelVersion: inventory.KernelVersion, Architecture: inventory.Architecture,
		LogicalCpuCount: uint32(inventory.CPUCount), MemoryBytes: memoryBytes,
		NetworkInterfaces: interfaces, Devices: devices, Labels: inventory.Labels, NumaNodes: numaNodes,
		CpuModel: inventory.CPUModel, CpuFeatures: inventory.CPUFeatures,
		CpuBenchmarkScore: inventory.CPUBenchmarkScore, ComputeWeight: inventory.ComputeWeight,
		RuntimeFacts: cloneDeviceAttributes(inventory.RuntimeFacts),
	}
}

func cloneDeviceAttributes(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func containsCapability(capabilities []string, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}
	return false
}

func validatedNetworkProbeTargets(nodeID string, candidates []*xirangv1.NetworkProbeTarget) ([]NetworkProbeTarget, error) {
	if len(candidates) > maxNetworkProbePairs {
		return nil, errors.New("network probe target set exceeds the safe limit")
	}
	targets := make([]NetworkProbeTarget, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.NodeId == "" || candidate.NodeId == nodeID ||
			len(candidate.NodeId) > 256 || candidate.InterfaceName == "" || len(candidate.InterfaceName) > 64 ||
			candidate.SpeedBitsPerSecond > 1_000_000_000_000_000 {
			return nil, errors.New("network probe target metadata is invalid")
		}
		address, ok := probeAddress(candidate.Address)
		if !ok {
			return nil, errors.New("network probe target address is invalid")
		}
		key := candidate.NodeId + "\x00" + candidate.InterfaceName + "\x00" + address.String()
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, NetworkProbeTarget{
			NodeID: candidate.NodeId, InterfaceName: candidate.InterfaceName,
			Address: address.String(), SpeedBitsPerSecond: candidate.SpeedBitsPerSecond,
		})
	}
	return targets, nil
}

func emitControlEvent(config ControlLoopConfig, message string) {
	if config.OnEvent != nil {
		config.OnEvent(message)
	}
}

func agentMessageID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "agent_" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "agent_" + hex.EncodeToString(buffer)
}
