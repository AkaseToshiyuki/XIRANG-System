package clusterops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/mountcontrol"
	"github.com/xirang-grid/xirang/internal/networkhealth"
)

var ErrInvalid = errors.New("invalid shared-space request")
var ErrForbidden = errors.New("shared-space operation forbidden")
var ErrNotFound = errors.New("shared-space entry not found")

type Repository interface {
	PutSharedSpace(context.Context, domain.SharedSpace) error
	GetSharedSpace(context.Context, string) (domain.SharedSpace, error)
	ListSharedSpaces(context.Context) ([]domain.SharedSpace, error)
	DeleteSharedSpace(context.Context, string) error
}

type Nodes interface {
	GetNode(context.Context, string) (domain.Node, error)
	ListNodes(context.Context) ([]domain.Node, error)
}

type Runner interface {
	Run(context.Context, domain.Node, ...string) ([]byte, error)
}

type Service struct {
	repository        Repository
	nodes             Nodes
	runner            Runner
	now               func() time.Time
	mu                sync.Mutex
	capacityMu        sync.Mutex
	capacityCache     map[string]capacityObservation
	defaultSourcePath string
	defaultMountPath  string
	storageQuotaFor   func(context.Context, domain.Principal) (int64, error)
	paths             *networkhealth.Resolver
}

func (service *Service) EnableNetworkPaths(paths *networkhealth.Resolver) { service.paths = paths }

type capacityObservation struct {
	usedBytes           uint64
	availableBytes      uint64
	totalBytes          uint64
	observedAt          time.Time
	quotaStatus         string
	quotaEnforcedBytes  uint64
	quotaAllocatedBytes uint64
	quotaError          string
}

const capacityCacheTTL = 30 * time.Second

func New(repository Repository, nodes Nodes, runner Runner) *Service {
	return &Service{
		repository: repository, nodes: nodes, runner: runner, now: time.Now,
		capacityCache: map[string]capacityObservation{},
	}
}

func (service *Service) EnableDefault(sourcePath, mountPath string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.defaultSourcePath = filepath.Clean(sourcePath)
	service.defaultMountPath = filepath.Clean(mountPath)
}

func (service *Service) EnableStorageQuotas(resolver func(context.Context, domain.Principal) (int64, error)) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.storageQuotaFor = resolver
}

// ResolveSharedPath returns a clean controller-visible path within an active
// shared-space mount. The caller performs operation-specific existence checks.
func (service *Service) ResolveSharedPath(ctx context.Context, spaceID, relative string) (string, error) {
	if spaceID == "" || relative == "" || filepath.IsAbs(relative) || !safeRelativeSharedPath(relative) {
		return "", ErrInvalid
	}
	space, err := service.repository.GetSharedSpace(ctx, spaceID)
	if err != nil {
		return "", err
	}
	if (space.Status != "active" && space.Status != "degraded") || !safePath(space.MountPath) {
		return "", fmt.Errorf("%w: shared space is not active", ErrInvalid)
	}
	base := filepath.Clean(space.MountPath)
	resolved := filepath.Join(base, filepath.FromSlash(relative))
	if resolved == base || !strings.HasPrefix(resolved, base+string(filepath.Separator)) {
		return "", ErrInvalid
	}
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("%w: shared-space mount is unavailable", ErrInvalid)
	}
	existing := resolved
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("%w: shared path is unavailable", ErrInvalid)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", ErrInvalid
		}
		existing = parent
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil || (resolvedExisting != resolvedBase && !strings.HasPrefix(resolvedExisting, resolvedBase+string(filepath.Separator))) {
		return "", fmt.Errorf("%w: shared path escapes its mount", ErrInvalid)
	}
	return resolved, nil
}

