package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/pkg/version"
)

type client struct {
	baseURL    string
	adminToken string
	http       *http.Client
}

func main() {
	serverURL := flag.String("server", "https://127.0.0.1:8899", "XIRANG control-plane URL")
	tokenFile := flag.String("token-file", ".data/xirang/admin.token", "Bootstrap, OIDC, or API bearer token file")
	caFile := flag.String("ca-file", ".data/xirang/pki/ca.crt", "control-plane CA certificate")
	flag.Parse()
	command := "help"
	if flag.NArg() > 0 {
		command = flag.Arg(0)
	}

	if command == "version" {
		fmt.Printf("%s CLI %s\n", version.Name, version.Version)
		return
	}
	if command == "help" {
		printUsage()
		return
	}
	adminToken, _ := readToken(*tokenFile)
	httpClient, clientErr := trustedHTTPClient(*caFile)
	if clientErr != nil {
		fmt.Fprintln(os.Stderr, clientErr)
		os.Exit(1)
	}
	api := client{baseURL: strings.TrimRight(*serverURL, "/"), adminToken: adminToken, http: httpClient}
	var err error
	switch command {
	case "status":
		err = api.getAndPrint("/api/v1/version", false)
	case "nodes":
		err = api.getAndPrint("/api/v1/nodes", true)
	case "jobs":
		err = listJobs(api, flag.Args()[1:])
	case "submit-job":
		err = submitJob(api, flag.Args()[1:])
	case "submit-file":
		err = submitJobFile(api, flag.Args()[1:])
	case "cancel-job":
		err = cancelJob(api, flag.Args()[1:])
	case "usage":
		err = getUsage(api, flag.Args()[1:], false)
	case "usage-users":
		err = getUsage(api, flag.Args()[1:], true)
	case "billing-rates":
		err = api.getAndPrint("/api/v1/accounting/rates", true)
	case "billing-rate-create":
		err = createBillingRate(api, flag.Args()[1:])
	case "inventory":
		if flag.NArg() < 2 {
			err = errors.New("usage: xirangctl inventory <node-id>")
		} else {
			err = api.getAndPrint("/api/v1/nodes/"+url.PathEscape(flag.Arg(1))+"/inventory", true)
		}
	case "observations":
		err = listObservations(api, flag.Args()[1:])
	case "topology":
		err = getTopology(api, flag.Args()[1:])
	case "audit":
		err = listAuditEvents(api, flag.Args()[1:])
	case "token":
		err = createEnrollmentToken(api, flag.Args()[1:])
	case "discovery-scan":
		if flag.NArg() < 2 {
			err = errors.New("usage: xirangctl discovery-scan <cidr> [cidr...]")
		} else {
			err = api.postAndPrint("/api/v1/discovery/scans", map[string]any{"cidrs": flag.Args()[1:]}, true)
		}
	case "candidates":
		err = api.getAndPrint("/api/v1/discovery/candidates", true)
	case "candidate-approve", "candidate-reject":
		if flag.NArg() < 2 {
			err = fmt.Errorf("usage: xirangctl %s <candidate-id>", command)
		} else {
			action := strings.TrimPrefix(command, "candidate-")
			err = api.postAndPrint("/api/v1/discovery/candidates/"+url.PathEscape(flag.Arg(1))+"/"+action, nil, true)
		}
	case "builds":
		err = api.getAndPrint("/api/v1/builds?limit=100", true)
	case "build-workers":
		err = api.getAndPrint("/api/v1/build-workers", true)
	case "submit-build":
		err = submitBuildRecipe(api, flag.Args()[1:])
	case "build-signing-key":
		err = api.getAndPrint("/api/v1/build-signing-key", true)
	case "rotate-build-signing-key":
		err = api.postAndPrint("/api/v1/build-signing-key/rotate", nil, true)
	case "access-me":
		err = api.getAndPrint("/api/v1/access/me", true)
	case "api-tokens":
		err = api.getAndPrint("/api/v1/access/tokens", true)
	case "api-token-create":
		err = createAPIToken(api, flag.Args()[1:])
	case "api-token-revoke":
		if flag.NArg() < 2 {
			err = errors.New("usage: xirangctl api-token-revoke <token-id>")
		} else {
			err = api.postAndPrint("/api/v1/access/tokens/"+url.PathEscape(flag.Arg(1))+"/revoke", nil, true)
		}
	case "backups":
		err = api.getAndPrint("/api/v1/system/backups", true)
	case "backup-create":
		err = api.postAndPrint("/api/v1/system/backups", nil, true)
	case "backup-verify":
		if flag.NArg() < 2 {
			err = errors.New("usage: xirangctl backup-verify <backup-id>")
		} else {
			err = api.postAndPrint("/api/v1/system/backups/"+url.PathEscape(flag.Arg(1))+"/verify", nil, true)
		}
	case "approve", "reject":
		if flag.NArg() < 2 {
			err = fmt.Errorf("usage: xirangctl %s <node-id>", command)
		} else {
			err = api.postAndPrint("/api/v1/nodes/"+flag.Arg(1)+"/"+command, nil, true)
		}
	case "mode":
		if flag.NArg() < 3 {
			err = errors.New("usage: xirangctl mode <node-id> <ready|cordoned|draining>")
		} else {
			err = api.postAndPrint("/api/v1/nodes/"+flag.Arg(1)+"/mode", map[string]string{"mode": flag.Arg(2)}, true)
		}
	case "node-configure":
		err = configureNode(api, flag.Args()[1:])
	case "revoke-certificate":
		if flag.NArg() < 2 {
			err = errors.New("usage: xirangctl revoke-certificate <node-id>")
		} else {
			err = api.postAndPrint("/api/v1/nodes/"+flag.Arg(1)+"/certificate/revoke", nil, true)
		}
	case "rotate-certificate":
		if flag.NArg() < 2 {
			err = errors.New("usage: xirangctl rotate-certificate <node-id>")
		} else {
			err = api.postAndPrint("/api/v1/nodes/"+flag.Arg(1)+"/certificate/rotate-command", nil, true)
		}
	default:
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: xirangctl [global flags] <status|nodes|jobs|submit-job|submit-file|cancel-job|usage|usage-users|billing-rates|billing-rate-create|inventory|observations|topology|token|discovery-scan|candidates|candidate-approve|candidate-reject|builds|build-workers|submit-build|build-signing-key|rotate-build-signing-key|access-me|api-tokens|api-token-create|api-token-revoke|backups|backup-create|backup-verify|approve|reject|mode|node-configure|rotate-certificate|revoke-certificate|audit|version>")
}

func getUsage(api client, arguments []string, allUsers bool) error {
	flags := flag.NewFlagSet("usage", flag.ContinueOnError)
	today := time.Now().UTC()
	from := flags.String("from", today.AddDate(0, 0, -29).Format("2006-01-02"), "first UTC date (YYYY-MM-DD)")
	to := flags.String("to", today.Format("2006-01-02"), "last UTC date (YYYY-MM-DD)")
	ownerID := flags.String("owner", "", "owner ID to inspect (administrator only)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	values := url.Values{"from": {*from}, "to": {*to}}
	path := "/api/v1/accounting/summary"
	if allUsers {
		path = "/api/v1/accounting/users"
	} else if strings.TrimSpace(*ownerID) != "" {
		values.Set("ownerId", strings.TrimSpace(*ownerID))
	}
	return api.getAndPrint(path+"?"+values.Encode(), true)
}

func createBillingRate(api client, arguments []string) error {
	flags := flag.NewFlagSet("billing-rate-create", flag.ContinueOnError)
	currency := flags.String("currency", "CNY", "three-letter currency code")
	cpu := flags.String("cpu", "0", "price per CPU core-hour")
	memory := flags.String("memory", "0", "price per memory GiB-hour")
	gpu := flags.String("gpu", "0", "price per GPU device-hour")
	storage := flags.String("storage", "0", "price per workspace GiB-day")
	effectiveAt := flags.String("effective-at", "", "optional RFC3339 effective time")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	price := func(name, value string) (int64, error) {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed < 0 || parsed > float64(^uint64(0)>>1)/1_000_000 {
			return 0, fmt.Errorf("%s price must be a non-negative number", name)
		}
		return int64(parsed*1_000_000 + .5), nil
	}
	cpuMicros, err := price("CPU", *cpu)
	if err != nil {
		return err
	}
	memoryMicros, err := price("memory", *memory)
	if err != nil {
		return err
	}
	gpuMicros, err := price("GPU", *gpu)
	if err != nil {
		return err
	}
	storageMicros, err := price("storage", *storage)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"currency": *currency, "cpuCoreHourMicros": cpuMicros, "memoryGiBHourMicros": memoryMicros,
		"gpuDeviceHourMicros": gpuMicros, "storageGiBDayMicros": storageMicros,
	}
	if strings.TrimSpace(*effectiveAt) != "" {
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(*effectiveAt)); err != nil {
			return errors.New("effective-at must use RFC3339")
		}
		payload["effectiveAt"] = strings.TrimSpace(*effectiveAt)
	}
	return api.postAndPrint("/api/v1/accounting/rates", payload, true)
}

