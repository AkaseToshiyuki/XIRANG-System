package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/accounting"
	"github.com/xirang-grid/xirang/internal/alerting"
	buildmanager "github.com/xirang-grid/xirang/internal/build"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	"github.com/xirang-grid/xirang/internal/notification"
	"github.com/xirang-grid/xirang/internal/pki"
	"github.com/xirang-grid/xirang/internal/scheduler"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"github.com/xirang-grid/xirang/pkg/version"
)

func TestAPITokenRBACMiddleware(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	created, err := access.CreateToken(context.Background(), "read only", "viewer", time.Hour, domain.Principal{
		Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{AdminToken: "bootstrap", Access: access}).HTTPHandler()
	me := jsonRequest(t, handler, http.MethodGet, "/api/v1/access/me", nil, created.Secret)
	if me.Code != http.StatusOK {
		t.Fatalf("viewer could not read own principal: %d %s", me.Code, me.Body.String())
	}
	forbidden := jsonRequest(t, handler, http.MethodPost, "/api/v1/access/tokens", map[string]any{
		"name": "forbidden", "role": "viewer", "ttlSeconds": 3600,
	}, created.Secret)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("viewer managed access tokens: %d %s", forbidden.Code, forbidden.Body.String())
	}
}

func TestAccountingUsersSeeOnlyTheirSummaryAndAdminsManageRates(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	admin := domain.Principal{Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	user, err := access.CreateToken(context.Background(), "accounting user", "user", time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{AdminToken: "bootstrap", Access: access, Accounting: accounting.New(store)}).HTTPHandler()

	summary := jsonRequest(t, handler, http.MethodGet, "/api/v1/accounting/summary?from=2026-07-01&to=2026-07-29&ownerId=another-user", nil, user.Secret)
	if summary.Code != http.StatusOK {
		t.Fatalf("user could not read own accounting summary: %d %s", summary.Code, summary.Body.String())
	}
	var own domain.UsageSummary
	if err := json.Unmarshal(summary.Body.Bytes(), &own); err != nil {
		t.Fatal(err)
	}
	if own.OwnerID != user.Token.Subject {
		t.Fatalf("user selected another accounting owner: got %q want %q", own.OwnerID, user.Token.Subject)
	}
	allUsers := jsonRequest(t, handler, http.MethodGet, "/api/v1/accounting/users?from=2026-07-01&to=2026-07-29", nil, user.Secret)
	if allUsers.Code != http.StatusForbidden {
		t.Fatalf("non-admin listed all accounting users: %d %s", allUsers.Code, allUsers.Body.String())
	}
	rate := jsonRequest(t, handler, http.MethodPost, "/api/v1/accounting/rates", map[string]any{
		"currency": "CNY", "cpuCoreHourMicros": 1000000, "memoryGiBHourMicros": 0,
		"gpuDeviceHourMicros": 0, "storageGiBDayMicros": 0,
	}, "bootstrap")
	if rate.Code != http.StatusCreated {
		t.Fatalf("administrator could not publish a billing rate: %d %s", rate.Code, rate.Body.String())
	}
}

func TestBuildSigningKeyRotationRequiresAccessAdministration(t *testing.T) {
	if permission := requiredPermission(http.MethodPost, "/api/v1/build-signing-key/rotate"); permission != identity.PermissionAccessManage {
		t.Fatalf("signing-key rotation permission = %q", permission)
	}
	if permission := requiredPermission(http.MethodGet, "/api/v1/build-signing-key"); permission != identity.PermissionRead {
		t.Fatalf("signing-key read permission = %q", permission)
	}
}

func TestSharedFileMutationsRequireExplicitWritePermission(t *testing.T) {
	if permission := requiredPermission(http.MethodPut, "/api/v1/shared-spaces/share/files/content"); permission != identity.PermissionFilesWrite {
		t.Fatalf("shared file upload requires %q, got %q", identity.PermissionFilesWrite, permission)
	}
	if permission := requiredPermission(http.MethodGet, "/api/v1/shared-spaces/share/files/content"); permission != identity.PermissionRead {
		t.Fatalf("shared file download requires %q, got %q", identity.PermissionRead, permission)
	}
	if permission := requiredPermission(http.MethodPut, "/api/v1/shared-spaces/share"); permission != identity.PermissionNodesManage {
		t.Fatalf("shared-space configuration requires %q, got %q", identity.PermissionNodesManage, permission)
	}
}

func TestEventStreamLimiterRejectsExcessPrincipalConnections(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handler := streamLimitMiddleware(2, 1, time.Minute, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))
	principal := domain.Principal{UserID: "user-a", Subject: "alice", Role: "user"}
	firstDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/events/alerts", nil)
		request = request.WithContext(identity.WithPrincipal(request.Context(), principal))
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(firstDone)
	}()
	<-entered
	request := httptest.NewRequest(http.MethodGet, "/api/v1/events/topology", nil)
	request = request.WithContext(identity.WithPrincipal(request.Context(), principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("second event stream status=%d body=%s", response.Code, response.Body.String())
	}
	close(release)
	<-firstDone
}

func TestEventStreamRouteMatchesEveryRegisteredStream(t *testing.T) {
	for _, path := range []string{
		"/api/v1/events/nodes", "/api/v1/events/topology", "/api/v1/events/alerts",
		"/api/v1/jobs/job-1/logs/stream",
	} {
		if !eventStreamRoute(http.MethodGet, path) {
			t.Errorf("registered event stream %q was not bounded", path)
		}
	}
	if eventStreamRoute(http.MethodPost, "/api/v1/events/nodes") || eventStreamRoute(http.MethodGet, "/api/v1/nodes") {
		t.Fatal("non-stream route was classified as an event stream")
	}
}

func TestPublicSharedSpaceHidesOwnershipIndex(t *testing.T) {
	space := domain.SharedSpace{
		ID: "share_test",
		FileOwners: map[string]domain.SharedFileOwner{
			"private.dat": {ID: "user-secret", Name: "Private User"},
		},
	}
	public := publicSharedSpace(space)
	if public.FileOwners != nil {
		t.Fatalf("ownership index leaked through shared-space response: %#v", public.FileOwners)
	}
	if len(space.FileOwners) != 1 {
		t.Fatal("sanitizing an API response must not mutate the persisted ownership index")
	}
}

