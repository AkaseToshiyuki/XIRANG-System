package build

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/runtimecompat"
	"github.com/xirang-grid/xirang/internal/safefile"
)

var (
	ErrInvalidRequest = errors.New("invalid build request")
	ErrNotFound       = errors.New("build not found")
	ErrUnavailable    = errors.New("native build service unavailable")
	ErrConflict       = errors.New("build state conflict")
	ErrForbidden      = errors.New("build access forbidden")
)

type Repository interface {
	PutBuild(context.Context, domain.Build) error
	GetBuild(context.Context, string) (domain.Build, error)
	ListBuilds(context.Context, int) ([]domain.Build, error)
	DeleteBuild(context.Context, string) error
	GetBuildRetentionPolicy(context.Context) (domain.BuildRetentionPolicy, error)
	PutBuildRetentionPolicy(context.Context, domain.BuildRetentionPolicy) error
	AppendAuditEvent(context.Context, domain.AuditEvent) error
}

type Runner interface {
	Run(context.Context, string, domain.BuildRecipe) (string, error)
	Worker() domain.BuildWorker
}

// RunnerRegistry resolves a recipe at submission time. Registries may expose
// dynamic Agent workers; a returned Runner is pinned to that worker for the
// lifetime of the build.
type RunnerRegistry interface {
	Select(domain.BuildRecipe) (Runner, bool)
	Workers() []domain.BuildWorker
}

type StaticRegistry struct{ Runners []Runner }

func (registry StaticRegistry) Select(recipe domain.BuildRecipe) (Runner, bool) {
	builderArchitecture := recipe.BuilderArchitecture
	if builderArchitecture == "" {
		builderArchitecture = recipe.Architecture
	}
	for _, runner := range registry.Runners {
		if runner == nil {
			continue
		}
		worker := runner.Worker()
		if worker.Ready && worker.Architecture == builderArchitecture && (recipe.WorkerID == "" || recipe.WorkerID == worker.ID) && workerSupports(worker, recipe) {
			return runner, true
		}
	}
	return nil, false
}

func (registry StaticRegistry) Workers() []domain.BuildWorker {
	workers := make([]domain.BuildWorker, 0, len(registry.Runners))
	for _, runner := range registry.Runners {
		if runner != nil {
			workers = append(workers, runner.Worker())
		}
	}
	return workers
}

type CompositeRegistry []RunnerRegistry

func (registry CompositeRegistry) Select(recipe domain.BuildRecipe) (Runner, bool) {
	for _, candidate := range registry {
		if candidate != nil {
			if runner, ok := candidate.Select(recipe); ok {
				return runner, true
			}
		}
	}
	return nil, false
}

func (registry CompositeRegistry) Workers() []domain.BuildWorker {
	var workers []domain.BuildWorker
	for _, candidate := range registry {
		if candidate != nil {
			workers = append(workers, candidate.Workers()...)
		}
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
	return workers
}

type Config struct {
	SourceRoot           string
	WorkspaceRoot        string
	ArtifactRoot         string
	ArtifactBaseURL      string
	SigningKeyPath       string
	Runner               Runner
	Registry             RunnerRegistry
	SharedWorkspace      bool
	SharedWorkspaceOwner string
	ResolveSharedPath    func(context.Context, string, string) (string, error)
	AuthorizeSharedPath  func(context.Context, string, string, domain.Principal, bool) error
	RetentionPolicy      domain.BuildRetentionPolicy
}

type activeBuild struct {
	cancel    context.CancelFunc
	canCancel bool
}

type Service struct {
	repository                                               Repository
	sourceRoot, workspaceRoot, artifactRoot, artifactBaseURL string
	registry                                                 RunnerRegistry
	privateKey                                               ed25519.PrivateKey
	publicKey                                                ed25519.PublicKey
	signingKeyPath                                           string
	keyActivatedAt                                           time.Time
	keyMu                                                    sync.RWMutex
	now                                                      func() time.Time
	sharedWorkspace                                          bool
	resolveSharedPath                                        func(context.Context, string, string) (string, error)
	authorizeSharedPath                                      func(context.Context, string, string, domain.Principal, bool) error
	sharedUID, sharedGID                                     int
	activeMu                                                 sync.Mutex
	active                                                   map[string]activeBuild
	retentionMu                                              sync.RWMutex
	retention                                                domain.BuildRetentionPolicy
}

func New(repository Repository, config Config) (*Service, error) {
	for _, path := range []string{config.SourceRoot, config.WorkspaceRoot, config.ArtifactRoot} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("%w: build roots must be absolute clean paths", ErrInvalidRequest)
		}
	}
	registry := config.Registry
	if registry == nil && config.Runner != nil {
		registry = StaticRegistry{Runners: []Runner{config.Runner}}
	}
	if registry == nil {
		return nil, ErrUnavailable
	}
	for _, path := range []string{config.SourceRoot, config.WorkspaceRoot, config.ArtifactRoot} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			return nil, err
		}
	}
	// Build children use an unprivileged UID. The workspace root is searchable
	// but not listable so that a worker can enter only the randomized directory
	// handed to it while other controller state stays private.
	workspaceMode := os.FileMode(0o711)
	sharedUID, sharedGID := -1, -1
	if config.SharedWorkspace {
		info, err := os.Stat(config.WorkspaceRoot)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o070 != 0o070 || info.Mode()&os.ModeSetgid == 0 {
			return nil, errors.New("shared build workspace must be a setgid directory writable by the controller group")
		}
		if config.SharedWorkspaceOwner == "" {
			return nil, errors.New("shared build workspace owner is required")
		}
		account, lookupErr := user.Lookup(config.SharedWorkspaceOwner)
		if lookupErr != nil {
			return nil, lookupErr
		}
		sharedGID64, gidErr := strconv.ParseInt(account.Gid, 10, 32)
		if gidErr != nil || sharedGID64 <= 0 {
			return nil, errors.New("shared build workspace owner has an invalid identity")
		}
		// Keep controller-created paths owned by the controller process while
		// assigning the isolated task group. Transferring ownership to the task
		// account would make the controller depend on CAP_FOWNER for every later
		// chmod in a nested source tree. Group ownership is sufficient because
		// every build runner joins the task group.
		sharedUID, sharedGID = os.Geteuid(), int(sharedGID64)
	} else if err := os.Chmod(config.WorkspaceRoot, workspaceMode); err != nil {
		return nil, err
	}
	privateKey, publicKey, err := loadOrCreateSigningKey(config.SigningKeyPath)
	if err != nil {
		return nil, err
	}
	retention := normalizeRetention(config.RetentionPolicy)
	if stored, loadErr := repository.GetBuildRetentionPolicy(context.Background()); loadErr == nil {
		retention = normalizeRetention(stored)
	}
	service := &Service{
		repository: repository, sourceRoot: config.SourceRoot, workspaceRoot: config.WorkspaceRoot,
		artifactRoot: config.ArtifactRoot, artifactBaseURL: strings.TrimRight(config.ArtifactBaseURL, "/"),
		registry: registry, privateKey: privateKey, publicKey: publicKey,
		signingKeyPath: config.SigningKeyPath, keyActivatedAt: time.Now().UTC(), now: time.Now,
		sharedWorkspace:     config.SharedWorkspace,
		resolveSharedPath:   config.ResolveSharedPath,
		authorizeSharedPath: config.AuthorizeSharedPath,
		sharedUID:           sharedUID, sharedGID: sharedGID,
		active: make(map[string]activeBuild), retention: retention,
	}
	_ = repository.PutBuildRetentionPolicy(context.Background(), retention)
	if builds, err := repository.ListBuilds(context.Background(), 10_000); err == nil {
		now := service.now().UTC()
		for _, build := range builds {
			if build.Status == domain.BuildStatusRunning || build.Status == domain.BuildStatusCanceling {
				build.Status, build.Failure, build.FinishedAt = domain.BuildStatusFailed, "controller restarted before native build completion", &now
				_ = repository.PutBuild(context.Background(), build)
				_ = os.RemoveAll(filepath.Join(service.workspaceRoot, build.ID))
			}
		}
	}
	return service, nil
}