func configureNode(api client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: xirangctl node-configure <node-id> [options]")
	}
	nodeID := strings.TrimSpace(arguments[0])
	flags := flag.NewFlagSet("node-configure", flag.ContinueOnError)
	rolesText := flags.String("roles", "compute", "comma-separated management, compute, and storage roles")
	managementAddress := flags.String("management-address", "", "administrator-reachable SSH address")
	sshUser := flags.String("ssh-user", "", "existing SSH account")
	group := flags.String("group", "", "administrator-defined node group")
	computePriority := flags.Int("compute-priority", 0, "administrator scheduling preference from -1000 to 1000")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if nodeID == "" || flags.NArg() != 0 || *computePriority < -1000 || *computePriority > 1000 {
		return errors.New("node-configure requires a node ID, valid options, and compute priority from -1000 to 1000")
	}
	roles := make([]string, 0, 3)
	seen := make(map[string]bool)
	for _, raw := range strings.Split(*rolesText, ",") {
		role := strings.TrimSpace(strings.ToLower(raw))
		switch role {
		case "management", "compute", "storage":
		default:
			return fmt.Errorf("unsupported node role %q", role)
		}
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return errors.New("at least one node role is required")
	}
	return api.putAndPrint("/api/v1/nodes/"+url.PathEscape(nodeID)+"/configuration", map[string]any{
		"roles": roles, "managementAddress": strings.TrimSpace(*managementAddress),
		"sshUser": strings.TrimSpace(*sshUser), "group": strings.TrimSpace(*group),
		"computePriority": *computePriority,
	}, true)
}