func TestJobTemplateHTTPFlowAndRBAC(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	admin := domain.Principal{Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	viewer, err := access.CreateToken(context.Background(), "template viewer", "viewer", time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	user, err := access.CreateToken(context.Background(), "template user", "user", time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{
		AdminToken: "bootstrap", Access: access, JobTemplates: jobtemplate.New(store),
	}).HTTPHandler()

	script := `XIRANG 1
project = template-test
artifact = https://artifacts.example/smoke
sha256 = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
architecture = amd64
entrypoint = smoke
cpu = 1
memory = 128M
`
	for _, secret := range []string{viewer.Secret, user.Secret} {
		read := jsonRequest(t, handler, http.MethodGet, "/api/v1/job-templates", nil, secret)
		if read.Code != http.StatusOK {
			t.Fatalf("authenticated user could not list templates: %d %s", read.Code, read.Body.String())
		}
		write := jsonRequest(t, handler, http.MethodPost, "/api/v1/job-templates", map[string]any{
			"name": "forbidden", "description": "", "script": script,
		}, secret)
		if write.Code != http.StatusForbidden {
			t.Fatalf("non-admin created a template: %d %s", write.Code, write.Body.String())
		}
	}
	created := jsonRequest(t, handler, http.MethodPost, "/api/v1/job-templates", map[string]any{
		"name": "Smoke", "description": "Portable smoke job", "script": script,
	}, "bootstrap")
	if created.Code != http.StatusCreated {
		t.Fatalf("admin template create status=%d body=%s", created.Code, created.Body.String())
	}
	var template domain.JobTemplate
	if err := json.Unmarshal(created.Body.Bytes(), &template); err != nil {
		t.Fatal(err)
	}
	listed := jsonRequest(t, handler, http.MethodGet, "/api/v1/job-templates", nil, viewer.Secret)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), template.ID) {
		t.Fatalf("viewer did not receive published template: %d %s", listed.Code, listed.Body.String())
	}
	updated := jsonRequest(t, handler, http.MethodPut, "/api/v1/job-templates/"+template.ID, map[string]any{
		"name": "Smoke v2", "description": "Updated", "script": script, "revision": template.Revision,
	}, "bootstrap")
	if updated.Code != http.StatusOK {
		t.Fatalf("admin template update status=%d body=%s", updated.Code, updated.Body.String())
	}
	stale := jsonRequest(t, handler, http.MethodPut, "/api/v1/job-templates/"+template.ID, map[string]any{
		"name": "stale", "description": "", "script": script, "revision": template.Revision,
	}, "bootstrap")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale template update status=%d body=%s", stale.Code, stale.Body.String())
	}
	removed := jsonRequest(t, handler, http.MethodDelete, "/api/v1/job-templates/"+template.ID+"?revision=2", nil, "bootstrap")
	if removed.Code != http.StatusNoContent {
		t.Fatalf("admin template delete status=%d body=%s", removed.Code, removed.Body.String())
	}
}

func TestTypedNilBuildManagerIsNotRegistered(t *testing.T) {
	var manager *buildmanager.Service
	response := jsonRequest(t, New(Config{AdminToken: "admin-test", Builds: manager}).HTTPHandler(), http.MethodGet, "/api/v1/builds", nil, "admin-test")
	if response.Code != http.StatusNotFound {
		t.Fatalf("typed nil build manager must not register routes: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSensitiveReadPermissionsRemainAdministratorOnly(t *testing.T) {
	for _, path := range []string{
		"/api/v1/enrollment/join-token",
		"/api/v1/discovery/scans",
		"/api/v1/assignments",
	} {
		if permission := requiredPermission(http.MethodGet, path); permission == identity.PermissionRead {
			t.Fatalf("sensitive path %s was exposed to read-only users", path)
		}
	}
}

func TestHealth(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	New(Config{}).HTTPHandler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected a request ID")
	}
	if response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("expected security headers")
	}
	if policy := response.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "style-src-attr 'unsafe-inline'") || !strings.Contains(policy, "form-action 'self'") {
		t.Fatalf("incomplete content security policy: %q", policy)
	}
	if response.Header().Get("Permissions-Policy") == "" || response.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatal("expected browser isolation and permissions headers")
	}
}

func TestEnrollmentCAEndpointSupportsPinnedFirstContact(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{Enrollment: enrollment.NewService(store, authority)}).HTTPHandler()
	response := jsonRequest(t, handler, http.MethodGet, "/api/v1/enrollment/ca", nil, "")
	if response.Code != http.StatusOK {
		t.Fatalf("CA bootstrap status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Certificate string `json:"caCertificatePem"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(payload.Certificate))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("CA bootstrap omitted a PEM certificate")
	}
	digest := sha256.Sum256(block.Bytes)
	if payload.Fingerprint != hex.EncodeToString(digest[:]) {
		t.Fatalf("CA fingerprint does not bind the returned certificate: %q", payload.Fingerprint)
	}
}

func TestWebRootServesSPAWithoutMaskingUnknownAPI(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<main>XIRANG</main>"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(Config{WebRoot: root, AdminToken: "bootstrap"}).HTTPHandler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/jobs/job-1", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "XIRANG") {
		t.Fatalf("SPA fallback failed: %d %s", page.Code, page.Body.String())
	}
	// An authenticated caller still receives 404 rather than the SPA shell, so
	// an unknown API path is never masked by the console fallback route.
	api := jsonRequest(t, handler, http.MethodGet, "/api/v1/not-real", nil, "bootstrap")
	if api.Code != http.StatusNotFound {
		t.Fatalf("unknown API was masked by SPA: %d", api.Code)
	}
	// Without a credential the same path fails closed instead of revealing
	// whether the route exists.
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/not-real", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("unknown API was reachable without a credential: %d", anonymous.Code)
	}
}

func TestArtifactRootServesOnlyContainedRegularFiles(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "artifacts")
	if err := os.MkdirAll(filepath.Join(root, "amd64"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "amd64", "smoke"), []byte("native-binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret")
	if err := os.WriteFile(outside, []byte("not-public"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	handler := New(Config{ArtifactRoot: root, AdminToken: "artifact-test"}).HTTPHandler()
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/artifacts/amd64/smoke", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated artifact status=%d", unauthorized.Code)
	}
	served := httptest.NewRecorder()
	servedRequest := httptest.NewRequest(http.MethodGet, "/artifacts/amd64/smoke", nil)
	servedRequest.Header.Set("Authorization", "Bearer artifact-test")
	handler.ServeHTTP(served, servedRequest)
	if served.Code != http.StatusOK || served.Body.String() != "native-binary" ||
		served.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(served.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("artifact response status=%d headers=%v body=%q", served.Code, served.Header(), served.Body.String())
	}
	for _, path := range []string{"/artifacts/missing", "/artifacts/amd64", "/artifacts/escape"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer artifact-test")
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("unsafe artifact path %s returned %d", path, response.Code)
		}
	}
}