func (service *Service) Workers() []domain.BuildWorker {
	return service.registry.Workers()
}

func (service *Service) SigningKey() domain.BuildSigningKey {
	service.keyMu.RLock()
	defer service.keyMu.RUnlock()
	return service.signingKeyLocked()
}

func (service *Service) signingKeyLocked() domain.BuildSigningKey {
	digest := sha256.Sum256(service.publicKey)
	return domain.BuildSigningKey{
		PublicKey:   base64.StdEncoding.EncodeToString(service.publicKey),
		Fingerprint: hex.EncodeToString(digest[:]), ActivatedAt: service.keyActivatedAt,
	}
}

func (service *Service) RotateSigningKey(ctx context.Context, actor domain.Principal) (domain.BuildSigningKey, error) {
	service.keyMu.Lock()
	defer service.keyMu.Unlock()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return domain.BuildSigningKey{}, err
	}
	if err := persistSigningKey(service.signingKeyPath, privateKey); err != nil {
		return domain.BuildSigningKey{}, err
	}
	service.privateKey, service.publicKey, service.keyActivatedAt = privateKey, publicKey, service.now().UTC()
	key := service.signingKeyLocked()
	if err := service.repository.AppendAuditEvent(ctx, domain.AuditEvent{
		ID: "build_signing_key_rotated_" + key.Fingerprint, ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "build.signing_key.rotated", TargetKind: "build_signing_key", TargetID: key.Fingerprint,
		Outcome: "succeeded", OccurredAt: service.keyActivatedAt,
	}); err != nil {
		return domain.BuildSigningKey{}, err
	}
	return key, nil
}

func (service *Service) Submit(ctx context.Context, recipe domain.BuildRecipe, actor domain.Principal) (domain.Build, error) {
	if recipe.RuntimeRequirements == nil {
		recipe.RuntimeRequirements = make(map[string]string)
	}
	if _, present := recipe.RuntimeRequirements["os"]; !present {
		recipe.RuntimeRequirements["os"] = "linux"
	}
	if _, present := recipe.RuntimeRequirements["arch"]; !present && recipe.Architecture != "" {
		recipe.RuntimeRequirements["arch"] = recipe.Architecture
	}
	if recipe.Application != "" {
		if actor.Role != "admin" {
			return domain.Build{}, ErrForbidden
		}
		recipe.Application = strings.TrimSpace(recipe.Application)
		recipe.Variant = strings.TrimSpace(recipe.Variant)
		if recipe.RuntimeEntrypoint == "" {
			recipe.RuntimeEntrypoint = filepath.Base(recipe.OutputPath)
		}
		normalized, err := normalizeRuntimeNodeIDs(recipe.RuntimeNodeIDs)
		if err != nil {
			return domain.Build{}, err
		}
		recipe.RuntimeNodeIDs = normalized
	}
	if recipe.CrossCompile && recipe.Toolchain == "go" {
		environment := make(map[string]string, len(recipe.Environment)+2)
		for name, value := range recipe.Environment {
			environment[name] = value
		}
		environment["GOOS"], environment["GOARCH"] = "linux", recipe.Architecture
		recipe.Environment = environment
	}
	source, publish, sourceDigest, runner, err := service.validateRecipe(ctx, recipe, actor)
	if err != nil {
		return domain.Build{}, err
	}
	recipe.SourceSHA256 = sourceDigest
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return domain.Build{}, err
	}
	now := service.now().UTC()
	build := domain.Build{
		ID: "build_" + hex.EncodeToString(idBytes), OwnerID: principalID(actor), OwnerName: principalName(actor),
		Status: domain.BuildStatusRunning, Recipe: recipe,
		WorkerID: runner.Worker().ID, CreatedAt: now, StartedAt: now,
	}
	if err := service.repository.PutBuild(ctx, build); err != nil {
		return domain.Build{}, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	service.activeMu.Lock()
	service.active[build.ID] = activeBuild{cancel: cancel, canCancel: runner.Worker().SupportsCancellation}
	service.activeMu.Unlock()
	go service.execute(runCtx, build, source, publish, runner)
	return build, nil
}