func submitJobFile(api client, arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: xirangctl submit-file <job.xirang>")
	}
	encoded, err := os.ReadFile(arguments[0])
	if err != nil {
		return err
	}
	return api.postAndPrint("/api/v1/jobs/script", map[string]any{"script": string(encoded)}, true)
}

func createAPIToken(api client, arguments []string) error {
	flags := flag.NewFlagSet("api-token-create", flag.ContinueOnError)
	name := flags.String("name", "", "human-readable token name")
	role := flags.String("role", "viewer", "viewer, builder, operator, or admin")
	ttl := flags.Duration("ttl", 30*24*time.Hour, "token validity, at most 365 days")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" || *ttl <= 0 || *ttl > 365*24*time.Hour {
		return errors.New("api-token-create requires -name and a positive -ttl of at most 365 days")
	}
	switch *role {
	case "viewer", "builder", "operator", "admin":
	default:
		return errors.New("API token role must be viewer, builder, operator, or admin")
	}
	return api.postAndPrint("/api/v1/access/tokens", map[string]any{
		"name": strings.TrimSpace(*name), "role": *role, "ttlSeconds": int64(ttl.Seconds()),
	}, true)
}

func submitBuildRecipe(api client, arguments []string) error {
	flags := flag.NewFlagSet("submit-build", flag.ContinueOnError)
	recipeFile := flags.String("file", "", "JSON build recipe file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *recipeFile == "" {
		return errors.New("usage: xirangctl submit-build -file recipe.json")
	}
	encoded, err := os.ReadFile(*recipeFile)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return fmt.Errorf("decode build recipe: %w", err)
	}
	return api.postAndPrint("/api/v1/builds", payload, true)
}

