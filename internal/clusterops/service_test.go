package clusterops

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type memoryRepository struct{ spaces map[string]domain.SharedSpace }

func (repository *memoryRepository) PutSharedSpace(_ context.Context, space domain.SharedSpace) error {
	if repository.spaces == nil {
		repository.spaces = map[string]domain.SharedSpace{}
	}
	repository.spaces[space.ID] = space
	return nil
}

func TestResolveSharedPathStaysInsideActiveMount(t *testing.T) {
	mount := t.TempDir()
	service := New(&memoryRepository{spaces: map[string]domain.SharedSpace{
		"share": {ID: "share", Status: "active", MountPath: mount},
	}}, memoryNodes{}, &recordedRunner{})
	path, err := service.ResolveSharedPath(context.Background(), "share", "src/module")
	if err != nil || path != filepath.Join(mount, "src/module") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	for _, unsafe := range []string{"", "../secret", "/etc/passwd"} {
		if _, err := service.ResolveSharedPath(context.Background(), "share", unsafe); err == nil {
			t.Fatalf("unsafe path %q accepted", unsafe)
		}
	}
}
func (repository *memoryRepository) GetSharedSpace(_ context.Context, id string) (domain.SharedSpace, error) {
	return repository.spaces[id], nil
}
func (repository *memoryRepository) ListSharedSpaces(context.Context) ([]domain.SharedSpace, error) {
	result := []domain.SharedSpace{}
	for _, space := range repository.spaces {
		result = append(result, space)
	}
	return result, nil
}
func (repository *memoryRepository) DeleteSharedSpace(_ context.Context, id string) error {
	delete(repository.spaces, id)
	return nil
}

type memoryNodes []domain.Node

func (nodes memoryNodes) GetNode(_ context.Context, id string) (domain.Node, error) {
	for _, node := range nodes {
		if node.ID == id {
			return node, nil
		}
	}
	return domain.Node{}, ErrInvalid
}
func (nodes memoryNodes) ListNodes(context.Context) ([]domain.Node, error) { return nodes, nil }

type dynamicNodes struct{ nodes []domain.Node }

func (nodes *dynamicNodes) GetNode(ctx context.Context, id string) (domain.Node, error) {
	return memoryNodes(nodes.nodes).GetNode(ctx, id)
}
func (nodes *dynamicNodes) ListNodes(context.Context) ([]domain.Node, error) { return nodes.nodes, nil }

type recordedRunner struct{ calls [][]string }

func (runner *recordedRunner) Run(_ context.Context, node domain.Node, arguments ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string{node.ID}, arguments...))
	if len(arguments) > 0 && arguments[0] == "stats" {
		return []byte(`{"usedBytes":10,"availableBytes":90,"totalBytes":100}`), nil
	}
	return nil, nil
}

func TestApplySharedSpaceSelectsGroupAndPersistsResults(t *testing.T) {
	repository := &memoryRepository{}
	nodes := memoryNodes{
		{ID: "storage", Roles: []domain.NodeRole{domain.NodeRoleStorage}, ManagementAddress: "10.0.0.1", SSHUser: "admin"},
		{ID: "compute-a", Roles: []domain.NodeRole{domain.NodeRoleCompute}, Group: "gpu", ManagementAddress: "10.0.0.2", SSHUser: "admin"},
		{ID: "compute-b", Roles: []domain.NodeRole{domain.NodeRoleCompute}, Group: "cpu", ManagementAddress: "10.0.0.3", SSHUser: "admin"},
	}
	runner := &recordedRunner{}
	service := New(repository, nodes, runner)
	space, err := service.Apply(context.Background(), domain.SharedSpace{
		Name: "science", SourceNodeID: "storage", SourcePath: "/srv/science", MountPath: "/shared/science",
		Target: domain.SharedSpaceGroup, TargetGroup: "gpu",
	})
	if err != nil {
		t.Fatal(err)
	}
	if space.Status != "active" || len(space.Results) != 2 || len(runner.calls) != 2 {
		t.Fatalf("unexpected applied space: %#v calls=%#v", space, runner.calls)
	}
	if runner.calls[0][1] != "export" || runner.calls[1][0] != "compute-a" || runner.calls[1][1] != "mount" {
		t.Fatalf("unexpected commands: %#v", runner.calls)
	}
}