func (service *Service) execute(ctx context.Context, build domain.Build, source, publish string, runner Runner) {
	workspace := filepath.Join(service.workspaceRoot, build.ID)
	defer func() {
		service.activeMu.Lock()
		delete(service.active, build.ID)
		service.activeMu.Unlock()
	}()
	release, acquireErr := AcquireRunner(ctx, runner)
	if acquireErr == nil {
		defer release()
	}
	if acquireErr != nil {
		build.Status, build.Failure = domain.BuildStatusFailed, bounded(acquireErr.Error(), 4096)
	} else if err := ctx.Err(); err != nil {
		build.Status, build.Failure = domain.BuildStatusCanceled, "native build canceled by administrator"
	} else if err := copyTreeOwnedContext(ctx, source, workspace, service.sharedWorkspace, service.sharedUID, service.sharedGID); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			build.Status, build.Failure = domain.BuildStatusCanceled, "native build canceled by administrator"
		} else {
			build.Status, build.Failure = domain.BuildStatusFailed, bounded(err.Error(), 4096)
		}
	} else {
		copiedDigest, digestErr := hashTreeContext(ctx, workspace)
		if errors.Is(ctx.Err(), context.Canceled) {
			build.Status, build.Failure = domain.BuildStatusCanceled, "native build canceled by administrator"
		} else if digestErr != nil || !strings.EqualFold(copiedDigest, build.Recipe.SourceSHA256) {
			build.Status, build.Failure = domain.BuildStatusFailed, "source changed while entering the private build workspace"
		} else {
			runCtx := ctx
			cancel := func() {}
			if build.Recipe.Timeout > 0 {
				runCtx, cancel = context.WithTimeout(ctx, build.Recipe.Timeout)
			}
			logs, runErr := runner.Run(runCtx, workspace, build.Recipe)
			cancel()
			build.LogTail = boundedTail(logs, 64<<10)
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(runErr, context.Canceled) {
				build.Status, build.Failure = domain.BuildStatusCanceled, "native build canceled by administrator"
			} else if runErr != nil {
				build.Status, build.Failure = domain.BuildStatusFailed, bounded(runErr.Error(), 4096)
			} else {
				expectedDigest := ""
				if reporter, ok := runner.(interface{ OutputDigest() string }); ok {
					expectedDigest = reporter.OutputDigest()
				}
				build.Artifact, runErr = service.finalizeArtifact(build, workspace, expectedDigest)
				if runErr != nil {
					build.Status, build.Failure = domain.BuildStatusFailed, bounded(runErr.Error(), 4096)
				} else if publish != "" {
					if publishErr := service.publishBuildOutput(filepath.Join(service.artifactRoot, build.ID, filepath.Base(build.Recipe.OutputPath)), publish); publishErr != nil {
						build.Status, build.Failure = domain.BuildStatusFailed, bounded(publishErr.Error(), 4096)
					} else if build.Recipe.Application != "" {
						publishErr := service.publishBuildOutput(filepath.Join(service.artifactRoot, build.ID, filepath.Base(build.Recipe.OutputPath)+".spdx.json"), publish+".spdx.json")
						if publishErr != nil {
							build.Status, build.Failure = domain.BuildStatusFailed, bounded(publishErr.Error(), 4096)
						} else {
							build.Artifact.URI = "file://" + publish
							build.Artifact.SBOMURI = "file://" + publish + ".spdx.json"
							// Shared application outputs have exactly one canonical copy.
							_ = os.RemoveAll(filepath.Join(service.artifactRoot, build.ID))
							build.Status = domain.BuildStatusSucceeded
						}
					} else {
						build.Status = domain.BuildStatusSucceeded
					}
				} else {
					build.Status = domain.BuildStatusSucceeded
				}
			}
		}
	}
	if acquireErr == nil {
		if err := CleanupRunnerWorkspace(runner, workspace); err != nil {
			build.Status, build.Failure = domain.BuildStatusFailed, bounded("workspace cleanup: "+err.Error(), 4096)
			build.Artifact = domain.BuildArtifact{}
		}
	}
	// Serialize terminal persistence with Cancel. A cancellation accepted before
	// the build leaves the active set must always win over success or an
	// incidental context-canceled copy error.
	service.activeMu.Lock()
	if errors.Is(ctx.Err(), context.Canceled) {
		build.Status, build.Failure = domain.BuildStatusCanceled, "native build canceled by administrator"
		build.Artifact = domain.BuildArtifact{}
	}
	finished := service.now().UTC()
	build.FinishedAt = &finished
	_ = service.repository.PutBuild(context.Background(), build)
	delete(service.active, build.ID)
	service.activeMu.Unlock()
	_, _ = service.Cleanup(context.Background(), domain.Principal{Subject: "system", AuthMethod: "controller"})
}