func listJobs(api client, arguments []string) error {
	flags := flag.NewFlagSet("jobs", flag.ContinueOnError)
	limit := flags.Int("limit", 100, "maximum number of newest jobs")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("job limit must be between 1 and 1000")
	}
	return api.getAndPrint(fmt.Sprintf("/api/v1/jobs?limit=%d", *limit), true)
}

func submitJob(api client, arguments []string) error {
	flags := flag.NewFlagSet("submit-job", flag.ContinueOnError)
	project := flags.String("project", "default", "project identifier")
	queue := flags.String("queue", "default", "queue name")
	priority := flags.Int("priority", 0, "higher values are scheduled first")
	maxAttempts := flags.Int("max-attempts", 1, "maximum execution attempts")
	artifactURI := flags.String("artifact-uri", "", "file:// or https:// native artifact URI")
	sha256 := flags.String("sha256", "", "artifact SHA-256 in hexadecimal")
	architecture := flags.String("architecture", "", "required node and artifact architecture")
	abi := flags.String("abi", "", "optional native ABI identifier")
	signature := flags.String("signature", "", "optional base64 Ed25519 signature over the artifact SHA-256 bytes")
	entrypoint := flags.String("entrypoint", "", "relative executable path in the artifact")
	cpu := flags.Uint64("cpu-millicores", 1000, "CPU reservation in millicores")
	memoryMiB := flags.Uint64("memory-mib", 1024, "memory reservation in MiB")
	timeout := flags.Duration("timeout", 0, "optional execution timeout")
	workingDirectory := flags.String("working-directory", "", "relative artifact working directory")
	nodes := flags.Int("nodes", 1, "distinct nodes in the gang allocation")
	tasksPerNode := flags.Int("tasks-per-node", 1, "ranks launched on each node")
	launcher := flags.String("launcher", "native", "native, openmpi, mpich, or intelmpi")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *artifactURI == "" || *sha256 == "" || *architecture == "" || *entrypoint == "" {
		return errors.New("submit-job requires -artifact-uri, -sha256, -architecture, and -entrypoint")
	}
	if *priority < -1_000_000 || *priority > 1_000_000 || *maxAttempts < 1 || *maxAttempts > 100 || *timeout < 0 {
		return errors.New("priority, max-attempts, or timeout is outside the supported range")
	}
	if *cpu < 10 || *memoryMiB == 0 || *memoryMiB > ^uint64(0)/(1<<20) {
		return errors.New("CPU reservation must be at least 10 millicores and memory must be positive")
	}
	if *nodes < 1 || *nodes > 128 || *tasksPerNode < 1 || *tasksPerNode > 256 || (*nodes)*(*tasksPerNode) > 4096 ||
		(*launcher != "native" && *launcher != "openmpi" && *launcher != "mpich" && *launcher != "intelmpi") {
		return errors.New("parallel allocation requires 1-128 nodes, 1-256 tasks per node, at most 4096 ranks, and a supported launcher")
	}
	payload := map[string]any{
		"projectId": *project, "queue": *queue, "priority": *priority, "maxAttempts": *maxAttempts,
		"artifact": map[string]any{
			"uri": *artifactURI, "sha256": *sha256, "architecture": *architecture,
			"abi": *abi, "signature": *signature, "entrypoint": *entrypoint,
		},
		"workingDirectory": *workingDirectory,
		"resources": map[string]any{
			"milliCpu": *cpu, "memoryBytes": *memoryMiB * (1 << 20), "ephemeralStorageBytes": 0,
		},
		"requiredArchitecture": *architecture,
		"parallel":             map[string]any{"nodes": *nodes, "tasksPerNode": *tasksPerNode, "launcher": *launcher},
		"timeoutSeconds":       int64(timeout.Seconds()),
		"arguments":            flags.Args(),
	}
	return api.postAndPrint("/api/v1/jobs", payload, true)
}