// AuthorizeSharedPath applies the embedded file manager's ownership rules
// before another subsystem reads from or publishes into a shared space.
func (service *Service) AuthorizeSharedPath(ctx context.Context, spaceID, relative string, principal domain.Principal, write bool) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	space, err := service.repository.GetSharedSpace(ctx, spaceID)
	if err != nil {
		return err
	}
	clean, err := cleanRelativePath(relative)
	if err != nil || clean == "" {
		return ErrInvalid
	}
	allowed := service.canManageFile(space, clean, principal)
	if write {
		allowed = service.canCreateOrManageFile(space, clean, principal)
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

func safeRelativeSharedPath(value string) bool {
	clean := filepath.Clean(filepath.FromSlash(value))
	return clean != "." && clean != ".." && !filepath.IsAbs(clean) &&
		!strings.HasPrefix(clean, ".."+string(filepath.Separator)) && !strings.ContainsRune(value, '\x00')
}

func (service *Service) List(ctx context.Context) ([]domain.SharedSpace, error) {
	spaces, err := service.repository.ListSharedSpaces(ctx)
	if err != nil {
		return nil, err
	}
	for index := range spaces {
		spaces[index].FileOwners = nil
		spaces[index].QuotaStatus = "unknown"
		// Retirement keeps an identity for audit, but not an operational mount
		// target. Also hide old persisted results before the next reconcile.
		service.pruneRetiredTargets(ctx, &spaces[index])
		if observation, ok := service.cachedCapacity(spaces[index].ID); ok {
			applyCapacity(&spaces[index], observation)
			continue
		}
		source, sourceErr := service.nodes.GetNode(ctx, spaces[index].SourceNodeID)
		if sourceErr != nil {
			spaces[index].CapacityError = "source node unavailable"
			continue
		}
		if nodeUnavailable(source) {
			spaces[index].CapacityError = "source node has been removed or rejected"
			continue
		}
		statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		output, statsErr := service.runner.Run(statsCtx, source, "stats", spaces[index].ID, spaces[index].SourcePath)
		cancel()
		if statsErr != nil {
			spaces[index].CapacityError = statsErr.Error()
			continue
		}
		observation, err := decodeSharedCapacity(output, service.now().UTC())
		if err != nil {
			spaces[index].CapacityError = "invalid capacity response"
			continue
		}
		service.storeCapacity(spaces[index].ID, observation)
		applyCapacity(&spaces[index], observation)
	}
	return spaces, nil
}

func (service *Service) cachedCapacity(spaceID string) (capacityObservation, bool) {
	service.capacityMu.Lock()
	defer service.capacityMu.Unlock()
	observation, ok := service.capacityCache[spaceID]
	if !ok || service.now().Sub(observation.observedAt) >= capacityCacheTTL {
		if ok {
			delete(service.capacityCache, spaceID)
		}
		return capacityObservation{}, false
	}
	return observation, true
}

func (service *Service) storeCapacity(spaceID string, observation capacityObservation) {
	service.capacityMu.Lock()
	defer service.capacityMu.Unlock()
	service.capacityCache[spaceID] = observation
}

func (service *Service) invalidateCapacity(spaceID string) {
	service.capacityMu.Lock()
	defer service.capacityMu.Unlock()
	delete(service.capacityCache, spaceID)
}

func applyCapacity(space *domain.SharedSpace, observation capacityObservation) {
	observedAt := observation.observedAt
	space.UsedBytes, space.AvailableBytes, space.TotalBytes =
		observation.usedBytes, observation.availableBytes, observation.totalBytes
	space.CapacityObservedAt = &observedAt
	space.QuotaStatus, space.QuotaEnforcedBytes, space.QuotaAllocatedBytes, space.QuotaError =
		observation.quotaStatus, observation.quotaEnforcedBytes, observation.quotaAllocatedBytes, observation.quotaError
}

func (service *Service) Apply(ctx context.Context, requested domain.SharedSpace) (domain.SharedSpace, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.applyLocked(ctx, requested)
}

func (service *Service) applyLocked(ctx context.Context, requested domain.SharedSpace) (domain.SharedSpace, error) {
	requested.Name = strings.TrimSpace(requested.Name)
	requested.SourcePath = filepath.Clean(requested.SourcePath)
	requested.MountPath = filepath.Clean(requested.MountPath)
	requested.TargetGroup = strings.TrimSpace(requested.TargetGroup)
	if requested.Name == "" || len(requested.Name) > 128 || !safePath(requested.SourcePath) || !safePath(requested.MountPath) {
		return domain.SharedSpace{}, ErrInvalid
	}
	switch requested.Target {
	case domain.SharedSpaceAll:
	case domain.SharedSpaceGroup:
		if requested.TargetGroup == "" {
			return domain.SharedSpace{}, ErrInvalid
		}
	case domain.SharedSpaceNodes:
		if len(requested.TargetNodeIDs) == 0 {
			return domain.SharedSpace{}, ErrInvalid
		}
	default:
		return domain.SharedSpace{}, ErrInvalid
	}
	source, err := service.nodes.GetNode(ctx, requested.SourceNodeID)
	if err != nil {
		return domain.SharedSpace{}, err
	}
	if nodeUnavailable(source) || !source.HasRole(domain.NodeRoleStorage) || source.ManagementAddress == "" || !hasStorageControl(source) {
		return domain.SharedSpace{}, fmt.Errorf("%w: source must be a storage node with shared-space control access", ErrInvalid)
	}
	nodes, err := service.nodes.ListNodes(ctx)
	if err != nil {
		return domain.SharedSpace{}, err
	}
	targets := selectTargets(nodes, requested)
	if len(targets) == 0 && requested.Target != domain.SharedSpaceGroup {
		return domain.SharedSpace{}, fmt.Errorf("%w: target selection is empty", ErrInvalid)
	}
	if requested.ID == "" {
		if requested.QuotaBytes != 0 {
			return domain.SharedSpace{}, fmt.Errorf("%w: create the shared space before setting its capacity limit", ErrInvalid)
		}
		requested.ID = "share_" + randomID()
		requested.CreatedAt = service.now().UTC()
		requested.FileOwners = map[string]domain.SharedFileOwner{}
	} else {
		current, getErr := service.repository.GetSharedSpace(ctx, requested.ID)
		if getErr != nil {
			return domain.SharedSpace{}, getErr
		}
		if current.Name != requested.Name || current.SourceNodeID != requested.SourceNodeID ||
			current.SourcePath != requested.SourcePath || current.MountPath != requested.MountPath {
			return domain.SharedSpace{}, fmt.Errorf("%w: name, source, and mount paths are immutable", ErrInvalid)
		}
		if current.ID == "share_default" && requested.Target != domain.SharedSpaceAll {
			return domain.SharedSpace{}, fmt.Errorf("%w: the default shared space always targets all nodes", ErrInvalid)
		}
		requested.CreatedAt = current.CreatedAt
		requested.Results = current.Results
		requested.Status = current.Status
		requested.FileOwners = current.FileOwners
		requested.QuotaBytes = current.QuotaBytes
	}
	// Capacity and enforcement are observations, never client-authored state.
	requested.QuotaStatus, requested.QuotaError = "", ""
	requested.QuotaEnforcedBytes, requested.QuotaAllocatedBytes = 0, 0
	requested.UpdatedAt = service.now().UTC()
	if err := service.repository.PutSharedSpace(ctx, requested); err != nil {
		return domain.SharedSpace{}, err
	}
	reconciled, err := service.reconcileSpaceLocked(ctx, requested, nodes)
	reconciled.FileOwners = nil
	return reconciled, err
}

func (service *Service) Reconcile(ctx context.Context) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	nodes, err := service.nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	spaces, err := service.repository.ListSharedSpaces(ctx)
	if err != nil {
		return err
	}
	if ensured, changed, ensureErr := service.ensureDefaultLocked(ctx, nodes, spaces); ensureErr != nil {
		return ensureErr
	} else if changed {
		replaced := false
		for index := range spaces {
			if spaces[index].ID == ensured.ID {
				spaces[index] = ensured
				replaced = true
				break
			}
		}
		if !replaced {
			spaces = append(spaces, ensured)
		}
	}
	var failures []error
	for _, space := range spaces {
		if _, reconcileErr := service.reconcileSpaceLocked(ctx, space, nodes); reconcileErr != nil {
			failures = append(failures, reconcileErr)
		}
	}
	return errors.Join(failures...)
}