func (service *Service) Get(ctx context.Context, id string) (domain.Build, error) {
	return service.repository.GetBuild(ctx, id)
}

func (service *Service) List(ctx context.Context, limit int) ([]domain.Build, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrInvalidRequest
	}
	return service.repository.ListBuilds(ctx, limit)
}

// UpdateRuntimeTargets changes where a completed application variant may run.
// The compiled bytes remain immutable; only routing metadata changes.
func (service *Service) UpdateRuntimeTargets(ctx context.Context, id string, nodeIDs []string, actor domain.Principal) (domain.Build, error) {
	if actor.Role != "admin" {
		return domain.Build{}, ErrForbidden
	}
	build, err := service.repository.GetBuild(ctx, id)
	if err != nil {
		return domain.Build{}, err
	}
	if !canManageBuild(build, actor) {
		return domain.Build{}, ErrForbidden
	}
	if build.Status != domain.BuildStatusSucceeded || build.Artifact.Application == "" {
		return domain.Build{}, ErrConflict
	}
	normalized, err := normalizeRuntimeNodeIDs(nodeIDs)
	if err != nil {
		return domain.Build{}, err
	}
	now := service.now().UTC()
	build.Recipe.RuntimeNodeIDs = append([]string(nil), normalized...)
	build.Artifact.RuntimeNodeIDs = append([]string(nil), normalized...)
	build.Artifact.RoutingUpdatedAt = &now
	build.Artifact.RoutingApproved = true
	if err := service.repository.PutBuild(ctx, build); err != nil {
		return domain.Build{}, err
	}
	_ = service.repository.AppendAuditEvent(ctx, domain.AuditEvent{
		ID:        "build_runtime_targets_" + build.ID + "_" + strconv.FormatInt(now.UnixNano(), 10),
		ActorType: actor.AuthMethod, ActorID: actor.Subject, Action: "build.runtime_targets.updated",
		TargetKind: "native_build", TargetID: build.ID, Outcome: "succeeded", OccurredAt: now,
	})
	return build, nil
}

// ResolveApplication returns completed variants newest-routing-first. The
// first mapping wins if an administrator temporarily maps two variants to one node.
func (service *Service) ResolveApplication(ctx context.Context, application string) ([]domain.NativeArtifactVariant, error) {
	application = strings.TrimSpace(application)
	if application == "" || len(application) > 128 {
		return nil, ErrInvalidRequest
	}
	builds, err := service.repository.ListBuilds(ctx, 10_000)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(builds, func(i, j int) bool {
		left, right := builds[i].FinishedAt, builds[j].FinishedAt
		if builds[i].Artifact.RoutingUpdatedAt != nil {
			left = builds[i].Artifact.RoutingUpdatedAt
		}
		if builds[j].Artifact.RoutingUpdatedAt != nil {
			right = builds[j].Artifact.RoutingUpdatedAt
		}
		if left == nil {
			return false
		}
		if right == nil {
			return true
		}
		return left.After(*right)
	})
	variants := make([]domain.NativeArtifactVariant, 0)
	for _, build := range builds {
		artifact := build.Artifact
		if build.Status != domain.BuildStatusSucceeded || !artifact.RoutingApproved || artifact.Application != application || len(artifact.RuntimeNodeIDs) == 0 {
			continue
		}
		signature, decodeErr := base64.StdEncoding.DecodeString(artifact.Signature)
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: build %s has invalid artifact signature", ErrConflict, build.ID)
		}
		variants = append(variants, domain.NativeArtifactVariant{
			BuildID: build.ID, Variant: artifact.Variant, NodeIDs: append([]string(nil), artifact.RuntimeNodeIDs...),
			Artifact: domain.NativeArtifact{URI: artifact.URI, SHA256: artifact.SHA256, Architecture: artifact.Architecture,
				ABI: artifact.ABI, Entrypoint: artifact.Entrypoint, Signature: signature,
				RuntimeRequirements: cloneStringValues(artifact.RuntimeRequirements)},
		})
	}
	if len(variants) == 0 {
		return nil, ErrNotFound
	}
	return variants, nil
}

func (service *Service) Cancel(ctx context.Context, id string, actor domain.Principal) (domain.Build, error) {
	build, err := service.repository.GetBuild(ctx, id)
	if err != nil {
		return domain.Build{}, err
	}
	if !canManageBuild(build, actor) {
		return domain.Build{}, ErrForbidden
	}
	if build.Status.Terminal() {
		return domain.Build{}, ErrConflict
	}
	service.activeMu.Lock()
	active, ok := service.active[id]
	if !ok {
		service.activeMu.Unlock()
		return domain.Build{}, ErrConflict
	}
	if !active.canCancel {
		service.activeMu.Unlock()
		return domain.Build{}, fmt.Errorf("%w: selected build worker does not support cancellation", ErrUnavailable)
	}
	build.Status = domain.BuildStatusCanceling
	build.Failure = "cancellation requested by " + actor.Subject
	if err := service.repository.PutBuild(ctx, build); err != nil {
		service.activeMu.Unlock()
		return domain.Build{}, err
	}
	active.cancel()
	service.activeMu.Unlock()
	now := service.now().UTC()
	_ = service.repository.AppendAuditEvent(ctx, domain.AuditEvent{
		ID: "build_cancel_requested_" + id + "_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "build.cancel.requested", TargetKind: "native_build", TargetID: id, Outcome: "succeeded", OccurredAt: now,
	})
	return build, nil
}

