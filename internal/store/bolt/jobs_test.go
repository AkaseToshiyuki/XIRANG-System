package bolt_test

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/xirang-grid/xirang/internal/domain"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestGroupDisableSuspendsAndResumesOnlyPolicyQueuedJobs(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	jobs := []domain.Job{
		{ID: "primary", GroupID: "group-a", Status: domain.JobStatusQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "secondary", GroupID: "group-b", PolicyGroupIDs: []string{"group-a", "group-b"}, Status: domain.JobStatusQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "other", GroupID: "group-c", Status: domain.JobStatusQueued, CreatedAt: now, UpdatedAt: now},
	}
	for _, job := range jobs {
		if err := store.CreateJob(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := store.SetGroupJobsSuspended(context.Background(), "group-a", true, "disabled", now.Add(time.Second))
	if err != nil || len(updated) != 2 {
		t.Fatalf("suspend updated=%d err=%v", len(updated), err)
	}
	for _, id := range []string{"primary", "secondary"} {
		job, getErr := store.GetJob(context.Background(), id)
		if getErr != nil || job.Status != domain.JobStatusSuspended || job.BlockReason == nil || job.BlockReason.Code != "group_disabled" {
			t.Fatalf("job %s was not policy-suspended: %#v err=%v", id, job, getErr)
		}
	}
	updated, err = store.SetGroupJobsSuspended(context.Background(), "group-b", true, "disabled", now.Add(2*time.Second))
	if err != nil || len(updated) != 1 || updated[0].ID != "secondary" || len(updated[0].SuspendedPolicyGroupIDs) != 2 {
		t.Fatalf("second policy suspension updated=%#v err=%v", updated, err)
	}
	updated, err = store.SetGroupJobsSuspended(context.Background(), "group-a", false, "disabled", now.Add(3*time.Second))
	if err != nil || len(updated) != 2 {
		t.Fatalf("first policy resume updated=%d err=%v", len(updated), err)
	}
	secondary, err := store.GetJob(context.Background(), "secondary")
	if err != nil || secondary.Status != domain.JobStatusSuspended || len(secondary.SuspendedPolicyGroupIDs) != 1 || secondary.SuspendedPolicyGroupIDs[0] != "group-b" {
		t.Fatalf("secondary job ignored remaining disabled policy: %#v err=%v", secondary, err)
	}
	updated, err = store.SetGroupJobsSuspended(context.Background(), "group-b", false, "disabled", now.Add(4*time.Second))
	if err != nil || len(updated) != 1 || updated[0].Status != domain.JobStatusQueued {
		t.Fatalf("final policy resume updated=%#v err=%v", updated, err)
	}
	other, err := store.GetJob(context.Background(), "other")
	if err != nil || other.Status != domain.JobStatusQueued {
		t.Fatalf("unrelated job changed: %#v err=%v", other, err)
	}
}

func TestCancelUnassignedSuspendedJobCompletesWithoutDispatch(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.CreateJob(context.Background(), domain.Job{ID: "suspended", Status: domain.JobStatusSuspended, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, dispatch, err := store.RequestJobCancellation(context.Background(), "suspended", "user request", 0, now.Add(time.Second))
	if err != nil || dispatch || job.Status != domain.JobStatusCanceled || job.FinishedAt == nil {
		t.Fatalf("cancel result=%#v dispatch=%v err=%v", job, dispatch, err)
	}
}

func TestOwnerScopedListingSurvivesABusyCluster(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Now().UTC()
	// One quiet owner whose work is old, buried under a large volume of newer
	// jobs belonging to somebody else.
	for index := 0; index < 3; index++ {
		job := domain.Job{
			ID: "quiet-" + strconv.Itoa(index), OwnerID: "user-quiet", Status: domain.JobStatusRunning,
			CreatedAt: base, UpdatedAt: base,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 200; index++ {
		job := domain.Job{
			ID: "busy-" + strconv.Itoa(index), OwnerID: "user-busy", Status: domain.JobStatusRunning,
			CreatedAt: base.Add(time.Duration(index+1) * time.Minute), UpdatedAt: base,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	// The cluster-wide page is entirely the busy owner's newest work, which is
	// exactly why filtering after the limit loses the quiet owner's jobs.
	page, err := store.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range page {
		if job.OwnerID == "user-quiet" {
			t.Fatalf("test premise broken: quiet owner appeared in the cluster page as %s", job.ID)
		}
	}

	owned, err := store.ListJobsForOwner(ctx, "user-quiet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 3 {
		t.Fatalf("owner page lost jobs to a busy cluster: got %d, want 3", len(owned))
	}
	for _, job := range owned {
		if job.OwnerID != "user-quiet" {
			t.Fatalf("owner page leaked %s belonging to %s", job.ID, job.OwnerID)
		}
	}
}

func TestActiveOwnerListingIsUnboundedAndExcludesTerminalJobs(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Now().UTC()
	const activeCount = 30
	for index := 0; index < activeCount; index++ {
		job := domain.Job{
			ID: "active-" + strconv.Itoa(index), OwnerID: "user-a", Status: domain.JobStatusQueued,
			CreatedAt: base, UpdatedAt: base,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 5; index++ {
		job := domain.Job{
			ID: "done-" + strconv.Itoa(index), OwnerID: "user-a", Status: domain.JobStatusSucceeded,
			CreatedAt: base, UpdatedAt: base,
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateJob(ctx, domain.Job{
		ID: "other-owner", OwnerID: "user-b", Status: domain.JobStatusQueued, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}

	// A bounded listing stops at its limit; quota accounting must not, or it
	// under-counts usage and lets the quota be exceeded.
	bounded, err := store.ListJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 10 {
		t.Fatalf("test premise broken: bounded listing returned %d, want 10", len(bounded))
	}
	active, err := store.ListActiveJobsForOwner(ctx, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != activeCount {
		t.Fatalf("active job accounting was truncated: got %d, want %d", len(active), activeCount)
	}
	for _, job := range active {
		if job.OwnerID != "user-a" || job.Status.Terminal() {
			t.Fatalf("unexpected job in active accounting: %s owner=%s status=%s", job.ID, job.OwnerID, job.Status)
		}
	}
}

func TestOwnerAndActiveIndexesTrackJobLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xirang.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, job := range []domain.Job{
		{ID: "a-queued", OwnerID: "user-a", Status: domain.JobStatusQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "a-running", OwnerID: "user-a", Status: domain.JobStatusRunning, CreatedAt: now, UpdatedAt: now},
		{ID: "b-queued", OwnerID: "user-b", Status: domain.JobStatusQueued, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	assertIDs := func(label string, jobs []domain.Job, want ...string) {
		t.Helper()
		got := make([]string, 0, len(jobs))
		for _, job := range jobs {
			got = append(got, job.ID)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}

	owned, err := store.ListJobsForOwner(ctx, "user-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("owner listing", owned, "a-queued", "a-running")

	active, err := store.ListActiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("active listing", active, "a-queued", "a-running", "b-queued")

	// Reaching a terminal state must drop the job from the active index while
	// leaving it in the owner's history.
	finished, err := store.FailQueuedJob(ctx, "a-queued", "operator cancelled", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !finished.Status.Terminal() {
		t.Fatalf("test premise broken: job did not reach a terminal state, got %s", finished.Status)
	}
	active, err = store.ListActiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("active listing after completion", active, "a-running", "b-queued")

	ownerActive, err := store.ListActiveJobsForOwner(ctx, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("owner active listing", ownerActive, "a-running")

	owned, err = store.ListJobsForOwner(ctx, "user-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("owner history retains the finished job", owned, "a-queued", "a-running")
}

func TestJobIndexesAreBackfilledForDatabasesWrittenBeforeThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xirang.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, job := range []domain.Job{
		{ID: "legacy-active", OwnerID: "user-a", Status: domain.JobStatusRunning, CreatedAt: now, UpdatedAt: now},
		{ID: "legacy-done", OwnerID: "user-a", Status: domain.JobStatusSucceeded, CreatedAt: now, UpdatedAt: now, FinishedAt: &now},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reproduce a database written before the indexes existed: the buckets are
	// absent and, crucially, so is the version marker that records which index
	// layout the file already carries.
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{"jobs_by_owner", "jobs_active", "jobs_finished"} {
			if err := tx.DeleteBucket([]byte(name)); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte("schema_metadata")).Delete([]byte("job_index_version"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	owned, err := reopened.ListJobsForOwner(ctx, "user-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 2 {
		t.Fatalf("owner index was not rebuilt on open: got %d jobs, want 2", len(owned))
	}
	active, err := reopened.ListActiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "legacy-active" {
		t.Fatalf("active index was not rebuilt on open: %+v", active)
	}
	finished, err := reopened.ListJobsFinishedSince(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(finished) != 1 || finished[0].ID != "legacy-done" {
		t.Fatalf("completion index was not rebuilt on open: %+v", finished)
	}
}

func TestCompletionIndexScansOnlyTheRequestedWindow(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-90 * 24 * time.Hour)

	for _, job := range []domain.Job{
		{ID: "ancient", OwnerID: "user-a", Status: domain.JobStatusSucceeded, CreatedAt: old, UpdatedAt: old, FinishedAt: &old},
		{ID: "recent", OwnerID: "user-a", Status: domain.JobStatusFailed, CreatedAt: now, UpdatedAt: now, FinishedAt: &now},
		{ID: "running", OwnerID: "user-a", Status: domain.JobStatusRunning, CreatedAt: now, UpdatedAt: now},
		// A terminal job with no completion time cannot be placed on a
		// time-ordered index, and never contributed to fair-share weighting
		// either, which requires the same field.
		{ID: "undated", OwnerID: "user-a", Status: domain.JobStatusCanceled, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	assertIDs := func(label string, jobs []domain.Job, want ...string) {
		t.Helper()
		got := make([]string, 0, len(jobs))
		for _, job := range jobs {
			got = append(got, job.ID)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}

	within, err := store.ListJobsFinishedSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("a short window excludes older completions", within, "recent")

	everything, err := store.ListJobsFinishedSince(ctx, old.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("a wide window includes both completions", everything, "ancient", "recent")

	future, err := store.ListJobsFinishedSince(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("a window past every completion is empty", future)

	// The completion index must not double as the liveness index.
	active, err := store.ListActiveJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertIDs("only the unfinished job is active", active, "running")
}