func (service *Service) ensureDefaultLocked(ctx context.Context, nodes []domain.Node, spaces []domain.SharedSpace) (domain.SharedSpace, bool, error) {
	if !safePath(service.defaultSourcePath) || !safePath(service.defaultMountPath) {
		return domain.SharedSpace{}, false, nil
	}
	for _, space := range spaces {
		if space.ID == "share_default" {
			currentSource, sourceErr := service.nodes.GetNode(ctx, space.SourceNodeID)
			if sourceErr == nil && defaultSourceEligible(currentSource) && nodeListed(nodes, currentSource.ID) {
				return domain.SharedSpace{}, false, nil
			}
			preferredAddress := ""
			if sourceErr == nil {
				preferredAddress = currentSource.ManagementAddress
			}
			replacement, ok := selectDefaultSource(nodes, preferredAddress)
			if !ok {
				return domain.SharedSpace{}, false, nil
			}
			space.SourceNodeID = replacement.ID
			space.Status = "applying"
			space.Results = nil
			space.UpdatedAt = service.now().UTC()
			if err := service.repository.PutSharedSpace(ctx, space); err != nil {
				return domain.SharedSpace{}, false, err
			}
			service.invalidateCapacity(space.ID)
			return space, true, nil
		}
	}
	source, ok := selectDefaultSource(nodes, "")
	if !ok {
		return domain.SharedSpace{}, false, nil
	}
	now := service.now().UTC()
	space := domain.SharedSpace{
		ID: "share_default", Name: "XirangShare", SourceNodeID: source.ID,
		SourcePath: service.defaultSourcePath, MountPath: service.defaultMountPath,
		Target: domain.SharedSpaceAll, Status: "applying",
		FileOwners: map[string]domain.SharedFileOwner{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.repository.PutSharedSpace(ctx, space); err != nil {
		return domain.SharedSpace{}, false, err
	}
	return space, true, nil
}

func (service *Service) reconcileSpaceLocked(ctx context.Context, space domain.SharedSpace, nodes []domain.Node) (domain.SharedSpace, error) {
	original := space
	service.pruneRetiredTargets(ctx, &space)
	source, err := service.nodes.GetNode(ctx, space.SourceNodeID)
	if err != nil {
		space.Status = "failed"
		space.UpdatedAt = service.now().UTC()
		_ = service.repository.PutSharedSpace(ctx, space)
		return space, err
	}
	if nodeUnavailable(source) {
		err = errors.New("shared-space source node has been removed or rejected")
		space.Status = "failed"
		space.UpdatedAt = service.now().UTC()
		_ = service.repository.PutSharedSpace(ctx, space)
		return space, err
	}
	nodeByID := make(map[string]domain.Node, len(nodes))
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	previousTargets := map[string]domain.SharedSpaceNodeResult{}
	sourceExported := false
	for _, result := range space.Results {
		if result.NodeID == source.ID && result.Status == "exported" {
			sourceExported = true
			continue
		}
		previousTargets[result.NodeID] = result
	}
	desiredTargets := selectTargets(nodes, space)
	desiredIDs := make(map[string]bool, len(desiredTargets))
	for _, target := range desiredTargets {
		desiredIDs[target.ID] = true
	}

	results := make([]domain.SharedSpaceNodeResult, 0, len(desiredTargets)+1)
	status := "active"
	if !sourceExported {
		if _, exportErr := service.runner.Run(ctx, source, "export", space.ID, space.SourcePath); exportErr != nil {
			results = append(results, domain.SharedSpaceNodeResult{NodeID: source.ID, Status: "failed", Message: exportErr.Error()})
			status = "failed"
		} else {
			results = append(results, domain.SharedSpaceNodeResult{NodeID: source.ID, Status: "exported"})
			sourceExported = true
		}
	} else {
		results = append(results, domain.SharedSpaceNodeResult{NodeID: source.ID, Status: "exported"})
	}

	for nodeID, previous := range previousTargets {
		if desiredIDs[nodeID] || previous.Status == "failed" {
			continue
		}
		target, exists := nodeByID[nodeID]
		if !exists {
			results = append(results, domain.SharedSpaceNodeResult{NodeID: nodeID, Status: "unmount_failed", Message: "node is unavailable"})
			status = "degraded"
			continue
		}
		if nodeNotOnline(target) {
			results = append(results, domain.SharedSpaceNodeResult{NodeID: nodeID, Status: "unmount_failed", Message: "node is offline or unreachable"})
			status = "degraded"
			continue
		}
		if target.ID != source.ID || space.SourcePath != space.MountPath {
			if _, unmountErr := service.runner.Run(ctx, target, "unmount", space.ID, space.MountPath); unmountErr != nil {
				results = append(results, domain.SharedSpaceNodeResult{NodeID: nodeID, Status: "unmount_failed", Message: unmountErr.Error()})
				status = "degraded"
			}
		}
	}

	for _, target := range desiredTargets {
		// Rolling upgrades retain an older Agent's confirmed mount. It cannot
		// acknowledge the new path-change contract until its capability appears.
		if service.paths != nil && !slices.Contains(target.Capabilities, mountcontrol.PathCapability) {
			if previous, ok := previousTargets[target.ID]; ok && (previous.Status == "mounted" || previous.Status == "available") {
				results = append(results, previous)
				continue
			}
		}
		var selected *networkhealth.Path
		var selectedPaths []networkhealth.Path
		if service.paths != nil && target.ID != source.ID {
			paths, pathErr := service.paths.Paths(ctx, target.ID, source.ID)
			if pathErr != nil || len(paths) == 0 {
				previous := previousTargets[target.ID]
				previous.NodeID, previous.Status, previous.Message = target.ID, "path_pending", "waiting for a fresh reciprocal network path"
				results = append(results, previous)
				if status != "failed" {
					status = "degraded"
				}
				continue
			}
			selected = &paths[0]
			selectedPaths = paths
		}
		if previous, exists := previousTargets[target.ID]; exists &&
			(previous.Status == "mounted" || previous.Status == "available") &&
			(selected == nil || previous.SourceAddress == selected.TargetAddress && previous.ClientAddress == selected.SourceAddress && previous.NetworkInterface == selected.SourceInterface) {
			results = append(results, previous)
			continue
		}
		result := domain.SharedSpaceNodeResult{NodeID: target.ID, Status: "mounted"}
		switch {
		case !sourceExported:
			result.Status, result.Message = "failed", "source export is unavailable"
		case nodeNotOnline(target):
			result.Status, result.Message = "failed", "node is offline or unreachable"
		case target.ID == source.ID && space.SourcePath == space.MountPath:
			result.Status = "available"
		default:
			// Mount commands address the authenticated Agent by node ID. A
			// compute node needs neither an SSH account nor an inbound address.
			var mountErr error
			if selected == nil {
				_, mountErr = service.runner.Run(ctx, target, "mount", space.ID, source.ManagementAddress, space.SourcePath, space.MountPath)
			} else {
				for _, path := range selectedPaths {
					_, mountErr = service.runner.Run(ctx, target, "repath", space.ID, path.TargetAddress, space.SourcePath, space.MountPath, path.SourceAddress, path.SourceInterface)
					if mountErr == nil {
						result.SourceAddress, result.ClientAddress, result.NetworkInterface = path.TargetAddress, path.SourceAddress, path.SourceInterface
						break
					}
					// Only preflight failures permit trying another endpoint. Never
					// repeat an operation whose mutation outcome is uncertain.
					if !strings.Contains(mountErr.Error(), "selected shared path unavailable:") {
						break
					}
				}
			}
			if mountErr != nil {
				result.Status, result.Message = "failed", mountErr.Error()
				if selected != nil {
					result.SourceAddress, result.ClientAddress, result.NetworkInterface = "", "", ""
				}
			}
		}
		if result.Status == "failed" && status != "failed" {
			status = "degraded"
		}
		results = append(results, result)
	}
	if original.Status == status && sharedSpaceResultsEqual(original.Results, results) {
		return space, nil
	}
	space.Status, space.Results, space.UpdatedAt = status, results, service.now().UTC()
	return space, service.repository.PutSharedSpace(ctx, space)
}

// Only a confirmed tombstone permits forgetting a mount result. An absent
// list entry, a lookup error, or an offline host must retain pending cleanup.
// This removes controller bookkeeping; it does not claim the retired host
// was physically unmounted or that its files were deleted.
func (service *Service) pruneRetiredTargets(ctx context.Context, space *domain.SharedSpace) {
	results := make([]domain.SharedSpaceNodeResult, 0, len(space.Results))
	removed := false
	for _, result := range space.Results {
		if result.NodeID != space.SourceNodeID {
			node, err := service.nodes.GetNode(ctx, result.NodeID)
			if err == nil && (node.RemovedAt != nil || node.Status == domain.NodeStatusRemoved) {
				removed = true
				continue
			}
		}
		results = append(results, result)
	}
	if !removed {
		return
	}
	space.Results = results
	if space.Status == "degraded" {
		for _, result := range results {
			if result.Status == "failed" || result.Status == "unmount_failed" {
				return
			}
		}
		space.Status = "active"
	}
}

func sharedSpaceResultsEqual(left, right []domain.SharedSpaceNodeResult) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (service *Service) Delete(ctx context.Context, id string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if id == "share_default" && safePath(service.defaultSourcePath) && safePath(service.defaultMountPath) {
		return fmt.Errorf("%w: the default shared space cannot be deleted", ErrInvalid)
	}
	space, err := service.repository.GetSharedSpace(ctx, id)
	if err != nil {
		return err
	}
	source, _ := service.nodes.GetNode(ctx, space.SourceNodeID)
	nodes, err := service.nodes.ListNodes(ctx)
	if err != nil {
		return err
	}
	var failures []error
	targets := selectTargets(nodes, space)
	seen := map[string]bool{}
	for _, target := range targets {
		seen[target.ID] = true
	}
	for _, result := range space.Results {
		if seen[result.NodeID] || result.NodeID == source.ID {
			continue
		}
		if target, ok := nodeByID(nodes, result.NodeID); ok {
			targets = append(targets, target)
			seen[target.ID] = true
		}
	}
	for _, target := range targets {
		if target.ID == source.ID && space.SourcePath == space.MountPath {
			continue
		}
		if _, err := service.runner.Run(ctx, target, "unmount", space.ID, space.MountPath); err != nil {
			failures = append(failures, err)
		}
	}
	if _, err := service.runner.Run(ctx, source, "unexport", space.ID, space.SourcePath); err != nil {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if err := service.repository.DeleteSharedSpace(ctx, id); err != nil {
		return err
	}
	service.invalidateCapacity(id)
	return nil
}

func nodeByID(nodes []domain.Node, id string) (domain.Node, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return domain.Node{}, false
}

type SSHRunner struct {
	KeyPath        string
	KnownHostsPath string
	Paths          *networkhealth.Resolver
}

func (runner SSHRunner) Run(ctx context.Context, node domain.Node, arguments ...string) ([]byte, error) {
	var output bytes.Buffer
	if err := runner.RunStream(ctx, node, nil, &output, arguments...); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (runner SSHRunner) RunStream(ctx context.Context, node domain.Node, input io.Reader, output io.Writer, arguments ...string) error {
	if node.ManagementAddress == "" || node.SSHUser == "" || len(arguments) == 0 {
		return ErrInvalid
	}
	remote := []string{"sudo", "-n", "/usr/local/bin/xirang-agent", "mount-helper"}
	remote = append(remote, arguments...)
	address := node.ManagementAddress
	var routeArgs []string
	if runner.Paths != nil {
		paths, err := runner.Paths.Paths(ctx, "", node.ID)
		if err != nil {
			return fmt.Errorf("select file transfer network path: %w", err)
		}
		for _, path := range paths {
			if networkhealth.CheckRoute(ctx, path) != nil {
				continue
			}
			dialer := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(path.SourceAddress)}}
			connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(path.TargetAddress, "22"))
			if err != nil {
				continue
			}
			connection.Close()
			address = path.TargetAddress
			// Preserve the known host identity when its transport IP changes.
			routeArgs = []string{"-b", path.SourceAddress, "-o", "HostKeyAlias=" + node.ManagementAddress, "-o", "StrictHostKeyChecking=yes"}
			break
		}
		if len(paths) > 0 && len(routeArgs) == 0 {
			return errors.New("no observed file transfer path is currently reachable")
		}
	}
	commandArguments := []string{
		"-i", runner.KeyPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + runner.KnownHostsPath,
	}
	commandArguments = append(routeArgs, commandArguments...)
	commandArguments = append(commandArguments, node.SSHUser+"@"+address)
	commandArguments = append(commandArguments, remote...)
	command := exec.CommandContext(ctx, "ssh", commandArguments...)
	command.Stdin = input
	command.Stdout = output
	var errorOutput bytes.Buffer
	command.Stderr = &errorOutput
	err := command.Run()
	if err != nil {
		message := strings.TrimSpace(errorOutput.String())
		if len(message) > 512 {
			message = message[len(message)-512:]
		}
		return fmt.Errorf("mount command on %s failed: %s", node.Name, message)
	}
	return nil
}

func selectTargets(nodes []domain.Node, space domain.SharedSpace) []domain.Node {
	ids := make(map[string]bool, len(space.TargetNodeIDs))
	for _, id := range space.TargetNodeIDs {
		ids[id] = true
	}
	result := []domain.Node{}
	for _, node := range nodes {
		if nodeUnavailable(node) {
			continue
		}
		switch space.Target {
		case domain.SharedSpaceAll:
			result = append(result, node)
		case domain.SharedSpaceGroup:
			if node.Group == space.TargetGroup {
				result = append(result, node)
			}
		case domain.SharedSpaceNodes:
			if ids[node.ID] {
				result = append(result, node)
			}
		}
	}
	return result
}

func nodeUnavailable(node domain.Node) bool {
	return node.RemovedAt != nil || node.Status == domain.NodeStatusRemoved || node.Status == domain.NodeStatusRejected
}

func nodeNotOnline(node domain.Node) bool {
	return node.Status == domain.NodeStatusOffline || node.Status == domain.NodeStatusUnreachable
}

func nodeListed(nodes []domain.Node, id string) bool {
	_, ok := nodeByID(nodes, id)
	return ok
}

func defaultSourceEligible(node domain.Node) bool {
	return !nodeUnavailable(node) && node.HasRole(domain.NodeRoleManagement) && node.HasRole(domain.NodeRoleStorage) &&
		node.ManagementAddress != "" && hasStorageControl(node)
}

func hasStorageControl(node domain.Node) bool {
	return node.SSHUser != "" || slices.Contains(node.Capabilities, mountcontrol.Capability)
}

func selectDefaultSource(nodes []domain.Node, preferredAddress string) (domain.Node, bool) {
	var fallback domain.Node
	for _, node := range nodes {
		if !defaultSourceEligible(node) {
			continue
		}
		if preferredAddress != "" && node.ManagementAddress == preferredAddress {
			return node, true
		}
		if fallback.ID == "" || (fallback.Status != domain.NodeStatusReady && node.Status == domain.NodeStatusReady) {
			fallback = node
		}
	}
	return fallback, fallback.ID != ""
}

func safePath(value string) bool {
	if !filepath.IsAbs(value) || value == "/" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("/._-", character)) {
			return false
		}
	}
	return true
}

func randomID() string {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}