func (service *Service) RetentionPolicy() domain.BuildRetentionPolicy {
	service.retentionMu.RLock()
	defer service.retentionMu.RUnlock()
	return service.retention
}

func (service *Service) UpdateRetentionPolicy(ctx context.Context, policy domain.BuildRetentionPolicy, actor domain.Principal) (domain.BuildRetentionPolicy, error) {
	if actor.Role != "admin" {
		return domain.BuildRetentionPolicy{}, ErrForbidden
	}
	if !validRetention(policy) {
		return domain.BuildRetentionPolicy{}, ErrInvalidRequest
	}
	policy = normalizeRetention(policy)
	if err := service.repository.PutBuildRetentionPolicy(ctx, policy); err != nil {
		return domain.BuildRetentionPolicy{}, err
	}
	service.retentionMu.Lock()
	service.retention = policy
	service.retentionMu.Unlock()
	now := service.now().UTC()
	_ = service.repository.AppendAuditEvent(ctx, domain.AuditEvent{
		ID: "build_retention_updated_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "build.retention.updated", TargetKind: "native_build_policy", TargetID: "retention", Outcome: "succeeded", OccurredAt: now,
	})
	return policy, nil
}

func (service *Service) Delete(ctx context.Context, id string, actor domain.Principal) (domain.BuildCleanupResult, error) {
	build, err := service.repository.GetBuild(ctx, id)
	if err != nil {
		return domain.BuildCleanupResult{}, err
	}
	if !canManageBuild(build, actor) || (build.Artifact.RoutingApproved && build.Artifact.Application != "" && actor.Role != "admin") {
		return domain.BuildCleanupResult{}, ErrForbidden
	}
	if !build.Status.Terminal() {
		return domain.BuildCleanupResult{}, ErrConflict
	}
	result := service.removeBuildFiles(build)
	if err := service.repository.DeleteBuild(ctx, id); err != nil {
		return domain.BuildCleanupResult{}, err
	}
	result.DeletedRecords = 1
	now := service.now().UTC()
	_ = service.repository.AppendAuditEvent(ctx, domain.AuditEvent{
		ID: "build_deleted_" + id + "_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "build.deleted", TargetKind: "native_build", TargetID: id, Outcome: "succeeded", OccurredAt: now,
	})
	return result, nil
}

func (service *Service) Cleanup(ctx context.Context, actor domain.Principal) (domain.BuildCleanupResult, error) {
	if actor.Role != "admin" && actor.Subject != "system" {
		return domain.BuildCleanupResult{}, ErrForbidden
	}
	builds, err := service.repository.ListBuilds(ctx, 10_000)
	if err != nil {
		return domain.BuildCleanupResult{}, err
	}
	policy := service.RetentionPolicy()
	cutoff := service.now().UTC().Add(-policy.MaximumAge)
	terminalIndex := 0
	var result domain.BuildCleanupResult
	for _, item := range builds {
		if !item.Status.Terminal() {
			continue
		}
		// Successful builds contain the signed artifact identity and, for
		// applications, the durable node-routing map. When preservation is
		// enabled automated cleanup must keep the complete record together.
		if policy.PreserveSuccessfulBuilds && item.Status == domain.BuildStatusSucceeded {
			continue
		}
		remove := terminalIndex >= policy.MaximumRecords || item.FinishedAt != nil && item.FinishedAt.Before(cutoff)
		terminalIndex++
		if !remove {
			continue
		}
		files := service.removeBuildFiles(item)
		if err := service.repository.DeleteBuild(ctx, item.ID); err != nil {
			return result, err
		}
		result.DeletedRecords++
		result.DeletedArtifacts += files.DeletedArtifacts
		result.ReclaimedBytes += files.ReclaimedBytes
	}
	if result.DeletedRecords > 0 {
		now := service.now().UTC()
		_ = service.repository.AppendAuditEvent(ctx, domain.AuditEvent{
			ID: "build_cleanup_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
			Action: "build.cleanup.completed", TargetKind: "native_build_policy", TargetID: "retention", Outcome: "succeeded", OccurredAt: now,
			Details: map[string]string{"deletedRecords": strconv.Itoa(result.DeletedRecords), "reclaimedBytes": strconv.FormatInt(result.ReclaimedBytes, 10)},
		})
	}
	return result, nil
}

func (service *Service) removeBuildFiles(build domain.Build) domain.BuildCleanupResult {
	directory := filepath.Join(service.artifactRoot, build.ID)
	var result domain.BuildCleanupResult
	_ = filepath.WalkDir(directory, func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			if info, infoErr := entry.Info(); infoErr == nil {
				result.ReclaimedBytes += info.Size()
				result.DeletedArtifacts++
			}
		}
		return nil
	})
	_ = os.RemoveAll(directory)
	_ = os.RemoveAll(filepath.Join(service.workspaceRoot, build.ID))
	return result
}

func normalizeRetention(policy domain.BuildRetentionPolicy) domain.BuildRetentionPolicy {
	if policy.MaximumRecords == 0 {
		policy.MaximumRecords = 500
	}
	if policy.MaximumAge == 0 {
		policy.MaximumAge = 30 * 24 * time.Hour
	}
	return policy
}

func validRetention(policy domain.BuildRetentionPolicy) bool {
	return policy.MaximumRecords >= 1 && policy.MaximumRecords <= 1000 && policy.MaximumAge >= 24*time.Hour && policy.MaximumAge <= 3650*24*time.Hour
}