func TestSharedSpaceRejectsNonStorageSourceAndUnsafePath(t *testing.T) {
	nodes := memoryNodes{{ID: "node", Roles: []domain.NodeRole{domain.NodeRoleCompute}, ManagementAddress: "10.0.0.1", SSHUser: "admin"}}
	service := New(&memoryRepository{}, nodes, &recordedRunner{})
	for _, sourcePath := range []string{"/srv/data", "/srv/data with spaces"} {
		_, err := service.Apply(context.Background(), domain.SharedSpace{
			Name: "bad", SourceNodeID: "node", SourcePath: sourcePath, MountPath: "/shared", Target: domain.SharedSpaceAll,
		})
		if err == nil {
			t.Fatalf("request should fail for sourcePath=%q", sourcePath)
		}
	}
}

func TestListSharedSpacesReportsSourceFilesystemCapacity(t *testing.T) {
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{
		"share-a": {ID: "share-a", SourceNodeID: "storage", SourcePath: "/srv/science"},
	}}
	runner := &recordedRunner{}
	service := New(repository, memoryNodes{{
		ID: "storage", Roles: []domain.NodeRole{domain.NodeRoleStorage},
		ManagementAddress: "10.0.0.1", SSHUser: "admin",
	}}, runner)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	spaces, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 || spaces[0].UsedBytes != 10 || spaces[0].AvailableBytes != 90 ||
		spaces[0].TotalBytes != 100 || spaces[0].CapacityObservedAt == nil {
		t.Fatalf("unexpected capacity: %#v", spaces)
	}
	if len(runner.calls) != 1 || runner.calls[0][1] != "stats" {
		t.Fatalf("capacity was not read from source node: %#v", runner.calls)
	}
	if _, err := service.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("capacity should be cached between UI refreshes: %#v", runner.calls)
	}
	now = now.Add(capacityCacheTTL)
	if _, err := service.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("expired capacity should be measured again: %#v", runner.calls)
	}
}