func TestJobHTTPFlow(t *testing.T) {
	manager := &testJobManager{job: domain.Job{ID: "job_test", ProjectID: "research", Queue: "default", Status: domain.JobStatusQueued}}
	handler := New(Config{Jobs: manager, AdminToken: "admin-test"}).HTTPHandler()

	unauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/jobs", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected job authentication, got %d", unauthorized.Code)
	}
	created := jsonRequest(t, handler, http.MethodPost, "/api/v1/jobs", map[string]any{
		"projectId": "research", "queue": "fast", "priority": 42, "maxAttempts": 2,
		"artifact": map[string]any{
			"uri": "https://artifacts.example/worker.tar.gz", "sha256": strings.Repeat("a", 64),
			"architecture": "arm64", "entrypoint": "bin/worker",
		},
		"resources":            map[string]any{"milliCpu": 2000, "memoryBytes": 2147483648, "ephemeralStorageBytes": 0},
		"parallel":             map[string]any{"nodes": 3, "tasksPerNode": 2, "launcher": "native"},
		"requiredArchitecture": "arm64", "requiredTopology": map[string]string{"topology.xirang.io/rack": "r1"},
		"excludedNodeIds": []string{"node-maintenance"}, "preferredZone": "east", "minimumNetworkBits": int64(25_000_000_000),
		"cpuSet": "0-3", "numaNodes": "0",
		"tolerations": []map[string]any{{"key": "accelerator", "value": "gpu", "effect": "NoSchedule"}}, "timeoutSeconds": 90,
	}, "admin-test")
	if created.Code != http.StatusCreated {
		t.Fatalf("create job status=%d body=%s", created.Code, created.Body.String())
	}
	if manager.request.Task.Placement.RequiredArchitecture != "arm64" || manager.request.Task.Timeout != 90*time.Second || manager.request.Priority != 42 ||
		manager.request.Task.Parallel.Nodes != 3 || manager.request.Task.Parallel.TasksPerNode != 2 || manager.request.Task.Parallel.Launcher != "native" {
		t.Fatalf("unexpected submission: %#v", manager.request)
	}
	if manager.request.Task.Placement.RequiredTopology["topology.xirang.io/rack"] != "r1" || manager.request.Task.Placement.PreferredZone != "east" ||
		manager.request.Task.Placement.MinimumNetworkBits != 25_000_000_000 || len(manager.request.Task.Placement.Tolerations) != 1 || len(manager.request.Task.Placement.ExcludedNodeIDs) != 1 {
		t.Fatalf("phase 4 placement fields were not mapped: %#v", manager.request.Task.Placement)
	}
	if manager.request.Task.Placement.CPUSet != "0-3" || manager.request.Task.Placement.NUMANodes != "0" {
		t.Fatalf("affinity fields were not mapped: %#v", manager.request.Task.Placement)
	}
	scripted := jsonRequest(t, handler, http.MethodPost, "/api/v1/jobs/script", map[string]any{"script": `XIRANG 1
project = scripted
queue = science
artifact = https://artifacts.example/worker
sha256 = aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
architecture = amd64
entrypoint = worker
cpu = 2
memory = 4G
nodes = 2
tasks_per_node = 4
launcher = native
env.OMP_NUM_THREADS = 2
arg = /shared/input.dat
`}, "admin-test")
	if scripted.Code != http.StatusCreated || manager.request.ProjectID != "scripted" || manager.request.Task.Resources.MilliCPU != 2000 ||
		manager.request.Task.Parallel.WorldSize() != 8 || manager.request.Task.Parallel.Launcher != "native" ||
		manager.request.Task.Environment["OMP_NUM_THREADS"] != "2" || len(manager.request.Task.Arguments) != 1 {
		t.Fatalf("script submission was not mapped: status=%d request=%#v body=%s", scripted.Code, manager.request, scripted.Body.String())
	}
	invalidScript := jsonRequest(t, handler, http.MethodPost, "/api/v1/jobs/script", map[string]any{"script": "XIRANG 1\ncpu = 1"}, "admin-test")
	if invalidScript.Code != http.StatusBadRequest {
		t.Fatalf("invalid script status=%d body=%s", invalidScript.Code, invalidScript.Body.String())
	}
	listed := jsonRequest(t, handler, http.MethodGet, "/api/v1/jobs?limit=25", nil, "admin-test")
	if listed.Code != http.StatusOK || manager.limit != 25 {
		t.Fatalf("list jobs status=%d limit=%d body=%s", listed.Code, manager.limit, listed.Body.String())
	}
	invalid := jsonRequest(t, handler, http.MethodGet, "/api/v1/jobs?limit=0", nil, "admin-test")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	canceled := jsonRequest(t, handler, http.MethodPost, "/api/v1/jobs/job_test/cancel", map[string]any{
		"reason": "maintenance", "gracePeriodSeconds": 12,
	}, "admin-test")
	if canceled.Code != http.StatusAccepted || manager.cancelID != "job_test" || manager.cancelRequest.GracePeriod != 12*time.Second {
		t.Fatalf("cancel job status=%d request=%#v body=%s", canceled.Code, manager.cancelRequest, canceled.Body.String())
	}
}

func TestEffectiveJobTimeoutUsesGroupPolicy(t *testing.T) {
	tests := []struct {
		name          string
		requested     int64
		maximum       int64
		administrator bool
		want          int64
		wantError     bool
	}{
		{name: "default group omitted", want: domain.DefaultMaxJobTimeoutSeconds},
		{name: "custom group omitted", maximum: 30 * 86400, want: 30 * 86400},
		{name: "custom group declaration", requested: 14 * 86400, maximum: 30 * 86400, want: 14 * 86400},
		{name: "group maximum exceeded", requested: 31 * 86400, maximum: 30 * 86400, wantError: true},
		{name: "administrator omitted is unlimited", maximum: 7 * 86400, administrator: true, want: 0},
		{name: "administrator long job", requested: 365 * 86400, administrator: true, want: 365 * 86400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := effectiveJobTimeoutSeconds(test.requested, test.maximum, test.administrator)
			if (err != nil) != test.wantError || got != test.want {
				t.Fatalf("timeout = %d, err=%v; want %d, error=%v", got, err, test.want, test.wantError)
			}
		})
	}
}