func (service *Service) validateRecipe(ctx context.Context, recipe domain.BuildRecipe, actor domain.Principal) (string, string, string, Runner, error) {
	runner, available := service.registry.Select(recipe)
	if !available {
		return "", "", "", nil, fmt.Errorf("%w: no ready worker for builder architecture, node, toolchain, and target matrix", ErrUnavailable)
	}
	worker := runner.Worker()
	builderArchitecture := recipe.BuilderArchitecture
	if builderArchitecture == "" {
		builderArchitecture = recipe.Architecture
	}
	if strings.TrimSpace(recipe.Name) == "" || len(recipe.Name) > 128 ||
		builderArchitecture != worker.Architecture || (recipe.WorkerID != "" && recipe.WorkerID != worker.ID) ||
		(!recipe.CrossCompile && recipe.Architecture != builderArchitecture) || recipe.Architecture == "" ||
		len(buildSteps(recipe)) == 0 || len(buildSteps(recipe)) > 16 || len(recipe.Environment) > 128 ||
		(recipe.SourceSHA256 != "" && len(recipe.SourceSHA256) != 64) || recipe.MilliCPU < 10 || recipe.MemoryBytes <= 0 ||
		recipe.Timeout <= 0 || recipe.Timeout > 2*time.Hour || runtimecompat.ValidateRequirements(recipe.RuntimeRequirements) != nil {
		return "", "", "", nil, ErrInvalidRequest
	}
	if recipe.Application != "" {
		if len(recipe.Application) > 128 || strings.ContainsRune(recipe.Application, '\x00') || strings.TrimSpace(recipe.Variant) == "" || len(recipe.Variant) > 128 || strings.ContainsRune(recipe.Variant, '\x00') ||
			recipe.OutputSpaceID == "" || !safeRelative(recipe.RuntimeEntrypoint) {
			return "", "", "", nil, ErrInvalidRequest
		}
		normalized, normalizeErr := normalizeRuntimeNodeIDs(recipe.RuntimeNodeIDs)
		if normalizeErr != nil {
			return "", "", "", nil, normalizeErr
		}
		recipe.RuntimeNodeIDs = normalized
	}
	if !workerSupports(worker, recipe) || !safeRelative(recipe.SourcePath) || !safeRelative(recipe.OutputPath) {
		return "", "", "", nil, ErrInvalidRequest
	}
	source := filepath.Join(service.sourceRoot, filepath.FromSlash(recipe.SourcePath))
	if recipe.SourceSpaceID != "" {
		if service.resolveSharedPath == nil || actor.Role != "admin" && service.authorizeSharedPath == nil {
			return "", "", "", nil, ErrInvalidRequest
		}
		if actor.Role != "admin" {
			if authorizeErr := service.authorizeSharedPath(ctx, recipe.SourceSpaceID, recipe.SourcePath, actor, false); authorizeErr != nil {
				return "", "", "", nil, fmt.Errorf("%w: source shared path is not owned by the builder", ErrForbidden)
			}
		}
		var resolveErr error
		source, resolveErr = service.resolveSharedPath(ctx, recipe.SourceSpaceID, recipe.SourcePath)
		if resolveErr != nil {
			return "", "", "", nil, fmt.Errorf("%w: source shared path unavailable: %v", ErrInvalidRequest, resolveErr)
		}
	} else if actor.Role != "admin" {
		return "", "", "", nil, fmt.Errorf("%w: non-administrators must build from an owned shared path", ErrForbidden)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("%w: source unavailable", ErrInvalidRequest)
	}
	if recipe.SourceSpaceID == "" && !inside(service.sourceRoot, resolved) {
		return "", "", "", nil, fmt.Errorf("%w: source escapes configured root", ErrInvalidRequest)
	}
	digest, err := hashTreeContext(ctx, resolved)
	if err != nil {
		return "", "", "", nil, err
	}
	if recipe.SourceSHA256 != "" && !strings.EqualFold(digest, recipe.SourceSHA256) {
		return "", "", "", nil, fmt.Errorf("%w: source checksum mismatch", ErrInvalidRequest)
	}
	for _, step := range buildSteps(recipe) {
		if len(step) == 0 || len(step) > 64 {
			return "", "", "", nil, ErrInvalidRequest
		}
		for _, part := range step {
			if part == "" || len(part) > 4096 || strings.ContainsRune(part, '\x00') {
				return "", "", "", nil, ErrInvalidRequest
			}
		}
	}
	for name, value := range recipe.Environment {
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "=\x00") || len(value) > 8192 || strings.ContainsRune(value, '\x00') ||
			strings.HasPrefix(name, "XIRANG_") || name == "PATH" || name == "HOME" || name == "LANG" || name == "SOURCE_DATE_EPOCH" {
			return "", "", "", nil, ErrInvalidRequest
		}
	}
	publish := ""
	if recipe.OutputSpaceID != "" {
		if service.resolveSharedPath == nil || actor.Role != "admin" && service.authorizeSharedPath == nil {
			return "", "", "", nil, ErrInvalidRequest
		}
		publishPath := recipe.PublishPath
		if publishPath == "" {
			publishPath = recipe.OutputPath
		}
		if !safeRelative(publishPath) {
			return "", "", "", nil, ErrInvalidRequest
		}
		if actor.Role != "admin" {
			if authorizeErr := service.authorizeSharedPath(ctx, recipe.OutputSpaceID, publishPath, actor, true); authorizeErr != nil {
				return "", "", "", nil, fmt.Errorf("%w: output shared path is not writable by the builder", ErrForbidden)
			}
		}
		publish, err = service.resolveSharedPath(ctx, recipe.OutputSpaceID, publishPath)
		if err != nil {
			return "", "", "", nil, fmt.Errorf("%w: output shared path unavailable: %v", ErrInvalidRequest, err)
		}
	}
	return resolved, publish, digest, runner, nil
}