func TestReconcileSharedSpaceFollowsDynamicGroupMembership(t *testing.T) {
	repository := &memoryRepository{}
	nodes := &dynamicNodes{nodes: []domain.Node{
		{ID: "storage", Roles: []domain.NodeRole{domain.NodeRoleStorage}, ManagementAddress: "10.0.0.1", SSHUser: "admin"},
		{ID: "compute-a", Roles: []domain.NodeRole{domain.NodeRoleCompute}, Group: "gpu", ManagementAddress: "10.0.0.2", SSHUser: "admin"},
		{ID: "compute-b", Roles: []domain.NodeRole{domain.NodeRoleCompute}, Group: "cpu", ManagementAddress: "10.0.0.3", SSHUser: "admin"},
	}}
	runner := &recordedRunner{}
	service := New(repository, nodes, runner)
	space, err := service.Apply(context.Background(), domain.SharedSpace{
		Name: "dynamic", SourceNodeID: "storage", SourcePath: "/srv/dynamic", MountPath: "/shared/dynamic",
		Target: domain.SharedSpaceGroup, TargetGroup: "gpu",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	nodes.nodes[1].Group = "cpu"
	nodes.nodes[2].Group = "gpu"
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "compute-a" || runner.calls[0][1] != "unmount" ||
		runner.calls[1][0] != "compute-b" || runner.calls[1][1] != "mount" {
		t.Fatalf("membership change was not reconciled: %#v", runner.calls)
	}
	updated := repository.spaces[space.ID]
	if len(updated.Results) != 2 || updated.Results[1].NodeID != "compute-b" || updated.Results[1].Status != "mounted" {
		t.Fatalf("unexpected reconciled state: %#v", updated)
	}
}

func TestReconcileCreatesDefaultSharedSpaceOnControllerStorageNode(t *testing.T) {
	repository := &memoryRepository{}
	nodes := memoryNodes{{
		ID: "controller", Roles: []domain.NodeRole{domain.NodeRoleManagement, domain.NodeRoleStorage},
		ManagementAddress: "10.0.0.1", SSHUser: "admin",
	}}
	service := New(repository, nodes, &recordedRunner{})
	service.EnableDefault("/srv/xirang/XirangShare", "/srv/xirang/XirangShare")
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	space, ok := repository.spaces["share_default"]
	if !ok || space.Target != domain.SharedSpaceAll || space.Name != "XirangShare" || space.Status != "active" {
		t.Fatalf("default shared space was not created: %#v", space)
	}
}

func TestReconcileMigratesDefaultSharedSpaceFromRemovedControllerIdentity(t *testing.T) {
	removedAt := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	createdAt := removedAt.Add(-24 * time.Hour)
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{
		"share_default": {
			ID: "share_default", Name: "XirangShare", SourceNodeID: "old-controller",
			SourcePath: "/srv/xirang/XirangShare", MountPath: "/srv/xirang/XirangShare",
			Target: domain.SharedSpaceAll, Status: "active", CreatedAt: createdAt,
			FileOwners: map[string]domain.SharedFileOwner{"artifact": {ID: "owner"}},
			Results:    []domain.SharedSpaceNodeResult{{NodeID: "old-controller", Status: "exported"}},
		},
	}}
	nodes := memoryNodes{
		{
			ID: "old-controller", Status: domain.NodeStatusRemoved, RemovedAt: &removedAt,
			Roles:             []domain.NodeRole{domain.NodeRoleManagement, domain.NodeRoleStorage},
			ManagementAddress: "10.0.0.1", SSHUser: "admin",
		},
		{
			ID: "new-controller", Status: domain.NodeStatusReady,
			Roles:             []domain.NodeRole{domain.NodeRoleManagement, domain.NodeRoleStorage},
			ManagementAddress: "10.0.0.1", SSHUser: "admin",
		},
	}
	runner := &recordedRunner{}
	service := New(repository, nodes, runner)
	service.EnableDefault("/srv/xirang/XirangShare", "/srv/xirang/XirangShare")
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	space := repository.spaces["share_default"]
	if space.SourceNodeID != "new-controller" || space.Status != "active" || !space.CreatedAt.Equal(createdAt) ||
		space.FileOwners["artifact"].ID != "owner" {
		t.Fatalf("default shared space was not safely migrated: %#v", space)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "new-controller" || runner.calls[0][1] != "export" {
		t.Fatalf("replacement source should be exported without mounting itself: %#v", runner.calls)
	}
	if len(space.Results) != 2 || space.Results[0].Status != "exported" || space.Results[1].Status != "available" {
		t.Fatalf("unexpected migrated results: %#v", space.Results)
	}
}

func TestListSharedSpacesDoesNotProbeRemovedSource(t *testing.T) {
	removedAt := time.Now().UTC()
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{
		"share": {ID: "share", SourceNodeID: "removed", SourcePath: "/srv/share"},
	}}
	runner := &recordedRunner{}
	service := New(repository, memoryNodes{{
		ID: "removed", Status: domain.NodeStatusRemoved, RemovedAt: &removedAt,
		Roles: []domain.NodeRole{domain.NodeRoleStorage}, ManagementAddress: "10.0.0.1", SSHUser: "admin",
	}}, runner)
	spaces, err := service.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 || spaces[0].CapacityError == "" || len(runner.calls) != 0 {
		t.Fatalf("removed source was probed: spaces=%#v calls=%#v", spaces, runner.calls)
	}
}

func TestReconcileRecordsOfflineTargetWithoutOpeningSSH(t *testing.T) {
	repository := &memoryRepository{}
	nodes := memoryNodes{
		{
			ID: "storage", Status: domain.NodeStatusReady, Roles: []domain.NodeRole{domain.NodeRoleStorage},
			ManagementAddress: "10.0.0.1", SSHUser: "admin",
		},
		{
			ID: "offline", Status: domain.NodeStatusOffline, Roles: []domain.NodeRole{domain.NodeRoleCompute},
			ManagementAddress: "10.0.0.2", SSHUser: "admin",
		},
	}
	runner := &recordedRunner{}
	service := New(repository, nodes, runner)
	space, err := service.Apply(context.Background(), domain.SharedSpace{
		Name: "science", SourceNodeID: "storage", SourcePath: "/srv/science", MountPath: "/shared/science",
		Target: domain.SharedSpaceAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if space.Status != "degraded" || len(runner.calls) != 2 {
		t.Fatalf("offline target should not be contacted: space=%#v calls=%#v", space, runner.calls)
	}
	if runner.calls[0][1] != "export" || runner.calls[1][0] != "storage" || runner.calls[1][1] != "mount" {
		t.Fatalf("unexpected shared-space commands: %#v", runner.calls)
	}
	for _, call := range runner.calls {
		if call[0] == "offline" {
			t.Fatalf("offline node received an SSH command: %#v", runner.calls)
		}
	}
}