func TestUnsatisfiableJobErrorIsStructuredAndUnprocessable(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeError(recorder, &scheduler.UnsatisfiableError{Reason: domain.JobBlockReason{
		Code: "per_node_capacity_impossible", Category: "resource", Message: "request exceeds every node",
		Details: map[string]string{"largestNodeMilliCPU": "8000"},
	}})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error  string                `json:"error"`
		Reason domain.JobBlockReason `json:"reason"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil || payload.Reason.Code != "per_node_capacity_impossible" || !strings.Contains(payload.Error, "cannot be satisfied") {
		t.Fatalf("unexpected error response: %#v err=%v", payload, err)
	}
}

type testJobManager struct {
	job           domain.Job
	request       scheduler.SubmitRequest
	limit         int
	ownerFilter   string
	cancelID      string
	cancelRequest scheduler.CancelRequest
	quotaUserID   string
	quotaBytes    int64
	disabledGroup string
	disabled      bool
}

func (manager *testJobManager) Submit(_ context.Context, request scheduler.SubmitRequest) (domain.Job, error) {
	manager.request = request
	return manager.job, nil
}

func (manager *testJobManager) Get(_ context.Context, _ string) (domain.Job, error) {
	return manager.job, nil
}

func (manager *testJobManager) List(_ context.Context, limit int) ([]domain.Job, error) {
	manager.limit = limit
	return []domain.Job{manager.job}, nil
}

func (manager *testJobManager) ListForOwner(_ context.Context, ownerID string, limit int) ([]domain.Job, error) {
	manager.limit, manager.ownerFilter = limit, ownerID
	if manager.job.OwnerID != ownerID {
		return []domain.Job{}, nil
	}
	return []domain.Job{manager.job}, nil
}

func (manager *testJobManager) ListActiveForOwner(_ context.Context, ownerID string) ([]domain.Job, error) {
	manager.ownerFilter = ownerID
	if manager.job.OwnerID != ownerID || manager.job.Status.Terminal() {
		return []domain.Job{}, nil
	}
	return []domain.Job{manager.job}, nil
}

func (manager *testJobManager) Cancel(_ context.Context, id string, request scheduler.CancelRequest) (domain.Job, error) {
	manager.cancelID, manager.cancelRequest = id, request
	return manager.job, nil
}

func (manager *testJobManager) UpdateUserStorageQuota(_ context.Context, userID string, quotaBytes int64) error {
	manager.quotaUserID, manager.quotaBytes = userID, quotaBytes
	return nil
}

func (manager *testJobManager) SetGroupDisabled(_ context.Context, groupID string, disabled bool) error {
	manager.disabledGroup, manager.disabled = groupID, disabled
	return nil
}

func TestDeleteAssignedUserGroupDetachesMembersAndReconcilesPolicies(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	admin, err := access.SetupAdmin(context.Background(), identity.LocalCredentials{Username: "admin", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{Subject: "bootstrap-admin", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	group, err := access.CreateUserGroup(context.Background(), domain.UserGroup{
		Name: "UserLabs", StorageQuotaBytes: 100 << 30, Disabled: true, DisableSuspendQueued: true,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.PutUserGroups(context.Background(), admin.ID, []string{group.ID}, actor); err != nil {
		t.Fatal(err)
	}
	jobs := &testJobManager{}
	handler := New(Config{AdminToken: "bootstrap", Access: access, Jobs: jobs}).HTTPHandler()
	response := jsonRequest(t, handler, http.MethodDelete, "/api/v1/access/groups/"+group.ID, nil, "bootstrap")
	if response.Code != http.StatusNoContent || response.Header().Get("X-Xirang-Detached-Users") != "1" {
		t.Fatalf("delete status=%d detached=%q body=%s", response.Code, response.Header().Get("X-Xirang-Detached-Users"), response.Body.String())
	}
	updated, _, err := store.GetUser(context.Background(), admin.ID)
	if err != nil || len(updated.GroupIDs) != 0 {
		t.Fatalf("updated groups=%v err=%v", updated.GroupIDs, err)
	}
	if jobs.quotaUserID != admin.ID || jobs.quotaBytes != 0 {
		t.Fatalf("quota reconciliation user=%q bytes=%d", jobs.quotaUserID, jobs.quotaBytes)
	}
	if jobs.disabledGroup != group.ID || jobs.disabled {
		t.Fatalf("disabled-group reconciliation group=%q disabled=%t", jobs.disabledGroup, jobs.disabled)
	}
}

func TestAccessSettingsHTTPConfiguresDefaultUserGroup(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	access := identity.NewAccessService(store, nil, nil)
	if _, err := access.SetupAdmin(context.Background(), identity.LocalCredentials{Username: "admin", Password: "correct-horse-battery-staple"}); err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{Subject: "bootstrap-admin", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	group, err := access.CreateUserGroup(context.Background(), domain.UserGroup{Name: "new-users"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{AdminToken: "bootstrap", Access: access}).HTTPHandler()
	response := jsonRequest(t, handler, http.MethodPut, "/api/v1/access/settings", map[string]string{"defaultUserGroupId": group.ID}, "bootstrap")
	if response.Code != http.StatusOK {
		t.Fatalf("put settings status=%d body=%s", response.Code, response.Body.String())
	}
	registered := jsonRequest(t, handler, http.MethodPost, "/api/v1/auth/register", map[string]string{
		"username": "new-user", "password": "another-correct-horse-battery-staple",
	}, "")
	if registered.Code != http.StatusAccepted {
		t.Fatalf("register status=%d body=%s", registered.Code, registered.Body.String())
	}
	var user domain.User
	if err := json.Unmarshal(registered.Body.Bytes(), &user); err != nil || len(user.GroupIDs) != 1 || user.GroupIDs[0] != group.ID {
		t.Fatalf("registered user=%#v err=%v", user, err)
	}
}

func TestEnrollmentHTTPFlow(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	commands := &testCommandQueue{}
	enrollmentService := enrollment.NewService(store, authority)
	handler := New(Config{Enrollment: enrollmentService, AdminToken: "admin-test", Commands: commands}).HTTPHandler()

	unauthorized := jsonRequest(t, handler, http.MethodPost, "/api/v1/enrollment/tokens", map[string]any{
		"name": "test", "ttlSeconds": 900, "maxUses": 1, "requireApproval": true,
	}, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized status, got %d", unauthorized.Code)
	}
	assignmentUnauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/assignments", nil, "")
	if assignmentUnauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected assignment list authentication, got %d", assignmentUnauthorized.Code)
	}
	assignmentList := jsonRequest(t, handler, http.MethodGet, "/api/v1/assignments", nil, "admin-test")
	if assignmentList.Code != http.StatusOK {
		t.Fatalf("assignment list status=%d body=%s", assignmentList.Code, assignmentList.Body.String())
	}

	createdResponse := jsonRequest(t, handler, http.MethodPost, "/api/v1/enrollment/tokens", map[string]any{
		"name": "test", "ttlSeconds": 900, "maxUses": 1, "requireApproval": true,
	}, "admin-test")
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create token status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created enrollment.CreatedToken
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	registerResponse := jsonRequest(t, handler, http.MethodPost, "/api/v1/enrollment/register", map[string]any{
		"token":  created.Secret,
		"csrPem": serverTestCSR(t),
		"inventory": domain.InventorySummary{
			Hostname: "node-a", OS: "linux", Architecture: "amd64", CPUCount: 8, MemoryBytes: 16 << 30,
			Devices: map[string]int64{"xirang.io/drm-render": 1},
		},
	}, "")
	if registerResponse.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", registerResponse.Code, registerResponse.Body.String())
	}
	var registered enrollment.RegistrationResult
	if err := json.NewDecoder(registerResponse.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	if registered.Node.Capacity.Devices["xirang.io/drm-render"] != 1 {
		t.Fatalf("registration device capacity was lost: %#v", registered.Node.Capacity.Devices)
	}
	deviceUpdated, err := enrollmentService.ReportDeviceCapacity(context.Background(), registered.Node.ID, map[string]int64{"xirang.io/drm-render": 2})
	if err != nil || deviceUpdated.Allocatable.Devices["xirang.io/drm-render"] != 2 {
		t.Fatalf("device capacity update failed: node=%#v err=%v", deviceUpdated, err)
	}
	if _, err := store.UpdateNode(context.Background(), registered.Node.ID, func(node *domain.Node) error {
		node.Allocated.Devices = map[string]int64{"xirang.io/drm-render": 1}
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	deviceFloor, err := enrollmentService.ReportDeviceCapacity(context.Background(), registered.Node.ID, nil)
	if err != nil || deviceFloor.Capacity.Devices["xirang.io/drm-render"] != 1 || deviceFloor.Allocatable.Devices["xirang.io/drm-render"] != 1 {
		t.Fatalf("allocated device floor was not preserved: node=%#v err=%v", deviceFloor, err)
	}

	approved := jsonRequest(t, handler, http.MethodPost, "/api/v1/nodes/"+registered.Node.ID+"/approve", nil, "admin-test")
	if approved.Code != http.StatusOK {
		t.Fatalf("approve status=%d body=%s", approved.Code, approved.Body.String())
	}
	configured := jsonRequest(t, handler, http.MethodPut, "/api/v1/nodes/"+registered.Node.ID+"/configuration", map[string]any{
		"roles": []string{"management", "compute"}, "managementAddress": "192.0.2.10", "sshUser": "xirang", "group": "lab", "computePriority": 0,
	}, "admin-test")
	if configured.Code != http.StatusAccepted || commands.message == nil || commands.message.GetSetManagementPanel() == nil || !commands.message.GetSetManagementPanel().Enabled {
		t.Fatalf("management configuration status=%d body=%s command=%#v", configured.Code, configured.Body.String(), commands.message)
	}
	if _, err := enrollmentService.ConfirmManagementPanel(context.Background(), registered.Node.ID, commands.message.MessageId, true, ""); err != nil {
		t.Fatal(err)
	}
	managed, err := enrollmentService.GetNode(context.Background(), registered.Node.ID)
	if err != nil || !managed.HasRole(domain.NodeRoleManagement) || !managed.ManagementPanelEnabled || managed.PendingManagementCommandID != "" {
		t.Fatalf("management panel acknowledgement was not persisted: node=%#v err=%v", managed, err)
	}
	tainted := jsonRequest(t, handler, http.MethodPut, "/api/v1/nodes/"+registered.Node.ID+"/taints", map[string]any{
		"taints": []map[string]any{{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}},
	}, "admin-test")
	if tainted.Code != http.StatusOK {
		t.Fatalf("taints status=%d body=%s", tainted.Code, tainted.Body.String())
	}
	var taintedNode domain.Node
	if err := json.NewDecoder(tainted.Body).Decode(&taintedNode); err != nil || len(taintedNode.Taints) != 1 {
		t.Fatalf("taints were not persisted: node=%#v err=%v", taintedNode, err)
	}
	rotation := jsonRequest(t, handler, http.MethodPost, "/api/v1/nodes/"+registered.Node.ID+"/certificate/rotate-command", nil, "admin-test")
	if rotation.Code != http.StatusAccepted || commands.message == nil || commands.message.GetRotateCertificate() == nil {
		t.Fatalf("rotation status=%d body=%s command=%#v", rotation.Code, rotation.Body.String(), commands.message)
	}
	if _, err := enrollmentService.ConfirmCertificateRotation(context.Background(), registered.Node.ID, commands.message.MessageId, false, "test cleanup"); err != nil {
		t.Fatal(err)
	}
	mode := jsonRequest(t, handler, http.MethodPost, "/api/v1/nodes/"+registered.Node.ID+"/mode", map[string]any{
		"mode": "cordoned", "reason": "maintenance",
	}, "admin-test")
	if mode.Code != http.StatusAccepted || commands.message == nil || commands.message.GetSetNodeMode() == nil {
		t.Fatalf("mode status=%d body=%s command=%#v", mode.Code, mode.Body.String(), commands.message)
	}
	block, _ := pem.Decode([]byte(registered.CertificatePEM))
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	heartbeatRequest := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+registered.Node.ID+"/heartbeat", nil)
	heartbeatRequest.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certificate},
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
	}
	heartbeat := httptest.NewRecorder()
	handler.ServeHTTP(heartbeat, heartbeatRequest)
	if heartbeat.Code != http.StatusOK {
		t.Fatalf("heartbeat status=%d body=%s", heartbeat.Code, heartbeat.Body.String())
	}

	unauthorizedRemoval := jsonRequest(t, handler, http.MethodDelete, "/api/v1/nodes/"+registered.Node.ID, nil, "")
	if unauthorizedRemoval.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized node removal status=%d body=%s", unauthorizedRemoval.Code, unauthorizedRemoval.Body.String())
	}
	activeRemoval := jsonRequest(t, handler, http.MethodDelete, "/api/v1/nodes/"+registered.Node.ID, nil, "admin-test")
	if activeRemoval.Code != http.StatusConflict {
		t.Fatalf("active node removal status=%d body=%s", activeRemoval.Code, activeRemoval.Body.String())
	}
	revoked := jsonRequest(t, handler, http.MethodPost, "/api/v1/nodes/"+registered.Node.ID+"/certificate/revoke", nil, "admin-test")
	if revoked.Code != http.StatusOK {
		t.Fatalf("certificate revocation before removal status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	removedResponse := jsonRequest(t, handler, http.MethodDelete, "/api/v1/nodes/"+registered.Node.ID, nil, "admin-test")
	if removedResponse.Code != http.StatusOK {
		t.Fatalf("node removal status=%d body=%s", removedResponse.Code, removedResponse.Body.String())
	}
	var removed domain.Node
	if err := json.NewDecoder(removedResponse.Body).Decode(&removed); err != nil {
		t.Fatal(err)
	}
	if removed.Status != domain.NodeStatusRemoved || removed.RemovedAt == nil || removed.CertificateRevokedAt == nil {
		t.Fatalf("node removal did not retain a revoked tombstone: %#v", removed)
	}
	listedResponse := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes", nil, "admin-test")
	var listed struct {
		Items []domain.Node `json:"items"`
	}
	if listedResponse.Code != http.StatusOK {
		t.Fatalf("node list after removal status=%d body=%s", listedResponse.Code, listedResponse.Body.String())
	}
	if err := json.NewDecoder(listedResponse.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 0 {
		t.Fatalf("removed node remained in operational list: %#v", listed.Items)
	}
	historicalResponse := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/"+registered.Node.ID, nil, "admin-test")
	if historicalResponse.Code != http.StatusOK {
		t.Fatalf("removed node tombstone was unavailable: %d %s", historicalResponse.Code, historicalResponse.Body.String())
	}
	repeatedRemoval := jsonRequest(t, handler, http.MethodDelete, "/api/v1/nodes/"+registered.Node.ID, nil, "admin-test")
	if repeatedRemoval.Code != http.StatusConflict {
		t.Fatalf("repeated node removal status=%d body=%s", repeatedRemoval.Code, repeatedRemoval.Body.String())
	}
}

type testCommandQueue struct {
	nodeID  string
	message *xirangv1.ControlMessage
}

func (queue *testCommandQueue) Enqueue(nodeID string, message *xirangv1.ControlMessage) error {
	queue.nodeID, queue.message = nodeID, message
	return nil
}

func serverTestCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func jsonRequest(t *testing.T, handler http.Handler, method, path string, body any, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &encoded)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestVersion(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	response := httptest.NewRecorder()
	New(Config{}).HTTPHandler().ServeHTTP(response, request)

	var info version.Info
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	if info.Name != version.Name || info.ProtocolVersion != version.ProtocolVersion {
		t.Fatalf("unexpected version info: %#v", info)
	}
}

func TestTelemetryRoutesRequireAdminAndReturnSnapshots(t *testing.T) {
	reader := &testTelemetryReader{
		inventory:    domain.NodeInventorySnapshot{NodeID: "node-1", Hostname: "node-a", Architecture: "arm64", ObservedAt: time.Now().UTC()},
		observations: []domain.NodeResourceObservation{{MessageID: "message-1", NodeID: "node-1", UsedMilliCPU: 500}},
	}
	handler := New(Config{AdminToken: "admin-test", Telemetry: reader}).HTTPHandler()
	unauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/node-1/inventory", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected authentication, got %d", unauthorized.Code)
	}
	inventory := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/node-1/inventory", nil, "admin-test")
	if inventory.Code != http.StatusOK || !bytes.Contains(inventory.Body.Bytes(), []byte(`"architecture":"arm64"`)) {
		t.Fatalf("inventory status=%d body=%s", inventory.Code, inventory.Body.String())
	}
	observations := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/node-1/observations?limit=1", nil, "admin-test")
	if observations.Code != http.StatusOK || reader.limit != 1 || !bytes.Contains(observations.Body.Bytes(), []byte(`"usedMilliCpu":500`)) {
		t.Fatalf("observations status=%d limit=%d body=%s", observations.Code, reader.limit, observations.Body.String())
	}
	invalidLimit := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/node-1/observations?limit=1001", nil, "admin-test")
	if invalidLimit.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid limit status, got %d", invalidLimit.Code)
	}
}

func TestTopologyRouteSupportsLatestAndVersion(t *testing.T) {
	reader := &testTopologyReader{snapshot: domain.TopologySnapshot{Version: 3, ReportCount: 2}}
	handler := New(Config{AdminToken: "admin-test", Topology: reader}).HTTPHandler()
	unauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/topology", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected authentication, got %d", unauthorized.Code)
	}
	latest := jsonRequest(t, handler, http.MethodGet, "/api/v1/topology", nil, "admin-test")
	if latest.Code != http.StatusOK || !bytes.Contains(latest.Body.Bytes(), []byte(`"version":3`)) {
		t.Fatalf("latest topology status=%d body=%s", latest.Code, latest.Body.String())
	}
	versioned := jsonRequest(t, handler, http.MethodGet, "/api/v1/topology?version=2", nil, "admin-test")
	if versioned.Code != http.StatusOK || reader.requestedVersion != 2 {
		t.Fatalf("versioned topology status=%d requested=%d", versioned.Code, reader.requestedVersion)
	}
	invalid := jsonRequest(t, handler, http.MethodGet, "/api/v1/topology?version=invalid", nil, "admin-test")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid version status, got %d", invalid.Code)
	}
}

func TestFlowRoutesRequireAdminAndBoundHistoryLimit(t *testing.T) {
	reader := &testFlowReader{latest: []domain.NodeFlowObservation{{
		MessageID: "flow-1", NodeID: "node-1", Flows: []domain.NetworkFlow{{
			Family: "ipv4", Protocol: "tcp", SourceAddress: "10.0.0.1", DestinationAddress: "10.0.0.2",
			Direction: "egress", Packets: 2, Bytes: 128,
		}},
	}}}
	handler := New(Config{AdminToken: "admin-test", Flows: reader}).HTTPHandler()
	unauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/flows", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected authentication, got %d", unauthorized.Code)
	}
	latest := jsonRequest(t, handler, http.MethodGet, "/api/v1/flows", nil, "admin-test")
	if latest.Code != http.StatusOK || !bytes.Contains(latest.Body.Bytes(), []byte(`"messageId":"flow-1"`)) {
		t.Fatalf("latest flows status=%d body=%s", latest.Code, latest.Body.String())
	}
	history := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/node-1/flows?limit=7", nil, "admin-test")
	if history.Code != http.StatusOK || reader.nodeID != "node-1" || reader.limit != 7 {
		t.Fatalf("history status=%d node=%s limit=%d", history.Code, reader.nodeID, reader.limit)
	}
	invalid := jsonRequest(t, handler, http.MethodGet, "/api/v1/nodes/node-1/flows?limit=1001", nil, "admin-test")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid limit status, got %d", invalid.Code)
	}
}

func TestEnrichTopologySnapshotCalculatesInterfaceRate(t *testing.T) {
	base := time.Now().UTC()
	reader := &testTelemetryReader{observations: []domain.NodeResourceObservation{
		{NodeID: "node-1", ObservedAt: base.Add(2 * time.Second), InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": {ReceivedBytes: 1_300, TransmittedBytes: 2_700, PacketCountersSupported: true}}},
		{NodeID: "node-1", ObservedAt: base, InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": {ReceivedBytes: 1_000, TransmittedBytes: 2_000, PacketCountersSupported: true}}},
	}}
	stored := domain.TopologySnapshot{Edges: []domain.TopologyEdge{{
		ID: "edge-1", SourceID: "node-1", TargetID: "router-1", SourceInterface: "eth0",
	}}}
	snapshot := enrichTopologySnapshot(context.Background(), stored, reader)
	if snapshot.Edges[0].ObservedBits != 4_000 {
		t.Fatalf("unexpected edge rate: %d", snapshot.Edges[0].ObservedBits)
	}
	if snapshot.Edges[0].Health != "healthy" {
		t.Fatalf("unexpected edge health: %q", snapshot.Edges[0].Health)
	}
	if stored.Edges[0].ObservedBits != 0 || stored.Edges[0].Health != "" {
		t.Fatalf("stored snapshot was mutated: %#v", stored.Edges[0])
	}
}

func TestTopologyRateOverlayOnlyAppliesToLatestResponse(t *testing.T) {
	base := time.Now().UTC()
	topologyReader := &testTopologyReader{snapshot: domain.TopologySnapshot{Version: 4, Edges: []domain.TopologyEdge{{
		ID: "edge-1", SourceID: "node-1", TargetID: "router-1", SourceInterface: "eth0",
	}}}}
	telemetryReader := &testTelemetryReader{observations: []domain.NodeResourceObservation{
		{ObservedAt: base.Add(time.Second), InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": {ReceivedBytes: 200, PacketCountersSupported: true}}},
		{ObservedAt: base, InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": {ReceivedBytes: 100, PacketCountersSupported: true}}},
	}}
	handler := New(Config{AdminToken: "admin-test", Topology: topologyReader, Telemetry: telemetryReader}).HTTPHandler()

	latest := jsonRequest(t, handler, http.MethodGet, "/api/v1/topology", nil, "admin-test")
	if latest.Code != http.StatusOK || !bytes.Contains(latest.Body.Bytes(), []byte(`"observedBits":800`)) {
		t.Fatalf("latest topology status=%d body=%s", latest.Code, latest.Body.String())
	}
	historical := jsonRequest(t, handler, http.MethodGet, "/api/v1/topology?version=3", nil, "admin-test")
	if historical.Code != http.StatusOK || bytes.Contains(historical.Body.Bytes(), []byte(`"observedBits"`)) || bytes.Contains(historical.Body.Bytes(), []byte(`"health"`)) {
		t.Fatalf("historical topology was enriched: status=%d body=%s", historical.Code, historical.Body.String())
	}
}

func TestTopologyEventStreamSendsLatestEnrichedSnapshot(t *testing.T) {
	base := time.Now().UTC()
	topologyReader := &testTopologyReader{snapshot: domain.TopologySnapshot{Version: 7, Edges: []domain.TopologyEdge{{
		ID: "edge-1", SourceID: "node-1", TargetID: "router-1", SourceInterface: "eth0",
	}}}}
	telemetryReader := &testTelemetryReader{observations: []domain.NodeResourceObservation{
		{ObservedAt: base.Add(time.Second), InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": {ReceivedBytes: 200, PacketCountersSupported: true}}},
		{ObservedAt: base, InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": {ReceivedBytes: 100, PacketCountersSupported: true}}},
	}}
	httpServer := httptest.NewServer(New(Config{AdminToken: "admin-test", Topology: topologyReader, Telemetry: telemetryReader}).HTTPHandler())
	defer httpServer.Close()
	unauthorized, err := http.Get(httpServer.URL + "/api/v1/events/topology")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected authenticated topology stream, got %d", unauthorized.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/events/topology", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer admin-test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("unexpected stream response: status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < len("data: ") || line[:len("data: ")] != "data: " {
			continue
		}
		var snapshot domain.TopologySnapshot
		if err := json.Unmarshal([]byte(line[len("data: "):]), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Version != 7 || len(snapshot.Edges) != 1 || snapshot.Edges[0].ObservedBits != 800 || snapshot.Edges[0].Health != "healthy" {
			t.Fatalf("unexpected streamed snapshot: %#v", snapshot)
		}
		return
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("topology stream ended without an event")
}

func TestAlertRoutesListAcknowledgeAndStream(t *testing.T) {
	manager := &testAlertManager{
		alerts:  []domain.Alert{{ID: "alert-1", Status: domain.AlertStatusFiring, Severity: domain.AlertSeverityWarning}},
		updates: make(chan domain.Alert, 1),
	}
	handler := New(Config{AdminToken: "admin-test", Alerts: manager}).HTTPHandler()
	unauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/alerts", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected alert authentication, got %d", unauthorized.Code)
	}
	listed := jsonRequest(t, handler, http.MethodGet, "/api/v1/alerts?includeResolved=true", nil, "admin-test")
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(`"id":"alert-1"`)) || !manager.includeResolved {
		t.Fatalf("list alerts status=%d body=%s includeResolved=%t", listed.Code, listed.Body.String(), manager.includeResolved)
	}
	invalid := jsonRequest(t, handler, http.MethodGet, "/api/v1/alerts?includeResolved=maybe", nil, "admin-test")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid includeResolved status, got %d", invalid.Code)
	}
	acknowledged := jsonRequest(t, handler, http.MethodPost, "/api/v1/alerts/alert-1/acknowledge", nil, "admin-test")
	if acknowledged.Code != http.StatusOK || manager.acknowledgedID != "alert-1" || manager.actor != "bootstrap-admin" {
		t.Fatalf("acknowledge status=%d id=%q actor=%q", acknowledged.Code, manager.acknowledgedID, manager.actor)
	}
	manager.ackErr = alerting.ErrNotFound
	missing := jsonRequest(t, handler, http.MethodPost, "/api/v1/alerts/missing/acknowledge", nil, "admin-test")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected missing alert status, got %d", missing.Code)
	}
	manager.ackErr = alerting.ErrInvalidState
	resolved := jsonRequest(t, handler, http.MethodPost, "/api/v1/alerts/alert-1/acknowledge", nil, "admin-test")
	if resolved.Code != http.StatusConflict {
		t.Fatalf("expected invalid state status, got %d", resolved.Code)
	}
	manager.ackErr = nil

	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/events/alerts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer admin-test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	manager.updates <- domain.Alert{ID: "alert-stream", Status: domain.AlertStatusFiring, Severity: domain.AlertSeverityCritical}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < len("data: ") || line[:len("data: ")] != "data: " {
			continue
		}
		if !bytes.Contains([]byte(line), []byte(`"id":"alert-stream"`)) {
			t.Fatalf("unexpected alert event: %s", line)
		}
		return
	}
	t.Fatal("alert stream ended without an event")
}

func TestAlertRoutesEncodeEmptyItemsAsArray(t *testing.T) {
	manager := &testAlertManager{updates: make(chan domain.Alert)}
	response := jsonRequest(t, New(Config{AdminToken: "admin-test", Alerts: manager}).HTTPHandler(), http.MethodGet, "/api/v1/alerts", nil, "admin-test")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"items":[]`)) {
		t.Fatalf("empty alert list must be a JSON array: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAlertDeliveryRoutesRequireAdminAndSupportRetry(t *testing.T) {
	manager := &testNotificationManager{
		status:     domain.AlertWebhookStatus{Enabled: true, Destination: "alerts.example.test"},
		deliveries: []domain.AlertDelivery{{ID: "delivery-1", AlertID: "alert-1", State: domain.AlertDeliveryFailed}},
	}
	handler := New(Config{AdminToken: "admin-test", Notifications: manager}).HTTPHandler()
	unauthorized := jsonRequest(t, handler, http.MethodGet, "/api/v1/alert-deliveries", nil, "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected delivery authentication, got %d", unauthorized.Code)
	}
	listed := jsonRequest(t, handler, http.MethodGet, "/api/v1/alert-deliveries?limit=20", nil, "admin-test")
	if listed.Code != http.StatusOK || manager.limit != 20 || !bytes.Contains(listed.Body.Bytes(), []byte(`"destination":"alerts.example.test"`)) || !bytes.Contains(listed.Body.Bytes(), []byte(`"id":"delivery-1"`)) {
		t.Fatalf("list deliveries status=%d limit=%d body=%s", listed.Code, manager.limit, listed.Body.String())
	}
	invalid := jsonRequest(t, handler, http.MethodGet, "/api/v1/alert-deliveries?limit=1001", nil, "admin-test")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid delivery limit status, got %d", invalid.Code)
	}
	retried := jsonRequest(t, handler, http.MethodPost, "/api/v1/alert-deliveries/delivery-1/retry", nil, "admin-test")
	if retried.Code != http.StatusOK || manager.retryID != "delivery-1" {
		t.Fatalf("retry status=%d id=%q", retried.Code, manager.retryID)
	}
	manager.retryErr = notification.ErrNotFound
	missing := jsonRequest(t, handler, http.MethodPost, "/api/v1/alert-deliveries/missing/retry", nil, "admin-test")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("expected missing delivery status, got %d", missing.Code)
	}
	manager.retryErr = notification.ErrInvalidState
	conflict := jsonRequest(t, handler, http.MethodPost, "/api/v1/alert-deliveries/delivery-1/retry", nil, "admin-test")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("expected retry conflict status, got %d", conflict.Code)
	}
}