func principalID(principal domain.Principal) string {
	if strings.TrimSpace(principal.UserID) != "" {
		return strings.TrimSpace(principal.UserID)
	}
	return strings.TrimSpace(principal.Subject)
}

func principalName(principal domain.Principal) string {
	if strings.TrimSpace(principal.DisplayName) != "" {
		return strings.TrimSpace(principal.DisplayName)
	}
	return strings.TrimSpace(principal.Subject)
}

func canManageBuild(build domain.Build, actor domain.Principal) bool {
	return actor.Role == "admin" || build.OwnerID != "" && build.OwnerID == principalID(actor)
}

func buildSteps(recipe domain.BuildRecipe) [][]string {
	if len(recipe.Steps) > 0 {
		return recipe.Steps
	}
	if len(recipe.Command) > 0 {
		return [][]string{recipe.Command}
	}
	return nil
}

func normalizeRuntimeNodeIDs(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 1024 {
		return nil, ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || strings.ContainsRune(value, '\x00') {
			return nil, ErrInvalidRequest
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, ErrInvalidRequest
	}
	sort.Strings(result)
	return result, nil
}

func (service *Service) finalizeArtifact(build domain.Build, workspace string, expectedDigests ...string) (domain.BuildArtifact, error) {
	input, err := safefile.OpenRegular(workspace, build.Recipe.OutputPath)
	if err != nil {
		return domain.BuildArtifact{}, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<30 {
		return domain.BuildArtifact{}, errors.New("declared build output is missing, not regular, or exceeds 1 GiB")
	}
	data, err := io.ReadAll(io.LimitReader(input, (1<<30)+1))
	if err != nil {
		return domain.BuildArtifact{}, err
	}
	if int64(len(data)) != info.Size() {
		return domain.BuildArtifact{}, errors.New("build output changed size during finalization")
	}
	digest := sha256.Sum256(data)
	digestText := hex.EncodeToString(digest[:])
	if len(expectedDigests) > 0 && expectedDigests[0] != "" && !strings.EqualFold(expectedDigests[0], digestText) {
		return domain.BuildArtifact{}, errors.New("remote build output changed before controller finalization")
	}
	directory := filepath.Join(service.artifactRoot, build.ID)
	directoryMode := os.FileMode(0o750)
	if service.sharedWorkspace {
		directoryMode = 0o2770
	}
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return domain.BuildArtifact{}, err
	}
	if err := os.Chmod(directory, directoryMode); err != nil {
		return domain.BuildArtifact{}, err
	}
	if service.sharedWorkspace {
		if err := os.Chown(directory, service.sharedUID, service.sharedGID); err != nil {
			return domain.BuildArtifact{}, err
		}
	}
	name := filepath.Base(build.Recipe.OutputPath)
	destination := filepath.Join(directory, name)
	if err := os.WriteFile(destination, data, 0o550); err != nil {
		return domain.BuildArtifact{}, err
	}
	if err := os.Chmod(destination, 0o550); err != nil {
		return domain.BuildArtifact{}, err
	}
	if service.sharedWorkspace {
		if err := os.Chown(destination, service.sharedUID, service.sharedGID); err != nil {
			return domain.BuildArtifact{}, err
		}
	}
	abi := inspectABI(destination)
	if !abiMatchesArchitecture(abi, build.Recipe.Architecture) {
		return domain.BuildArtifact{}, fmt.Errorf("compiled output architecture does not match requested target %q", build.Recipe.Architecture)
	}
	sbomName := name + ".spdx.json"
	sbomPath := filepath.Join(directory, sbomName)
	sbom := map[string]any{
		"spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
		"name": build.Recipe.Name, "documentNamespace": "https://xirang.local/spdx/" + build.ID,
		"creationInfo": map[string]any{"created": service.now().UTC().Format(time.RFC3339), "creators": []string{"Tool: XIRANG Build Manager"}},
		"packages": []map[string]any{{
			"name": name, "SPDXID": "SPDXRef-Package", "versionInfo": digestText[:12],
			"checksums":        []map[string]string{{"algorithm": "SHA256", "checksumValue": digestText}},
			"downloadLocation": "NOASSERTION", "filesAnalyzed": false,
		}},
	}
	encodedSBOM, _ := json.MarshalIndent(sbom, "", "  ")
	if err := os.WriteFile(sbomPath, encodedSBOM, 0o440); err != nil {
		return domain.BuildArtifact{}, err
	}
	if err := os.Chmod(sbomPath, 0o440); err != nil {
		return domain.BuildArtifact{}, err
	}
	if service.sharedWorkspace {
		if err := os.Chown(sbomPath, service.sharedUID, service.sharedGID); err != nil {
			return domain.BuildArtifact{}, err
		}
	}
	uri := "file://" + destination
	sbomURI := "file://" + sbomPath
	if service.artifactBaseURL != "" {
		uri = service.artifactBaseURL + "/" + build.ID + "/" + name
		sbomURI = service.artifactBaseURL + "/" + build.ID + "/" + sbomName
	}
	service.keyMu.RLock()
	signature := ed25519.Sign(service.privateKey, digest[:])
	publicKey := append(ed25519.PublicKey(nil), service.publicKey...)
	service.keyMu.RUnlock()
	routingUpdatedAt := service.now().UTC()
	return domain.BuildArtifact{
		URI: uri, SHA256: digestText, Signature: base64.StdEncoding.EncodeToString(signature),
		PublicKey: base64.StdEncoding.EncodeToString(publicKey), Architecture: build.Recipe.Architecture,
		ABI: abi, Toolchain: build.Recipe.Toolchain, SBOMURI: sbomURI,
		Application: build.Recipe.Application, Variant: build.Recipe.Variant,
		Entrypoint: build.Recipe.RuntimeEntrypoint, RuntimeNodeIDs: append([]string(nil), build.Recipe.RuntimeNodeIDs...),
		RuntimeRequirements: cloneStringValues(build.Recipe.RuntimeRequirements),
		RoutingUpdatedAt:    &routingUpdatedAt,
		RoutingApproved:     build.Recipe.Application != "",
	}, nil
}

func cloneStringValues(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func loadOrCreateSigningKey(path string) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("%w: absolute signing key path required", ErrInvalidRequest)
	}
	if data, err := os.ReadFile(path); err == nil {
		decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil || len(decoded) != ed25519.PrivateKeySize {
			return nil, nil, errors.New("invalid build signing key")
		}
		privateKey := ed25519.PrivateKey(decoded)
		return privateKey, privateKey.Public().(ed25519.PublicKey), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if err := persistSigningKey(path, privateKey); err != nil {
		return nil, nil, err
	}
	return privateKey, publicKey, nil
}

func persistSigningKey(path string, privateKey ed25519.PrivateKey) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(base64.StdEncoding.EncodeToString(privateKey) + "\n"); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}

