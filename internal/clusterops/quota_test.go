package clusterops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/xirang-grid/xirang/internal/domain"
)

type quotaRunner struct {
	limit          uint64
	fail, mismatch bool
	calls          int
}

func (runner *quotaRunner) Run(_ context.Context, _ domain.Node, args ...string) ([]byte, error) {
	runner.calls++
	if args[0] == "quota-set" {
		if runner.fail {
			return nil, errors.New("project quota enforcement is not enabled")
		}
		runner.limit, _ = strconv.ParseUint(args[3], 10, 64)
		return nil, nil
	}
	limit := runner.limit
	if runner.mismatch {
		limit += 1024
	}
	status := "unlimited"
	if limit != 0 {
		status = "enforced"
	}
	return []byte(fmt.Sprintf(`{"usedBytes":1,"availableBytes":8192,"totalBytes":16384,"quotaStatus":%q,"quotaEnforcedBytes":%d}`, status, limit)), nil
}

func TestQuotaUpdateRequiresHostEnforcementAndFreshReadback(t *testing.T) {
	ctx := context.Background()
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{"share": {ID: "share", SourceNodeID: "source", SourcePath: "/srv/share"}}}
	runner := &quotaRunner{}
	service := New(repository, memoryNodes{{ID: "source", Status: domain.NodeStatusReady}}, runner)
	// Seed a stale cached capacity observation. The update must not trust it.
	service.storeCapacity("share", capacityObservation{availableBytes: 1, observedAt: service.now()})
	space, err := service.UpdateQuota(ctx, "share", 4096)
	if err != nil || space.QuotaBytes != 4096 || space.QuotaEnforcedBytes != 4096 || space.QuotaStatus != "enforced" || runner.calls != 2 {
		t.Fatalf("update=%+v err=%v calls=%d", space, err, runner.calls)
	}
	runner.fail = true
	if _, err := service.UpdateQuota(ctx, "share", 2048); err == nil || repository.spaces["share"].QuotaBytes != 4096 {
		t.Fatal("failed host application overwrote the acknowledged limit")
	}
	if _, ok := service.cachedCapacity("share"); ok {
		t.Fatal("failed update retained stale enforcement observation")
	}
	runner.fail, runner.mismatch = false, true
	if _, err := service.UpdateQuota(ctx, "share", 2048); err == nil {
		t.Fatal("mismatched kernel observation was reported as success")
	}
	runner.mismatch = false
	space, err = service.UpdateQuota(ctx, "share", 0)
	if err != nil || space.QuotaStatus != "unlimited" || space.QuotaEnforcedBytes != 0 {
		t.Fatalf("clear=%+v err=%v", space, err)
	}
	before := runner.calls
	if _, err := service.UpdateQuota(ctx, "share", 1025); err == nil || runner.calls != before {
		t.Fatal("invalid unit reached privileged runner")
	}
}

func TestSharedTargetEditCannotForgeOrRemoveQuota(t *testing.T) {
	repository := &memoryRepository{spaces: map[string]domain.SharedSpace{"share": {ID: "share", Name: "share", SourceNodeID: "source", SourcePath: "/srv/share", MountPath: "/srv/share", Target: domain.SharedSpaceAll, QuotaBytes: 4096}}}
	service := New(repository, memoryNodes{{ID: "source", Roles: []domain.NodeRole{domain.NodeRoleStorage}, ManagementAddress: "source", SSHUser: "operator"}}, &recordedRunner{})
	request := repository.spaces["share"]
	request.QuotaBytes, request.QuotaEnforcedBytes, request.QuotaStatus = 0, 0, "unlimited"
	space, err := service.Apply(context.Background(), request)
	if err != nil || space.QuotaBytes != 4096 || space.QuotaStatus != "" {
		t.Fatalf("quota bypass: %+v %v", space, err)
	}
}