func TestAlertDeliveryRoutesEncodeEmptyItemsAsArray(t *testing.T) {
	manager := &testNotificationManager{}
	response := jsonRequest(t, New(Config{AdminToken: "admin-test", Notifications: manager}).HTTPHandler(), http.MethodGet, "/api/v1/alert-deliveries", nil, "admin-test")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"items":[]`)) {
		t.Fatalf("empty delivery list must be a JSON array: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEnrichTopologySnapshotRejectsUnsafeCounterDeltas(t *testing.T) {
	base := time.Now().UTC()
	tests := []struct {
		name     string
		current  domain.NetworkInterfaceCounters
		previous domain.NetworkInterfaceCounters
	}{
		{name: "counter regression", current: domain.NetworkInterfaceCounters{ReceivedBytes: 9}, previous: domain.NetworkInterfaceCounters{ReceivedBytes: 10}},
		{name: "delta addition overflow", current: domain.NetworkInterfaceCounters{ReceivedBytes: ^uint64(0), TransmittedBytes: ^uint64(0)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &testTelemetryReader{observations: []domain.NodeResourceObservation{
				{ObservedAt: base.Add(time.Second), InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": test.current}},
				{ObservedAt: base, InterfaceCounters: map[string]domain.NetworkInterfaceCounters{"eth0": test.previous}},
			}}
			snapshot := enrichTopologySnapshot(context.Background(), domain.TopologySnapshot{Edges: []domain.TopologyEdge{{SourceID: "node-1", SourceInterface: "eth0"}}}, reader)
			if snapshot.Edges[0].ObservedBits != 0 {
				t.Fatalf("unsafe counters produced rate %d", snapshot.Edges[0].ObservedBits)
			}
		})
	}
}

type testTelemetryReader struct {
	inventory    domain.NodeInventorySnapshot
	observations []domain.NodeResourceObservation
	limit        int
}

type testAlertManager struct {
	alerts          []domain.Alert
	updates         chan domain.Alert
	includeResolved bool
	acknowledgedID  string
	actor           string
	ackErr          error
}

type testNotificationManager struct {
	status     domain.AlertWebhookStatus
	deliveries []domain.AlertDelivery
	limit      int
	retryID    string
	retryErr   error
}

func (manager *testNotificationManager) Status() domain.AlertWebhookStatus { return manager.status }

func (manager *testNotificationManager) List(_ context.Context, limit int) ([]domain.AlertDelivery, error) {
	manager.limit = limit
	return append([]domain.AlertDelivery(nil), manager.deliveries...), nil
}

func (manager *testNotificationManager) Retry(_ context.Context, id string) (domain.AlertDelivery, error) {
	manager.retryID = id
	if manager.retryErr != nil {
		return domain.AlertDelivery{}, manager.retryErr
	}
	return domain.AlertDelivery{ID: id, State: domain.AlertDeliveryPending}, nil
}

func (manager *testAlertManager) List(_ context.Context, includeResolved bool) ([]domain.Alert, error) {
	manager.includeResolved = includeResolved
	return append([]domain.Alert(nil), manager.alerts...), nil
}

func (manager *testAlertManager) Acknowledge(_ context.Context, id, actor string) (domain.Alert, error) {
	manager.acknowledgedID = id
	manager.actor = actor
	if manager.ackErr != nil {
		return domain.Alert{}, manager.ackErr
	}
	return domain.Alert{ID: id, Status: domain.AlertStatusAcknowledged}, nil
}

func (manager *testAlertManager) Subscribe(context.Context) <-chan domain.Alert {
	return manager.updates
}

type testTopologyReader struct {
	snapshot         domain.TopologySnapshot
	requestedVersion uint64
}

type testFlowReader struct {
	latest []domain.NodeFlowObservation
	nodeID string
	limit  int
}

func (reader *testFlowReader) List(_ context.Context, nodeID string, limit int) ([]domain.NodeFlowObservation, error) {
	reader.nodeID, reader.limit = nodeID, limit
	return reader.latest, nil
}

func (reader *testFlowReader) ListLatest(context.Context) ([]domain.NodeFlowObservation, error) {
	return reader.latest, nil
}

func (reader *testTopologyReader) LatestSnapshot(context.Context) (domain.TopologySnapshot, error) {
	return reader.snapshot, nil
}

func (reader *testTopologyReader) GetSnapshot(_ context.Context, version uint64) (domain.TopologySnapshot, error) {
	reader.requestedVersion = version
	snapshot := reader.snapshot
	snapshot.Version = version
	return snapshot, nil
}

func (reader *testTelemetryReader) GetInventory(_ context.Context, _ string) (domain.NodeInventorySnapshot, error) {
	return reader.inventory, nil
}

func (reader *testTelemetryReader) ListObservations(_ context.Context, _ string, limit int) ([]domain.NodeResourceObservation, error) {
	reader.limit = limit
	return reader.observations, nil
}

type testTelemetryIntervalManager struct {
	interval time.Duration
}

func (manager *testTelemetryIntervalManager) TelemetryInterval() time.Duration {
	return manager.interval
}

func (manager *testTelemetryIntervalManager) UpdateTelemetryInterval(_ context.Context, interval time.Duration) error {
	manager.interval = interval
	return nil
}

func TestTelemetryIntervalAPIReadsAndUpdatesClusterSetting(t *testing.T) {
	manager := &testTelemetryIntervalManager{interval: 10 * time.Second}
	handler := New(Config{AdminToken: "admin-test", TelemetryIntervals: manager}).HTTPHandler()
	read := jsonRequest(t, handler, http.MethodGet, "/api/v1/system/telemetry-interval", nil, "admin-test")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"intervalSeconds":10`) {
		t.Fatalf("unexpected telemetry interval response: %d %s", read.Code, read.Body.String())
	}
	updated := jsonRequest(t, handler, http.MethodPut, "/api/v1/system/telemetry-interval", map[string]int{"intervalSeconds": 7}, "admin-test")
	if updated.Code != http.StatusOK || manager.interval != 7*time.Second {
		t.Fatalf("telemetry interval was not updated: status=%d interval=%s body=%s", updated.Code, manager.interval, updated.Body.String())
	}
	invalid := jsonRequest(t, handler, http.MethodPut, "/api/v1/system/telemetry-interval", map[string]int{"intervalSeconds": 1}, "admin-test")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range telemetry interval status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestCredentialEndpointsAreThrottledHarderThanTheManagementAPI(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := New(Config{Access: identity.NewAccessService(store, nil, nil), AdminToken: "bootstrap"}).HTTPHandler()

	login := func() int {
		body, err := json.Marshal(map[string]string{"username": "cluster-admin", "password": "not the password"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	for attempt := 0; attempt < 5; attempt++ {
		if code := login(); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was throttled before the burst was spent", attempt)
		}
	}
	if code := login(); code != http.StatusTooManyRequests {
		t.Fatalf("credential endpoint was not throttled after its burst: %d", code)
	}
	// The general management bucket is far larger, so an unrelated read from
	// the same peer is unaffected by the credential throttle.
	if code := jsonRequest(t, handler, http.MethodGet, "/api/v1/setup/status", nil, "").Code; code == http.StatusTooManyRequests {
		t.Fatal("the credential throttle leaked onto the management API")
	}
}
