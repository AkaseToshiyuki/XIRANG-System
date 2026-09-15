package bolt_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/pki"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestEnrollmentLifecyclePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xirang.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := pki.OpenOrCreate(filepath.Join(t.TempDir(), "pki"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := enrollment.NewService(store, authority)
	created, err := service.CreateToken(context.Background(), enrollment.TokenOptions{
		Name: "test node", TTL: 15 * time.Minute, MaxUses: 1, RequireApproval: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory := domain.InventorySummary{
		Hostname: "compute-a01", OS: "linux", Architecture: "amd64",
		Distribution: "Test Linux", CPUCount: 16, MemoryBytes: 64 << 30,
	}
	csr := testCSR(t)
	registered, err := service.Register(context.Background(), created.Secret, csr, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Node.Status != domain.NodeStatusPendingApproval {
		t.Fatalf("unexpected initial status: %s", registered.Node.Status)
	}
	if _, err := service.Register(context.Background(), created.Secret, csr, inventory); !errors.Is(err, enrollment.ErrInvalidToken) {
		t.Fatalf("expected one-use token rejection, got %v", err)
	}
	approved, err := service.Approve(context.Background(), registered.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != domain.NodeStatusReady || approved.ApprovedAt == nil {
		t.Fatalf("unexpected approved node: %#v", approved)
	}
	rotationRequested, err := service.RequestCertificateRotation(context.Background(), registered.Node.ID, "command-rotation", "rotation-test")
	if err != nil {
		t.Fatal(err)
	}
	if rotationRequested.PendingCertificateCommandID != "command-rotation" || rotationRequested.PendingCertificateRotationID != "rotation-test" {
		t.Fatalf("unexpected pending certificate command: %#v", rotationRequested)
	}
	rotationRejected, err := service.ConfirmCertificateRotation(context.Background(), registered.Node.ID, "command-rotation", false, "test cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if rotationRejected.PendingCertificateCommandID != "" || rotationRejected.PendingCertificateRotationID != "" {
		t.Fatalf("certificate command was not cleared: %#v", rotationRejected)
	}
	requestedMode, err := service.RequestNodeMode(context.Background(), registered.Node.ID, domain.NodeStatusCordoned, "command-cordon")
	if err != nil {
		t.Fatal(err)
	}
	if requestedMode.DesiredStatus != domain.NodeStatusCordoned || requestedMode.PendingModeCommandID != "command-cordon" || requestedMode.Schedulable() {
		t.Fatalf("unexpected pending node mode: %#v", requestedMode)
	}
	cordoned, err := service.ConfirmNodeMode(context.Background(), registered.Node.ID, "command-cordon", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if cordoned.Status != domain.NodeStatusCordoned || cordoned.DesiredStatus != "" || cordoned.PendingModeCommandID != "" {
		t.Fatalf("node mode was not confirmed: %#v", cordoned)
	}
	preservedMode, err := service.Heartbeat(context.Background(), registered.Node.ID, registered.Node.CertificateSerial)
	if err != nil || preservedMode.Status != domain.NodeStatusCordoned {
		t.Fatalf("HTTP certificate heartbeat changed node mode: node=%#v err=%v", preservedMode, err)
	}
	if _, err := service.RequestNodeMode(context.Background(), registered.Node.ID, domain.NodeStatusReady, "command-ready"); err != nil {
		t.Fatal(err)
	}
	ready, err := service.ConfirmNodeMode(context.Background(), registered.Node.ID, "command-ready", true, "")
	if err != nil || ready.Status != domain.NodeStatusReady {
		t.Fatalf("node did not return to ready: node=%#v err=%v", ready, err)
	}
	if _, err := service.Heartbeat(context.Background(), registered.Node.ID, "wrong"); !errors.Is(err, enrollment.ErrUnauthorized) {
		t.Fatalf("expected heartbeat authentication failure, got %v", err)
	}
	heartbeat, err := service.Heartbeat(context.Background(), registered.Node.ID, registered.Node.CertificateSerial)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.Revision <= approved.Revision {
		t.Fatal("heartbeat should advance node revision")
	}
	lostRotation, err := service.RotateCertificate(context.Background(), registered.Node.ID, registered.Node.CertificateSerial, testCSR(t))
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := service.RotateCertificate(context.Background(), registered.Node.ID, registered.Node.CertificateSerial, testCSR(t))
	if err != nil {
		t.Fatalf("retry rotation using the active certificate: %v", err)
	}
	if rotation.Node.PendingCertificateSerial == "" || rotation.Node.PendingCertificateSerial == registered.Node.CertificateSerial {
		t.Fatalf("unexpected pending certificate: %#v", rotation.Node)
	}
	if rotation.Node.PendingCertificateSerial == lostRotation.Node.PendingCertificateSerial {
		t.Fatal("rotation retry should replace an unrecoverable pending certificate")
	}
	if _, err := service.Heartbeat(context.Background(), registered.Node.ID, lostRotation.Node.PendingCertificateSerial); !errors.Is(err, enrollment.ErrUnauthorized) {
		t.Fatalf("replaced pending certificate should be rejected: %v", err)
	}
	if _, err := service.Heartbeat(context.Background(), registered.Node.ID, registered.Node.CertificateSerial); err != nil {
		t.Fatalf("old certificate should remain valid before activation: %v", err)
	}
	activated, err := service.Heartbeat(context.Background(), registered.Node.ID, rotation.Node.PendingCertificateSerial)
	if err != nil {
		t.Fatal(err)
	}
	if activated.CertificateSerial != rotation.Node.PendingCertificateSerial || activated.PendingCertificateSerial != "" {
		t.Fatalf("rotation was not activated: %#v", activated)
	}
	if _, err := service.Heartbeat(context.Background(), registered.Node.ID, registered.Node.CertificateSerial); !errors.Is(err, enrollment.ErrUnauthorized) {
		t.Fatalf("old certificate should be rejected after activation: %v", err)
	}
	revoked, err := service.RevokeCertificate(context.Background(), registered.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.CertificateRevokedAt == nil || revoked.Status != domain.NodeStatusOffline {
		t.Fatalf("unexpected revoked node: %#v", revoked)
	}
	if _, err := service.Heartbeat(context.Background(), registered.Node.ID, activated.CertificateSerial); !errors.Is(err, enrollment.ErrUnauthorized) {
		t.Fatalf("revoked certificate should be rejected: %v", err)
	}
	events, err := service.ListAuditEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 4 {
		t.Fatalf("expected lifecycle audit events, got %d", len(events))
	}
	autoToken, err := service.CreateToken(context.Background(), enrollment.TokenOptions{
		Name: "auto approve", TTL: 15 * time.Minute, MaxUses: 1, RequireApproval: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	inventory.Hostname = "compute-a02"
	automatic, err := service.Register(context.Background(), autoToken.Secret, testCSR(t), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if automatic.Node.Status != domain.NodeStatusReady || automatic.Node.ApprovedAt == nil {
		t.Fatalf("auto-approved token should create a ready node: %#v", automatic.Node)
	}
	rotations, err := service.RequestExpiringCertificateRotations(context.Background(), 31*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(rotations) != 1 || rotations[0].ID != automatic.Node.ID || rotations[0].PendingCertificateCommandID == "" || rotations[0].PendingCertificateRotationID == "" {
		t.Fatalf("expected one automatic certificate rotation command: %#v", rotations)
	}
	duplicateRotations, err := service.RequestExpiringCertificateRotations(context.Background(), 31*24*time.Hour)
	if err != nil || len(duplicateRotations) != 0 {
		t.Fatalf("automatic certificate rotation was not idempotent: rotations=%#v err=%v", duplicateRotations, err)
	}
	if _, err := service.ConfirmCertificateRotation(context.Background(), automatic.Node.ID, rotations[0].PendingCertificateCommandID, false, "test cleanup"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateNode(context.Background(), automatic.Node.ID, func(node *domain.Node) error {
		node.LastSeenAt = time.Now().Add(-3 * time.Minute)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	changed, err := service.ReconcileStaleNodes(context.Background(), 30*time.Second, 2*time.Minute)
	if err != nil || len(changed) != 1 || changed[0].Status != domain.NodeStatusUnreachable {
		t.Fatalf("expected unreachable reconciliation: changed=%#v err=%v", changed, err)
	}
	changed, err = service.ReconcileStaleNodes(context.Background(), 30*time.Second, 2*time.Minute)
	if err != nil || len(changed) != 1 || changed[0].Status != domain.NodeStatusOffline {
		t.Fatalf("expected offline reconciliation: changed=%#v err=%v", changed, err)
	}
	restored, err := service.HeartbeatWithMode(context.Background(), automatic.Node.ID, automatic.Node.CertificateSerial, domain.NodeStatusCordoned)
	if err != nil || restored.Status != domain.NodeStatusCordoned {
		t.Fatalf("expected heartbeat mode restoration: node=%#v err=%v", restored, err)
	}
	assignmentTime := time.Now().UTC()
	if err := service.ReportAssignment(context.Background(), automatic.Node.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: "assignment-1", Attempt: 1, State: xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING,
		OccurredAt: timestamppb.New(assignmentTime),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.ReportAssignment(context.Background(), automatic.Node.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: "assignment-1", Attempt: 1, State: xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED,
		OccurredAt: timestamppb.New(assignmentTime.Add(time.Second)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.ReportAssignment(context.Background(), automatic.Node.ID, &xirangv1.AssignmentUpdate{
		AssignmentId: "assignment-1", Attempt: 1, State: xirangv1.AssignmentState_ASSIGNMENT_STATE_ACCEPTED,
		OccurredAt: timestamppb.New(assignmentTime.Add(-time.Second)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetNode(context.Background(), registered.Node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != domain.NodeStatusOffline || persisted.CertificateRevokedAt == nil || persisted.Name != "compute-a01" {
		t.Fatalf("unexpected persisted node: %#v", persisted)
	}
	persistedEvents, err := reopened.ListAuditEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(persistedEvents) < 5 {
		t.Fatalf("expected persisted lifecycle audit events, got %d", len(persistedEvents))
	}
	reports, err := reopened.ListAssignmentReports(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].State != domain.AssignmentStateSucceeded {
		t.Fatalf("unexpected persisted legacy assignment reports: %#v", reports)
	}
}

func TestApplyAssignmentReportDoesNotPersistUnknownAssignment(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if _, changed, err := store.ApplyAssignmentReport(context.Background(), domain.AssignmentReport{
		NodeID: "node-a", AssignmentID: "unknown", Attempt: 1,
		State: domain.AssignmentStateRunning, OccurredAt: now, ReceivedAt: now,
	}); err != nil || changed {
		t.Fatalf("unknown report was not ignored: changed=%t err=%v", changed, err)
	}
	reports, err := store.ListAssignmentReports(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Fatalf("unknown assignment report reached persistent storage: %#v", reports)
	}
}

func testCSR(t *testing.T) string {
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