func cancelJob(api client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: xirangctl cancel-job <job-id> [-reason text] [-grace 5s]")
	}
	jobID := arguments[0]
	flags := flag.NewFlagSet("cancel-job", flag.ContinueOnError)
	reason := flags.String("reason", "canceled by administrator", "cancellation reason")
	grace := flags.Duration("grace", 5*time.Second, "grace before SIGKILL")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if *grace < 0 || *grace > 5*time.Minute {
		return errors.New("cancel grace must be between zero and five minutes")
	}
	return api.postAndPrint("/api/v1/jobs/"+url.PathEscape(jobID)+"/cancel", map[string]any{
		"reason": *reason, "gracePeriodSeconds": int64(grace.Seconds()),
	}, true)
}

func getTopology(api client, arguments []string) error {
	flags := flag.NewFlagSet("topology", flag.ContinueOnError)
	version := flags.Uint64("version", 0, "specific topology snapshot version")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	path := "/api/v1/topology"
	if *version > 0 {
		path += fmt.Sprintf("?version=%d", *version)
	}
	return api.getAndPrint(path, true)
}

func listObservations(api client, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: xirangctl observations <node-id> [-limit 100]")
	}
	nodeID := arguments[0]
	flags := flag.NewFlagSet("observations", flag.ContinueOnError)
	limit := flags.Int("limit", 100, "maximum number of newest observations")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("observation limit must be between 1 and 1000")
	}
	return api.getAndPrint(fmt.Sprintf("/api/v1/nodes/%s/observations?limit=%d", url.PathEscape(nodeID), *limit), true)
}

func listAuditEvents(api client, arguments []string) error {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	limit := flags.Int("limit", 100, "maximum number of newest events")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("audit limit must be between 1 and 1000")
	}
	return api.getAndPrint(fmt.Sprintf("/api/v1/audit?limit=%d", *limit), true)
}

func createEnrollmentToken(api client, arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "create" {
		return fmt.Errorf("usage: xirangctl token create [options]")
	}
	flags := flag.NewFlagSet("token create", flag.ContinueOnError)
	name := flags.String("name", "node enrollment", "token description")
	ttl := flags.Duration("ttl", 15*time.Minute, "token validity")
	maxUses := flags.Uint("max-uses", 1, "maximum successful registrations")
	requireApproval := flags.Bool("require-approval", true, "require administrator approval")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	payload := map[string]any{
		"name": *name, "ttlSeconds": int64(ttl.Seconds()), "maxUses": *maxUses,
		"requireApproval": *requireApproval,
	}
	return api.postAndPrint("/api/v1/enrollment/tokens", payload, true)
}

func (api client) getAndPrint(path string, authenticated bool) error {
	request, err := http.NewRequest(http.MethodGet, api.baseURL+path, nil)
	if err != nil {
		return err
	}
	return api.executeAndPrint(request, authenticated)
}

func (api client) postAndPrint(path string, payload any, authenticated bool) error {
	return api.sendJSONAndPrint(http.MethodPost, path, payload, authenticated)
}

func (api client) putAndPrint(path string, payload any, authenticated bool) error {
	return api.sendJSONAndPrint(http.MethodPut, path, payload, authenticated)
}

func (api client) sendJSONAndPrint(method, path string, payload any, authenticated bool) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, api.baseURL+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return api.executeAndPrint(request, authenticated)
}

func (api client) executeAndPrint(request *http.Request, authenticated bool) error {
	if authenticated {
		if api.adminToken == "" {
			return errors.New("administrator token is unavailable; set -token-file")
		}
		request.Header.Set("Authorization", "Bearer "+api.adminToken)
	}
	response, err := api.http.Do(request)
	if err != nil {
		return fmt.Errorf("query control plane: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("control plane returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	formatted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(formatted))
	return nil
}

func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func trustedHTTPClient(caFile string) (*http.Client, error) {
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read control-plane CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid control-plane CA certificate")
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool,
	}}}, nil
}
