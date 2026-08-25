package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xirang-grid/xirang/internal/agent"
	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/pkg/version"
)

func main() {
	command := "help"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "__xirang-build-exec":
		if err := buildmanager.RunExecutorLauncher(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(126)
		}
	case "__xirang-exec":
		if err := agent.RunExecutorLauncher(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(126)
		}
	case "__xirang-terminal-exec":
		if err := agent.RunTerminalLauncher(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(126)
		}
	case "__executor-stress-workload":
		if len(os.Args) != 3 {
			os.Exit(2)
		}
		if err := agent.RunExecutorStressWorkload(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "__executor-device-workload":
		if len(os.Args) != 4 {
			os.Exit(2)
		}
		if err := agent.RunExecutorDeviceWorkload(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "run":
		runAgent(os.Args[2:])
	case "enroll":
		enrollAgent(os.Args[2:])
	case "rotate-certificate":
		rotateCertificate(os.Args[2:])
	case "inventory":
		printInventory()
	case "resources":
		printResources()
	case "topology":
		printTopology()
	case "ebpf-probe":
		probeEBPF(os.Args[2:])
	case "executor-probe":
		probeExecutor(os.Args[2:])
	case "flows":
		printFlows(os.Args[2:])
	case "mount-helper":
		if err := agent.RunMountHelper(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "version", "--version":
		info := version.Current()
		fmt.Printf("%s agent %s (%s/%s)\n", version.Name, version.Version, info.OS, info.Architecture)
	default:
		fmt.Fprintln(os.Stderr, "usage: xirang-agent <run|enroll|rotate-certificate|inventory|resources|topology|ebpf-probe|executor-probe|flows|mount-helper|version>")
		if command != "help" && command != "--help" {
			os.Exit(2)
		}
	}
}

func probeExecutor(arguments []string) {
	flags := flag.NewFlagSet("executor-probe", flag.ExitOnError)
	taskUser := flags.String("task-user", "xirang-task", "dedicated non-root task user")
	taskGroup := flags.String("task-group", "", "task group (defaults to the user's primary group)")
	taskDirectory := flags.String("task-dir", "", "temporary task workspace root")
	cgroupRoot := flags.String("cgroup-root", "auto", "delegated cgroup v2 root")
	taskUIDMin := flags.Uint("task-uid-min", 0, "minimum temporary task UID (0 uses the secure default range)")
	taskUIDMax := flags.Uint("task-uid-max", 0, "maximum temporary task UID (0 uses the secure default range)")
	stress := flags.Bool("stress", false, "also verify CPU throttling, OOM containment, and cancellation")
	_ = flags.Parse(arguments)
	if *taskUIDMin > uint(^uint32(0)) || *taskUIDMax > uint(^uint32(0)) {
		fmt.Fprintln(os.Stderr, "task UID bounds must fit uint32")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := agent.ProbeNativeExecutor(ctx, agent.ExecutorIsolationConfig{
		TaskUser: *taskUser, TaskGroup: *taskGroup, TaskDirectory: *taskDirectory, CgroupRoot: *cgroupRoot,
		TaskUIDMin: uint32(*taskUIDMin), TaskUIDMax: uint32(*taskUIDMax),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *stress {
		stressResult, err := agent.ProbeNativeExecutorStress(ctx, agent.ExecutorIsolationConfig{
			TaskUser: *taskUser, TaskGroup: *taskGroup, TaskDirectory: *taskDirectory, CgroupRoot: *cgroupRoot,
			TaskUIDMin: uint32(*taskUIDMin), TaskUIDMax: uint32(*taskUIDMax),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		result.Stress = &stressResult
	}
	writeJSON(result)
}

func probeEBPF(arguments []string) {
	flags := flag.NewFlagSet("ebpf-probe", flag.ExitOnError)
	cgroupPath := flags.String("cgroup-path", "/sys/fs/cgroup", "cgroup v2 subtree to observe")
	_ = flags.Parse(arguments)
	writeJSON(agent.ProbeEBPFCapability(*cgroupPath))
}

func printFlows(arguments []string) {
	flags := flag.NewFlagSet("flows", flag.ExitOnError)
	cgroupPath := flags.String("cgroup-path", "/sys/fs/cgroup", "cgroup v2 subtree to observe")
	mapEntries := flags.Uint("map-entries", 4096, "maximum in-kernel flow aggregates")
	reportSize := flags.Int("limit", 256, "maximum flow aggregates to print")
	duration := flags.Duration("duration", 3*time.Second, "collection duration")
	_ = flags.Parse(arguments)
	if *mapEntries > uint(^uint32(0)) {
		fmt.Fprintln(os.Stderr, "-map-entries exceeds uint32")
		os.Exit(2)
	}
	collector, err := agent.NewEBPFFlowCollector(agent.FlowMonitoringConfig{
		Enabled: true, CgroupPath: *cgroupPath, MapEntries: uint32(*mapEntries), ReportSize: *reportSize, Interval: *duration,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer collector.Close()
	time.Sleep(*duration)
	sample, err := collector.Sample()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writeJSON(sample)
}

func writeJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printResources() {
	sampler, err := agent.NewResourceSampler()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// CPU utilization is derived from a counter delta; allow enough time for a
	// meaningful diagnostic sample without delaying the long-running Agent path.
	time.Sleep(250 * time.Millisecond)
	observation, err := sampler.Sample()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(observation); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printTopology() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observation, err := agent.CollectTopology(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(observation); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rotateCertificate(arguments []string) {
	flags := flag.NewFlagSet("rotate-certificate", flag.ExitOnError)
	stateDirectory := flags.String("state-dir", ".data/xirang-agent", "Agent state directory")
	_ = flags.Parse(arguments)
	state, err := agent.LoadState(*stateDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rotated, node, err := agent.RotateCertificate(ctx, state, *stateDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("activated certificate serial=%s node=%s revision=%d\n", rotated.Identity, node.ID, node.Revision)
}

func enrollAgent(arguments []string) {
	flags := flag.NewFlagSet("enroll", flag.ExitOnError)
	serverURL := flags.String("server", "https://127.0.0.1:8899", "control-plane URL")
	token := flags.String("token", "", "one-use enrollment token")
	tokenFile := flags.String("token-file", "", "mode 0600 file containing the enrollment token")
	caFile := flags.String("ca-file", "auto", "control-plane CA certificate, or auto to retrieve and confirm its fingerprint")
	acceptCAFingerprint := flags.String("accept-ca-fingerprint", "", "expected SHA-256 CA fingerprint when -ca-file=auto")
	stateDirectory := flags.String("state-dir", ".data/xirang-agent", "Agent state directory")
	_ = flags.Parse(arguments)
	if *token == "" && *tokenFile != "" {
		encoded, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		*token = strings.TrimSpace(string(encoded))
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "-token or -token-file is required")
		os.Exit(2)
	}
	if *caFile == "auto" {
		bootstrapContext, bootstrapCancel := context.WithTimeout(context.Background(), 10*time.Second)
		caPEM, fingerprint, err := agent.FetchControllerCA(bootstrapContext, *serverURL)
		bootstrapCancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		accepted := *acceptCAFingerprint == fingerprint
		if *acceptCAFingerprint != "" && !accepted {
			fmt.Fprintln(os.Stderr, "controller CA fingerprint does not match -accept-ca-fingerprint")
			os.Exit(1)
		}
		if !accepted {
			info, _ := os.Stdin.Stat()
			if info == nil || info.Mode()&os.ModeCharDevice == 0 {
				fmt.Fprintln(os.Stderr, "automatic CA bootstrap requires -accept-ca-fingerprint in non-interactive mode")
				os.Exit(2)
			}
			fmt.Printf("Controller CA SHA-256 fingerprint:\n%s\nTrust this controller? [yes/no]: ", fingerprint)
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
				fmt.Fprintln(os.Stderr, "controller CA was not trusted")
				os.Exit(2)
			}
		}
		if err := os.MkdirAll(*stateDirectory, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		temporaryCA := filepath.Join(*stateDirectory, ".enrollment-ca.crt")
		if err := os.WriteFile(temporaryCA, []byte(caPEM), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer os.Remove(temporaryCA)
		*caFile = temporaryCA
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := agent.Enroll(ctx, *serverURL, *token, *caFile, *stateDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("enrolled node %s; state saved under %s\n", state.NodeID, *stateDirectory)
}

func runAgent(arguments []string) {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	stateDirectory := flags.String("state-dir", ".data/xirang-agent", "Agent state directory")
	controlAddress := flags.String("control-address", "", "Agent gRPC address (defaults to control-plane host:8444)")
	bootstrapListen := flags.String("bootstrap-listen", ":8844", "TLS enrollment listener used only while this Agent has no cluster identity (empty disables)")
	enableFlows := flags.Bool("enable-ebpf-flows", false, "enable payload-free eBPF flow aggregation (requires Linux privileges)")
	flowCgroup := flags.String("flow-cgroup-path", "/sys/fs/cgroup", "cgroup v2 subtree observed by eBPF")
	flowMapEntries := flags.Uint("flow-map-entries", 4096, "maximum in-kernel flow aggregates")
	flowReportSize := flags.Int("flow-report-size", 256, "maximum flow aggregates per report")
	flowInterval := flags.Duration("flow-interval", 10*time.Second, "flow report interval")
	taskUser := flags.String("executor-task-user", "", "dedicated non-root user for cgroup-isolated native tasks")
	taskGroup := flags.String("executor-task-group", "", "dedicated task group (defaults to the task user's primary group)")
	cgroupRoot := flags.String("executor-cgroup-root", "", "delegated cgroup v2 root for native tasks; use auto for the Agent service cgroup")
	taskDirectory := flags.String("executor-task-dir", "", "isolated task workspace root")
	taskUIDMin := flags.Uint("executor-task-uid-min", 0, "minimum temporary task UID (0 uses the secure default range)")
	taskUIDMax := flags.Uint("executor-task-uid-max", 0, "maximum temporary task UID (0 uses the secure default range)")
	buildEnabled := flags.Bool("build-enabled", false, "advertise this Agent as an isolated native build worker")
	buildWorkspaceRoot := flags.String("build-workspace-root", "/srv/xirang/XirangShare/.xirang/builds/work", "shared native build workspace root")
	buildLocalWorkspaceRoot := flags.String("build-local-workspace-root", "/var/lib/xirang-build", "private local staging root for native builds")
	buildCgroupRoot := flags.String("build-cgroup-root", "", "delegated cgroup v2 root for native builds")
	buildUser := flags.String("build-user", "xirang-build", "dedicated non-root native build identity")
	buildGroup := flags.String("build-group", "", "native build group (defaults to the build user's primary group)")
	buildMaxConcurrent := flags.Int("build-max-concurrent", 1, "maximum concurrent native builds on this Agent (currently fixed at 1)")
	terminalUser := flags.String("terminal-user", "", "dedicated non-root identity for administrator terminal sessions (empty disables terminals)")
	var buildToolchains repeatedFlag
	flags.Var(&buildToolchains, "build-toolchain", "toolchain advertised by the native build worker; repeat as needed")
	var trustKeyFiles repeatedFlag
	flags.Var(&trustKeyFiles, "artifact-trust-key-file", "base64 Ed25519 build public key file; repeat during key rotation")
	_ = flags.Parse(arguments)
	if *flowMapEntries > uint(^uint32(0)) || *taskUIDMin > uint(^uint32(0)) || *taskUIDMax > uint(^uint32(0)) {
		fmt.Fprintln(os.Stderr, "flow-map-entries and task UID bounds must fit uint32")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	state, err := agent.LoadState(*stateDirectory)
	if errors.Is(err, os.ErrNotExist) && *bootstrapListen != "" {
		pairingCode, pairingErr := agent.BootstrapPairingCode(*stateDirectory)
		if pairingErr != nil {
			fmt.Fprintf(os.Stderr, "prepare Agent pairing code: %v\n", pairingErr)
			os.Exit(1)
		}
		fmt.Printf("waiting for controller approval on %s\n", *bootstrapListen)
		fmt.Printf("one-time Agent pairing code: %s\n", pairingCode)
		if err := agent.ServeBootstrapEnrollment(ctx, *bootstrapListen, *stateDirectory); err != nil {
			fmt.Fprintf(os.Stderr, "bootstrap enrollment: %v\n", err)
			os.Exit(1)
		}
		state, err = agent.LoadState(*stateDirectory)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "load Agent state: %v; run xirang-agent enroll first\n", err)
		os.Exit(1)
	}
	trustKeys, err := loadArtifactTrustKeys(trustKeyFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load artifact trust keys: %v\n", err)
		os.Exit(1)
	}
	if err := agent.RunControlLoop(ctx, state, agent.ControlLoopConfig{
		Address: *controlAddress, StateDirectory: *stateDirectory,
		OnEvent: func(message string) { fmt.Println(message) },
		FlowMonitoring: agent.FlowMonitoringConfig{
			Enabled: *enableFlows, CgroupPath: *flowCgroup, MapEntries: uint32(*flowMapEntries),
			ReportSize: *flowReportSize, Interval: *flowInterval,
		},
		ExecutorIsolation: agent.ExecutorIsolationConfig{
			CgroupRoot: *cgroupRoot, TaskUser: *taskUser, TaskGroup: *taskGroup, TaskDirectory: *taskDirectory,
			TaskUIDMin: uint32(*taskUIDMin), TaskUIDMax: uint32(*taskUIDMax),
		},
		NativeBuild: agent.NativeBuildConfig{
			Enabled: *buildEnabled, WorkspaceRoot: *buildWorkspaceRoot, LocalWorkspaceRoot: *buildLocalWorkspaceRoot, CgroupRoot: *buildCgroupRoot,
			User: *buildUser, Group: *buildGroup, Toolchains: buildToolchains, MaxConcurrent: *buildMaxConcurrent,
		},
		TerminalUser:      *terminalUser,
		ArtifactTrustKeys: trustKeys,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type repeatedFlag []string

func (values *repeatedFlag) String() string { return fmt.Sprint([]string(*values)) }
func (values *repeatedFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("file path cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func loadArtifactTrustKeys(paths []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(paths))
	for _, path := range paths {
		encoded, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s does not contain one base64 Ed25519 public key", path)
		}
		keys = append(keys, ed25519.PublicKey(decoded))
	}
	return keys, nil
}

func printInventory() {
	inventory, err := agent.CollectInventory()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(inventory); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
