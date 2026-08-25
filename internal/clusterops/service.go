package clusterops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
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
}

type capacityObservation struct {
	usedBytes      uint64
	availableBytes uint64
	totalBytes     uint64
	observedAt     time.Time
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
		if observation, ok := service.cachedCapacity(spaces[index].ID); ok {
			applyCapacity(&spaces[index], observation)
			continue
		}
		source, sourceErr := service.nodes.GetNode(ctx, spaces[index].SourceNodeID)
		if sourceErr != nil {
			spaces[index].CapacityError = "source node unavailable"
			continue
		}
		statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		output, statsErr := service.runner.Run(statsCtx, source, "stats", spaces[index].ID, spaces[index].SourcePath)
		cancel()
		if statsErr != nil {
			spaces[index].CapacityError = statsErr.Error()
			continue
		}
		var capacity struct {
			UsedBytes      uint64 `json:"usedBytes"`
			AvailableBytes uint64 `json:"availableBytes"`
			TotalBytes     uint64 `json:"totalBytes"`
		}
		if err := json.Unmarshal(output, &capacity); err != nil || capacity.TotalBytes == 0 {
			spaces[index].CapacityError = "invalid capacity response"
			continue
		}
		observation := capacityObservation{
			usedBytes: capacity.UsedBytes, availableBytes: capacity.AvailableBytes,
			totalBytes: capacity.TotalBytes, observedAt: service.now().UTC(),
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
	if !source.HasRole(domain.NodeRoleStorage) || source.ManagementAddress == "" || source.SSHUser == "" {
		return domain.SharedSpace{}, fmt.Errorf("%w: source must be a storage node with SSH access", ErrInvalid)
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
	}
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
	if created, ensureErr := service.ensureDefaultLocked(ctx, nodes, spaces); ensureErr != nil {
		return ensureErr
	} else if created.ID != "" {
		spaces = append(spaces, created)
	}
	var failures []error
	for _, space := range spaces {
		if _, reconcileErr := service.reconcileSpaceLocked(ctx, space, nodes); reconcileErr != nil {
			failures = append(failures, reconcileErr)
		}
	}
	return errors.Join(failures...)
}

func (service *Service) ensureDefaultLocked(ctx context.Context, nodes []domain.Node, spaces []domain.SharedSpace) (domain.SharedSpace, error) {
	if !safePath(service.defaultSourcePath) || !safePath(service.defaultMountPath) {
		return domain.SharedSpace{}, nil
	}
	for _, space := range spaces {
		if space.ID == "share_default" {
			return domain.SharedSpace{}, nil
		}
	}
	var source domain.Node
	for _, node := range nodes {
		if node.HasRole(domain.NodeRoleManagement) && node.HasRole(domain.NodeRoleStorage) &&
			node.ManagementAddress != "" && node.SSHUser != "" && node.Status != domain.NodeStatusRejected {
			source = node
			break
		}
	}
	if source.ID == "" {
		return domain.SharedSpace{}, nil
	}
	now := service.now().UTC()
	space := domain.SharedSpace{
		ID: "share_default", Name: "XirangShare", SourceNodeID: source.ID,
		SourcePath: service.defaultSourcePath, MountPath: service.defaultMountPath,
		Target: domain.SharedSpaceAll, Status: "applying",
		FileOwners: map[string]domain.SharedFileOwner{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.repository.PutSharedSpace(ctx, space); err != nil {
		return domain.SharedSpace{}, err
	}
	return space, nil
}

func (service *Service) reconcileSpaceLocked(ctx context.Context, space domain.SharedSpace, nodes []domain.Node) (domain.SharedSpace, error) {
	source, err := service.nodes.GetNode(ctx, space.SourceNodeID)
	if err != nil {
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
		if target.ID != source.ID || space.SourcePath != space.MountPath {
			if _, unmountErr := service.runner.Run(ctx, target, "unmount", space.ID, space.MountPath); unmountErr != nil {
				results = append(results, domain.SharedSpaceNodeResult{NodeID: nodeID, Status: "unmount_failed", Message: unmountErr.Error()})
				status = "degraded"
			}
		}
	}

	for _, target := range desiredTargets {
		if previous, exists := previousTargets[target.ID]; exists &&
			(previous.Status == "mounted" || previous.Status == "available") {
			results = append(results, previous)
			continue
		}
		result := domain.SharedSpaceNodeResult{NodeID: target.ID, Status: "mounted"}
		switch {
		case !sourceExported:
			result.Status, result.Message = "failed", "source export is unavailable"
		case target.ManagementAddress == "" || target.SSHUser == "":
			result.Status, result.Message = "failed", "SSH access is not configured"
		case target.ID == source.ID && space.SourcePath == space.MountPath:
			result.Status = "available"
		default:
			if _, mountErr := service.runner.Run(ctx, target, "mount", space.ID, source.ManagementAddress, space.SourcePath, space.MountPath); mountErr != nil {
				result.Status, result.Message = "failed", mountErr.Error()
			}
		}
		if result.Status == "failed" && status != "failed" {
			status = "degraded"
		}
		results = append(results, result)
	}
	if space.Status == status && sharedSpaceResultsEqual(space.Results, results) {
		return space, nil
	}
	space.Status, space.Results, space.UpdatedAt = status, results, service.now().UTC()
	return space, service.repository.PutSharedSpace(ctx, space)
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
	if id == "share_default" {
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
	commandArguments := []string{
		"-i", runner.KeyPath, "-o", "BatchMode=yes", "-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + runner.KnownHostsPath,
		node.SSHUser + "@" + node.ManagementAddress,
	}
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
		if node.Status == domain.NodeStatusRejected {
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