func hashTreeContext(ctx context.Context, root string) (string, error) {
	hash := sha256.New()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("source tree cannot contain symbolic links")
		}
		if entry.Type().IsRegular() {
			relative, _ := filepath.Rel(root, path)
			paths = append(paths, relative)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	for _, relative := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, filepath.ToSlash(relative)+"\x00")
		file, err := os.Open(filepath.Join(root, relative))
		if err != nil {
			return "", err
		}
		_, copyErr := copyWithContext(ctx, hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyTreeOwnedContext(ctx context.Context, source, destination string, sharedWorkspace bool, sharedUID, sharedGID int) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			mode := os.FileMode(0o750)
			if sharedWorkspace {
				mode = 0o2770
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("create build workspace directory %s: %w", relative, err)
			}
			if err := os.Chmod(target, mode); err != nil {
				return fmt.Errorf("protect build workspace directory %s: %w", relative, err)
			}
			if sharedWorkspace && sharedUID > 0 && sharedGID > 0 {
				if err := os.Chown(target, sharedUID, sharedGID); err != nil {
					return fmt.Errorf("assign shared build directory %s: %w", relative, err)
				}
			}
			return nil
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode := info.Mode().Perm() & 0o755
			if sharedWorkspace {
				mode = 0o660
				if info.Mode().Perm()&0o111 != 0 {
					mode = 0o770
				}
			}
			input, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("read build source %s: %w", relative, err)
			}
			output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_EXCL, mode)
			if err != nil {
				_ = input.Close()
				return fmt.Errorf("write build workspace file %s: %w", relative, err)
			}
			_, copyErr := copyWithContext(ctx, output, input)
			inputCloseErr := input.Close()
			outputCloseErr := output.Close()
			if copyErr != nil {
				return fmt.Errorf("copy build source %s: %w", relative, copyErr)
			}
			if inputCloseErr != nil || outputCloseErr != nil {
				return fmt.Errorf("close build source %s: %w", relative, errors.Join(inputCloseErr, outputCloseErr))
			}
			if err := os.Chmod(target, mode); err != nil {
				return fmt.Errorf("protect build workspace file %s: %w", relative, err)
			}
			if sharedWorkspace && sharedUID > 0 && sharedGID > 0 {
				if err := os.Chown(target, sharedUID, sharedGID); err != nil {
					return fmt.Errorf("assign shared build file %s: %w", relative, err)
				}
			}
			return nil
		}
		return errors.New("source contains unsupported filesystem object")
	})
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 1<<20)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			output, writeErr := destination.Write(buffer[:count])
			written += int64(output)
			if writeErr != nil {
				return written, writeErr
			}
			if output != count {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func inspectABI(path string) string {
	file, err := elf.Open(path)
	if err != nil {
		return "native-unknown"
	}
	defer file.Close()
	libraries, _ := file.ImportedLibraries()
	sort.Strings(libraries)
	return fmt.Sprintf("elf/%s/%s;libs=%s", file.Class, file.Machine, strings.Join(libraries, ","))
}

func abiMatchesArchitecture(abi, architecture string) bool {
	if abi == "native-unknown" {
		return true
	}
	machine := map[string]string{"amd64": "EM_X86_64", "arm64": "EM_AARCH64", "riscv64": "EM_RISCV", "ppc64le": "EM_PPC64"}[architecture]
	return machine != "" && strings.Contains(abi, "/"+machine+";")
}

func safeRelative(value string) bool {
	cleaned := filepath.Clean(filepath.FromSlash(value))
	return value != "" && cleaned != "." && !filepath.IsAbs(cleaned) && cleaned != ".." && !strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}
func inside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func bounded(value string, maximum int) string {
	if len(value) > maximum {
		return value[:maximum]
	}
	return value
}
func boundedTail(value string, maximum int) string {
	if len(value) > maximum {
		return value[len(value)-maximum:]
	}
	return value
}
