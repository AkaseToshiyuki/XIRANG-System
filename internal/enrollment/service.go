package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/pki"
	"github.com/xirang-grid/xirang/internal/runtimecompat"
)

type Service struct {
	repository Repository
	now        func() time.Time
	random     io.Reader
	issuer     *pki.Authority
	observerMu sync.RWMutex
	observer   func(domain.Node)
}

type TokenOptions struct {
	Name            string
	TTL             time.Duration
	MaxUses         uint32
	RequireApproval bool
}

type CreatedToken struct {
	Token  domain.EnrollmentToken `json:"token"`
	Secret string                 `json:"secret"`
}

type RegistrationResult struct {
	Node             domain.Node `json:"node"`
	CertificatePEM   string      `json:"certificatePem"`
	CACertificatePEM string      `json:"caCertificatePem"`
}

type CertificateRotationResult struct {
	Node             domain.Node `json:"node"`
	CertificatePEM   string      `json:"certificatePem"`
	CACertificatePEM string      `json:"caCertificatePem"`
}

func NewService(repository Repository, issuer *pki.Authority) *Service {
	return &Service{repository: repository, now: time.Now, random: rand.Reader, issuer: issuer}
}

func (service *Service) SetNodeObserver(observer func(domain.Node)) {
	service.observerMu.Lock()
	defer service.observerMu.Unlock()
	service.observer = observer
}

func (service *Service) CACertificatePEM() (string, error) {
	if service.issuer == nil {
		return "", ErrUnavailable
	}
	encoded, err := os.ReadFile(service.issuer.CAPath())
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (service *Service) CreateToken(ctx context.Context, options TokenOptions) (CreatedToken, error) {
	name := strings.TrimSpace(options.Name)
	if name == "" {
		return CreatedToken{}, fmt.Errorf("%w: token name is required", ErrInvalidRequest)
	}
	if options.TTL <= 0 || options.TTL > 365*24*time.Hour {
		return CreatedToken{}, fmt.Errorf("%w: token TTL must be between 1ns and 365d", ErrInvalidRequest)
	}
	if options.MaxUses == 0 || options.MaxUses > 1_000_000 {
		return CreatedToken{}, fmt.Errorf("%w: max uses must be between 1 and 1000000", ErrInvalidRequest)
	}

	id, err := randomText(service.random, 12)
	if err != nil {
		return CreatedToken{}, err
	}
	secret, err := randomText(service.random, 32)
	if err != nil {
		return CreatedToken{}, err
	}
	now := service.now().UTC()
	token := domain.EnrollmentToken{
		ID:              id,
		Name:            name,
		SecretHash:      digest(secret),
		RequireApproval: options.RequireApproval,
		MaxUses:         options.MaxUses,
		CreatedAt:       now,
		ExpiresAt:       now.Add(options.TTL),
	}
	if err := service.repository.CreateEnrollmentToken(ctx, token); err != nil {
		return CreatedToken{}, err
	}
	token.SecretHash = ""
	return CreatedToken{Token: token, Secret: "xet." + id + "." + secret}, nil
}

func (service *Service) RevokeToken(ctx context.Context, id string) error {
	repository, ok := service.repository.(interface {
		RevokeEnrollmentToken(context.Context, string, time.Time) error
	})
	if !ok {
		return ErrUnavailable
	}
	return repository.RevokeEnrollmentToken(ctx, id, service.now().UTC())
}

func (service *Service) Register(ctx context.Context, presentedToken, csrPEM string, inventory domain.InventorySummary) (RegistrationResult, error) {
	if err := validateInventory(inventory); err != nil {
		return RegistrationResult{}, err
	}
	tokenID, secret, err := parseToken(presentedToken)
	if err != nil {
		return RegistrationResult{}, err
	}
	nodeText, err := randomText(service.random, 12)
	if err != nil {
		return RegistrationResult{}, err
	}
	if service.issuer == nil {
		return RegistrationResult{}, errors.New("node certificate issuer is unavailable")
	}
	now := service.now().UTC()
	node := domain.Node{
		ID:            "node_" + nodeText,
		Name:          inventory.Hostname,
		Status:        domain.NodeStatusPendingApproval,
		OS:            inventory.OS,
		Architecture:  inventory.Architecture,
		Distribution:  inventory.Distribution,
		KernelVersion: inventory.KernelVersion,
		RuntimeFacts:  cloneTextValues(inventory.RuntimeFacts),
		AgentVersion:  inventory.AgentVersion,
		CPUModel:      inventory.CPUModel, CPUFeatures: append([]string(nil), inventory.CPUFeatures...),
		CPUBenchmarkScore: inventory.CPUBenchmarkScore, ComputeWeight: inventory.ComputeWeight,
		Labels:       map[string]string{"xirang.io/arch": inventory.Architecture},
		Roles:        []domain.NodeRole{domain.NodeRoleCompute},
		NUMATopology: cloneNUMATopology(inventory.NUMATopology), DeviceTopology: cloneDeviceTopology(inventory.DeviceTopology),
		Capacity: domain.ResourceVector{
			MilliCPU: int64(inventory.CPUCount) * 1000, MemoryBytes: inventory.MemoryBytes,
			EphemeralStorageBytes: inventory.EphemeralStorageBytes, Devices: cloneDeviceQuantities(inventory.Devices),
		},
		Allocatable: domain.ResourceVector{
			MilliCPU: int64(inventory.CPUCount) * 1000, MemoryBytes: inventory.MemoryBytes,
			EphemeralStorageBytes: inventory.EphemeralStorageBytes, Devices: cloneDeviceQuantities(inventory.Devices),
		},
		Revision:   1,
		CreatedAt:  now,
		UpdatedAt:  now,
		LastSeenAt: now,
	}
	issued, err := service.issuer.IssueNodeCertificate(node.ID, csrPEM)
	if err != nil {
		return RegistrationResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	node.CertificateSerial = issued.Serial
	node.CertificateNotAfter = &issued.NotAfter
	consumedToken, err := service.repository.EnrollNode(ctx, tokenID, digest(secret), node, now)
	if err != nil {
		return RegistrationResult{}, err
	}
	service.publishNode(node)
	if !consumedToken.RequireApproval {
		approved, err := service.updateNode(ctx, node.ID, func(current *domain.Node) error {
			current.Status = domain.NodeStatusReady
			current.ApprovedAt = &now
			current.UpdatedAt = now
			return nil
		}, nil)
		if err != nil {
			return RegistrationResult{}, err
		}
		node = approved
	}
	return RegistrationResult{Node: node, CertificatePEM: issued.CertificatePEM, CACertificatePEM: issued.CACertificatePEM}, nil
}

func (service *Service) ListNodes(ctx context.Context) ([]domain.Node, error) {
	nodes, err := service.repository.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	active := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.RemovedAt == nil {
			active = append(active, node)
		}
	}
	return active, nil
}

func (service *Service) GetNode(ctx context.Context, id string) (domain.Node, error) {
	return service.repository.GetNode(ctx, id)
}

func (service *Service) Approve(ctx context.Context, id string) (domain.Node, error) {
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.approved", id, nil)
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if !domain.CanTransitionNode(node.Status, domain.NodeStatusReady) {
			return ErrInvalidState
		}
		node.Status = domain.NodeStatusReady
		node.ApprovedAt = &now
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) Reject(ctx context.Context, id string) (domain.Node, error) {
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.rejected", id, nil)
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if !domain.CanTransitionNode(node.Status, domain.NodeStatusRejected) {
			return ErrInvalidState
		}
		node.Status = domain.NodeStatusRejected
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) SetNodeTaints(ctx context.Context, id string, taints []domain.Taint) (domain.Node, error) {
	if len(taints) > 64 {
		return domain.Node{}, ErrInvalidRequest
	}
	seen := make(map[string]bool, len(taints))
	for _, taint := range taints {
		if !validTaintText(taint.Key, 128, false) || !validTaintText(taint.Value, 256, true) || (taint.Effect != "NoSchedule" && taint.Effect != "NoExecute") {
			return domain.Node{}, ErrInvalidRequest
		}
		key := taint.Key + "\x00" + taint.Effect
		if seen[key] {
			return domain.Node{}, ErrInvalidRequest
		}
		seen[key] = true
	}
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.taints_updated", id, map[string]string{"count": strconv.Itoa(len(taints))})
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		node.Taints = append([]domain.Taint(nil), taints...)
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) SetNodeConfiguration(ctx context.Context, id string, roles []domain.NodeRole, managementAddress, sshUser, group string, computePriority int32) (domain.Node, error) {
	managementAddress, sshUser, group, err := validateNodeConfiguration(roles, managementAddress, sshUser, group, computePriority)
	if err != nil {
		return domain.Node{}, err
	}
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.configuration_updated", id, map[string]string{
		"roles": strings.Join(nodeRoleStrings(roles), ","), "managementAddress": managementAddress, "sshUser": sshUser,
		"group": group, "computePriority": strconv.FormatInt(int64(computePriority), 10),
	})
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.PendingManagementCommandID != "" {
			return ErrInvalidState
		}
		applyNodeConfiguration(node, roles, managementAddress, sshUser, group, computePriority, now)
		return nil
	}, audit)
}

func validateNodeConfiguration(roles []domain.NodeRole, managementAddress, sshUser, group string, computePriority int32) (string, string, string, error) {
	if len(roles) == 0 || len(roles) > 3 {
		return "", "", "", ErrInvalidRequest
	}
	seen := make(map[domain.NodeRole]bool, len(roles))
	for _, role := range roles {
		if (role != domain.NodeRoleManagement && role != domain.NodeRoleCompute && role != domain.NodeRoleStorage) || seen[role] {
			return "", "", "", ErrInvalidRequest
		}
		seen[role] = true
	}
	managementAddress = strings.TrimSpace(managementAddress)
	sshUser = strings.TrimSpace(sshUser)
	group = strings.TrimSpace(group)
	if len(managementAddress) > 255 || strings.ContainsAny(managementAddress, "\x00\r\n") ||
		len(sshUser) > 64 || (sshUser != "" && !validSSHUser(sshUser)) ||
		len(group) > 128 || strings.ContainsAny(group, "\x00\r\n") || computePriority < -1000 || computePriority > 1000 {
		return "", "", "", ErrInvalidRequest
	}
	return managementAddress, sshUser, group, nil
}

func applyNodeConfiguration(node *domain.Node, roles []domain.NodeRole, managementAddress, sshUser, group string, computePriority int32, now time.Time) {
	node.Roles = append([]domain.NodeRole(nil), roles...)
	node.ManagementAddress = managementAddress
	node.SSHUser = sshUser
	node.Group = group
	node.ComputePriority = computePriority
	node.UpdatedAt = now
}

func (service *Service) RequestNodeConfiguration(ctx context.Context, id string, roles []domain.NodeRole, managementAddress, sshUser, group string, computePriority int32, commandID string, managementEnabled bool) (domain.Node, error) {
	if commandID == "" {
		return domain.Node{}, ErrInvalidRequest
	}
	managementAddress, sshUser, group, err := validateNodeConfiguration(roles, managementAddress, sshUser, group, computePriority)
	if err != nil {
		return domain.Node{}, err
	}
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.configuration_updated", id, map[string]string{
		"roles": strings.Join(nodeRoleStrings(roles), ","), "managementAddress": managementAddress, "sshUser": sshUser,
		"group": group, "computePriority": strconv.FormatInt(int64(computePriority), 10), "managementPanel": strconv.FormatBool(managementEnabled),
		"commandId": commandID,
	})
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.PendingManagementCommandID != "" {
			return ErrInvalidState
		}
		applyNodeConfiguration(node, roles, managementAddress, sshUser, group, computePriority, now)
		desired := managementEnabled
		node.DesiredManagementPanel = &desired
		node.PendingManagementCommandID = commandID
		return nil
	}, audit)
}

func (service *Service) ConfirmManagementPanel(ctx context.Context, id, commandID string, accepted bool, reason string) (domain.Node, error) {
	current, err := service.repository.GetNode(ctx, id)
	if err != nil {
		return domain.Node{}, err
	}
	if commandID == "" || current.PendingManagementCommandID != commandID || current.DesiredManagementPanel == nil {
		return domain.Node{}, ErrInvalidState
	}
	action, outcome := "node.management_panel_rejected", "failure"
	if accepted {
		action, outcome = "node.management_panel_applied", "success"
	}
	audit, err := service.auditEvent(id, "node", action, id, map[string]string{
		"commandId": commandID, "enabled": strconv.FormatBool(*current.DesiredManagementPanel), "reason": reason,
	})
	if err != nil {
		return domain.Node{}, err
	}
	audit.Outcome = outcome
	now := service.now().UTC()
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.PendingManagementCommandID != commandID || node.DesiredManagementPanel == nil {
			return ErrInvalidState
		}
		desired := *node.DesiredManagementPanel
		if accepted {
			node.ManagementPanelEnabled = desired
			if desired {
				node.ManagementPanelStatus = "ready"
			} else {
				node.ManagementPanelStatus = "disabled"
			}
		} else {
			node.Roles = setManagementRole(node.Roles, !desired)
			node.ManagementPanelStatus = "error: " + strings.TrimSpace(reason)
		}
		node.DesiredManagementPanel = nil
		node.PendingManagementCommandID = ""
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) ReportManagementPanel(ctx context.Context, id string, enabled bool, status string) (domain.Node, error) {
	status = strings.TrimSpace(status)
	if len(status) > 512 || strings.ContainsAny(status, "\x00\r\n") {
		return domain.Node{}, ErrInvalidRequest
	}
	current, err := service.repository.GetNode(ctx, id)
	if err != nil {
		return domain.Node{}, err
	}
	if current.ManagementPanelEnabled == enabled && current.ManagementPanelStatus == status {
		return current, nil
	}
	now := service.now().UTC()
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		node.ManagementPanelEnabled = enabled
		node.ManagementPanelStatus = status
		node.UpdatedAt = now
		return nil
	}, nil)
}

func setManagementRole(roles []domain.NodeRole, enabled bool) []domain.NodeRole {
	updated := make([]domain.NodeRole, 0, len(roles)+1)
	found := false
	for _, role := range roles {
		if role == domain.NodeRoleManagement {
			found = true
			if !enabled {
				continue
			}
		}
		updated = append(updated, role)
	}
	if enabled && !found {
		updated = append(updated, domain.NodeRoleManagement)
	}
	return updated
}

func validSSHUser(value string) bool {
	for index, character := range value {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(index > 0 && character >= '0' && character <= '9') || character == '_' || character == '-') {
			return false
		}
	}
	return value != ""
}

func nodeRoleStrings(roles []domain.NodeRole) []string {
	result := make([]string, len(roles))
	for index, role := range roles {
		result[index] = string(role)
	}
	return result
}

func validTaintText(value string, maximum int, emptyAllowed bool) bool {
	return (emptyAllowed || value != "") && len(value) <= maximum && !strings.ContainsRune(value, '\x00')
}

func (service *Service) RequestNodeMode(ctx context.Context, id string, desired domain.NodeStatus, commandID string) (domain.Node, error) {
	if commandID == "" || (desired != domain.NodeStatusReady && desired != domain.NodeStatusCordoned && desired != domain.NodeStatusDraining) {
		return domain.Node{}, fmt.Errorf("%w: invalid node mode command", ErrInvalidRequest)
	}
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.mode_requested", id, map[string]string{
		"desiredStatus": string(desired), "commandId": commandID,
	})
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.CertificateRevokedAt != nil || node.PendingModeCommandID != "" || !domain.CanTransitionNode(node.Status, desired) {
			return ErrInvalidState
		}
		node.DesiredStatus = desired
		node.PendingModeCommandID = commandID
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) ConfirmNodeMode(ctx context.Context, id, commandID string, accepted bool, reason string) (domain.Node, error) {
	current, err := service.repository.GetNode(ctx, id)
	if err != nil {
		return domain.Node{}, err
	}
	if commandID == "" || current.PendingModeCommandID != commandID || current.DesiredStatus == "" {
		return domain.Node{}, ErrInvalidState
	}
	action := "node.mode_rejected"
	outcome := "failure"
	if accepted {
		action, outcome = "node.mode_applied", "success"
	}
	audit, err := service.auditEvent(id, "node", action, id, map[string]string{
		"desiredStatus": string(current.DesiredStatus), "commandId": commandID, "reason": reason,
	})
	if err != nil {
		return domain.Node{}, err
	}
	audit.Outcome = outcome
	now := service.now().UTC()
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.PendingModeCommandID != commandID || node.DesiredStatus == "" {
			return ErrInvalidState
		}
		if accepted {
			if !domain.CanTransitionNode(node.Status, node.DesiredStatus) {
				return ErrInvalidState
			}
			node.Status = node.DesiredStatus
		}
		node.DesiredStatus = ""
		node.PendingModeCommandID = ""
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) RequestCertificateRotation(ctx context.Context, id, commandID, rotationID string) (domain.Node, error) {
	return service.requestCertificateRotation(ctx, id, commandID, rotationID, "bootstrap-admin", "admin", false)
}

func (service *Service) requestCertificateRotation(ctx context.Context, id, commandID, rotationID, actorID, actorType string, automatic bool) (domain.Node, error) {
	if commandID == "" || rotationID == "" {
		return domain.Node{}, fmt.Errorf("%w: certificate rotation command IDs are required", ErrInvalidRequest)
	}
	now := service.now().UTC()
	audit, err := service.auditEvent(actorID, actorType, "certificate.rotation_requested", id, map[string]string{
		"commandId": commandID, "rotationId": rotationID, "automatic": strconv.FormatBool(automatic),
	})
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.CertificateSerial == "" || node.CertificateRevokedAt != nil || node.Status == domain.NodeStatusPendingApproval ||
			node.Status == domain.NodeStatusRejected || node.PendingCertificateSerial != "" || node.PendingCertificateCommandID != "" {
			return ErrInvalidState
		}
		node.PendingCertificateCommandID = commandID
		node.PendingCertificateRotationID = rotationID
		node.UpdatedAt = now
		return nil
	}, audit)
}

// RequestExpiringCertificateRotations persists rotation commands for active
// node certificates that are approaching expiry. Persisting before enqueueing
// makes the commands recoverable after a controller restart.
func (service *Service) RequestExpiringCertificateRotations(ctx context.Context, rotateBefore time.Duration) ([]domain.Node, error) {
	if rotateBefore <= 0 {
		return nil, fmt.Errorf("%w: certificate rotation window must be positive", ErrInvalidRequest)
	}
	nodes, err := service.repository.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := service.now().UTC()
	deadline := now.Add(rotateBefore)
	requested := make([]domain.Node, 0)
	for _, node := range nodes {
		if node.CertificateNotAfter == nil || !now.Before(*node.CertificateNotAfter) || node.CertificateNotAfter.After(deadline) ||
			node.CertificateSerial == "" || node.CertificateRevokedAt != nil || node.PendingCertificateSerial != "" ||
			node.PendingCertificateCommandID != "" || node.Status == domain.NodeStatusPendingApproval || node.Status == domain.NodeStatusRejected {
			continue
		}
		commandText, err := randomText(service.random, 18)
		if err != nil {
			return requested, err
		}
		rotationText, err := randomText(service.random, 18)
		if err != nil {
			return requested, err
		}
		updated, err := service.requestCertificateRotation(ctx, node.ID, "command_"+commandText, "rotation_"+rotationText, "xirangd", "system", true)
		if errors.Is(err, ErrInvalidState) {
			continue
		}
		if err != nil {
			return requested, err
		}
		requested = append(requested, updated)
	}
	return requested, nil
}

func (service *Service) ConfirmCertificateRotation(ctx context.Context, id, commandID string, accepted bool, reason string) (domain.Node, error) {
	current, err := service.repository.GetNode(ctx, id)
	if err != nil {
		return domain.Node{}, err
	}
	if commandID == "" || current.PendingCertificateCommandID != commandID || current.PendingCertificateRotationID == "" {
		return domain.Node{}, ErrInvalidState
	}
	action := "certificate.rotation_command_rejected"
	outcome := "failure"
	if accepted {
		action, outcome = "certificate.rotation_command_applied", "success"
	}
	audit, err := service.auditEvent(id, "node", action, id, map[string]string{
		"commandId": commandID, "rotationId": current.PendingCertificateRotationID, "reason": reason,
	})
	if err != nil {
		return domain.Node{}, err
	}
	audit.Outcome = outcome
	now := service.now().UTC()
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.PendingCertificateCommandID != commandID || node.PendingCertificateRotationID == "" {
			return ErrInvalidState
		}
		node.PendingCertificateCommandID = ""
		node.PendingCertificateRotationID = ""
		node.UpdatedAt = now
		return nil
	}, audit)
}

func (service *Service) Heartbeat(ctx context.Context, id, certificateSerial string) (domain.Node, error) {
	return service.HeartbeatWithMode(ctx, id, certificateSerial, "")
}

func (service *Service) HeartbeatWithMode(ctx context.Context, id, certificateSerial string, reportedMode domain.NodeStatus) (domain.Node, error) {
	if reportedMode != "" && reportedMode != domain.NodeStatusReady && reportedMode != domain.NodeStatusCordoned && reportedMode != domain.NodeStatusDraining {
		return domain.Node{}, fmt.Errorf("%w: invalid reported node mode", ErrInvalidRequest)
	}
	now := service.now().UTC()
	current, err := service.repository.GetNode(ctx, id)
	if err != nil {
		return domain.Node{}, err
	}
	var audit *domain.AuditEvent
	if current.PendingCertificateSerial == certificateSerial {
		audit, err = service.auditEvent(id, "node", "certificate.activated", id, map[string]string{"serial": certificateSerial})
		if err != nil {
			return domain.Node{}, err
		}
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.CertificateRevokedAt != nil || certificateSerial == "" {
			return ErrUnauthorized
		}
		if node.CertificateSerial != certificateSerial {
			if node.PendingCertificateSerial != certificateSerial {
				return ErrUnauthorized
			}
			node.CertificateSerial = node.PendingCertificateSerial
			node.CertificateNotAfter = node.PendingCertificateNotAfter
			node.PendingCertificateSerial = ""
			node.PendingCertificateNotAfter = nil
		}
		if node.CertificateNotAfter == nil || !now.Before(*node.CertificateNotAfter) {
			return ErrUnauthorized
		}
		node.LastSeenAt = now
		node.UpdatedAt = now
		if reportedMode != "" && node.PendingModeCommandID == "" {
			switch node.Status {
			case domain.NodeStatusReady, domain.NodeStatusCordoned, domain.NodeStatusDraining, domain.NodeStatusUnreachable, domain.NodeStatusOffline:
				node.Status = reportedMode
			}
		}
		return nil
	}, audit)
}

func (service *Service) ReconcileStaleNodes(ctx context.Context, unreachableAfter, offlineAfter time.Duration) ([]domain.Node, error) {
	if unreachableAfter <= 0 || offlineAfter <= unreachableAfter {
		return nil, fmt.Errorf("%w: offline timeout must be greater than unreachable timeout", ErrInvalidRequest)
	}
	nodes, err := service.repository.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := service.now().UTC()
	changed := make([]domain.Node, 0)
	for _, candidate := range nodes {
		var target domain.NodeStatus
		switch candidate.Status {
		case domain.NodeStatusReady, domain.NodeStatusCordoned, domain.NodeStatusDraining:
			if now.Sub(candidate.LastSeenAt) >= unreachableAfter {
				target = domain.NodeStatusUnreachable
			}
		case domain.NodeStatusUnreachable:
			if now.Sub(candidate.LastSeenAt) >= offlineAfter {
				target = domain.NodeStatusOffline
			}
		}
		if target == "" {
			continue
		}
		audit, err := service.auditEvent("xirangd", "system", "node."+string(target), candidate.ID, map[string]string{
			"lastSeenAt": candidate.LastSeenAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return changed, err
		}
		updated, err := service.updateNode(ctx, candidate.ID, func(node *domain.Node) error {
			if node.Status != candidate.Status || node.LastSeenAt.After(candidate.LastSeenAt) {
				return ErrConflict
			}
			node.Status = target
			node.UpdatedAt = now
			return nil
		}, audit)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return changed, err
		}
		changed = append(changed, updated)
	}
	return changed, nil
}

func (service *Service) RotateCertificate(ctx context.Context, id, currentSerial, csrPEM string) (CertificateRotationResult, error) {
	node, err := service.repository.GetNode(ctx, id)
	if err != nil {
		return CertificateRotationResult{}, err
	}
	if node.CertificateRevokedAt != nil || node.CertificateSerial != currentSerial {
		return CertificateRotationResult{}, ErrUnauthorized
	}
	issued, err := service.issuer.IssueNodeCertificate(id, csrPEM)
	if err != nil {
		return CertificateRotationResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	details := map[string]string{"pendingSerial": issued.Serial}
	if node.PendingCertificateSerial != "" {
		details["replacedPendingSerial"] = node.PendingCertificateSerial
	}
	audit, err := service.auditEvent(id, "node", "certificate.rotation_requested", id, details)
	if err != nil {
		return CertificateRotationResult{}, err
	}
	updated, err := service.updateNode(ctx, id, func(current *domain.Node) error {
		if current.CertificateRevokedAt != nil || current.CertificateSerial != currentSerial {
			return ErrUnauthorized
		}
		current.PendingCertificateSerial = issued.Serial
		current.PendingCertificateNotAfter = &issued.NotAfter
		current.UpdatedAt = service.now().UTC()
		return nil
	}, audit)
	if err != nil {
		return CertificateRotationResult{}, err
	}
	return CertificateRotationResult{Node: updated, CertificatePEM: issued.CertificatePEM, CACertificatePEM: issued.CACertificatePEM}, nil
}

func (service *Service) RevokeCertificate(ctx context.Context, id string) (domain.Node, error) {
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "certificate.revoked", id, nil)
	if err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, id, func(node *domain.Node) error {
		if node.CertificateRevokedAt != nil {
			return ErrInvalidState
		}
		node.CertificateRevokedAt = &now
		node.PendingCertificateSerial = ""
		node.PendingCertificateNotAfter = nil
		node.UpdatedAt = now
		switch node.Status {
		case domain.NodeStatusPendingApproval, domain.NodeStatusEnrolling, domain.NodeStatusDiscovered:
			node.Status = domain.NodeStatusRejected
		case domain.NodeStatusReady, domain.NodeStatusCordoned, domain.NodeStatusDraining, domain.NodeStatusUnreachable:
			node.Status = domain.NodeStatusOffline
		}
		return nil
	}, audit)
}

// RemoveNode retires an inactive node from operational views while retaining
// its immutable identity and historical references for audit and job records.
// A connected or potentially recoverable node must first complete its normal
// lifecycle transition to offline; rejected enrollments may be removed directly.
func (service *Service) RemoveNode(ctx context.Context, id string) (domain.Node, error) {
	now := service.now().UTC()
	audit, err := service.auditEvent("bootstrap-admin", "admin", "node.removed", id, nil)
	if err != nil {
		return domain.Node{}, err
	}
	node, err := service.repository.UpdateNode(ctx, id, func(node *domain.Node) error {
		if node.RemovedAt != nil || (node.Status != domain.NodeStatusOffline && node.Status != domain.NodeStatusRejected) {
			return ErrInvalidState
		}
		if node.CertificateRevokedAt == nil {
			node.CertificateRevokedAt = &now
		}
		node.Status = domain.NodeStatusRemoved
		node.DesiredStatus = ""
		node.PendingModeCommandID = ""
		node.PendingManagementCommandID = ""
		node.DesiredManagementPanel = nil
		node.PendingCertificateSerial = ""
		node.PendingCertificateNotAfter = nil
		node.PendingCertificateCommandID = ""
		node.PendingCertificateRotationID = ""
		node.RemovedAt = &now
		node.UpdatedAt = now
		return nil
	}, audit)
	if err == nil {
		service.publishNode(node)
	}
	return node, err
}

func (service *Service) ListAuditEvents(ctx context.Context, limit int) ([]domain.AuditEvent, error) {
	return service.repository.ListAuditEvents(ctx, limit)
}

func (service *Service) ReportAssignment(ctx context.Context, nodeID string, update *xirangv1.AssignmentUpdate) error {
	if update == nil || nodeID == "" || len(update.AssignmentId) == 0 || len(update.AssignmentId) > 256 || strings.ContainsRune(update.AssignmentId, '\x00') || update.Attempt == 0 ||
		update.OccurredAt == nil || len(update.Reason) > 4096 {
		return ErrInvalidRequest
	}
	if err := update.OccurredAt.CheckValid(); err != nil {
		return ErrInvalidRequest
	}
	state, ok := assignmentState(update.State)
	if !ok {
		return ErrInvalidRequest
	}
	now := service.now().UTC()
	occurredAt := update.OccurredAt.AsTime().UTC()
	if occurredAt.After(now.Add(5 * time.Minute)) {
		return ErrInvalidRequest
	}
	return service.repository.UpsertAssignmentReport(ctx, domain.AssignmentReport{
		NodeID: nodeID, AssignmentID: update.AssignmentId, Attempt: update.Attempt,
		State: state, ExitCode: update.ExitCode, Reason: update.Reason,
		OccurredAt: occurredAt, ReceivedAt: now,
	})
}

func (service *Service) ReportCapabilities(ctx context.Context, nodeID string, capabilities []string) error {
	if nodeID == "" || len(capabilities) > 64 {
		return ErrInvalidRequest
	}
	cloned := append([]string(nil), capabilities...)
	for _, capability := range cloned {
		if capability == "" || len(capability) > 128 || strings.ContainsRune(capability, '\x00') {
			return ErrInvalidRequest
		}
	}
	_, err := service.updateNode(ctx, nodeID, func(node *domain.Node) error {
		node.Capabilities = cloned
		node.UpdatedAt = service.now().UTC()
		return nil
	}, nil)
	return err
}

func (service *Service) ReportAgentVersion(ctx context.Context, nodeID, agentVersion string) error {
	agentVersion = strings.TrimSpace(agentVersion)
	if nodeID == "" || agentVersion == "" || len(agentVersion) > 128 || strings.ContainsRune(agentVersion, '\x00') {
		return ErrInvalidRequest
	}
	_, err := service.updateNode(ctx, nodeID, func(node *domain.Node) error {
		node.AgentVersion = agentVersion
		node.UpdatedAt = service.now().UTC()
		return nil
	}, nil)
	return err
}

func (service *Service) ReportDeviceCapacity(ctx context.Context, nodeID string, devices map[string]int64) (domain.Node, error) {
	return service.ReportHardwareTopology(ctx, nodeID, devices, nil, nil)
}

func (service *Service) ReportHardwareTopology(ctx context.Context, nodeID string, devices map[string]int64, numa []domain.NUMANode, deviceTopology map[string]map[string]int64) (domain.Node, error) {
	if nodeID == "" || len(devices) > 64 {
		return domain.Node{}, ErrInvalidRequest
	}
	for name, quantity := range devices {
		if !validTaintText(name, 128, false) || quantity <= 0 || quantity > 1_000_000 {
			return domain.Node{}, ErrInvalidRequest
		}
	}
	if err := validateHardwareTopology(numa, deviceTopology); err != nil {
		return domain.Node{}, err
	}
	return service.updateNode(ctx, nodeID, func(node *domain.Node) error {
		capacity := cloneDeviceQuantities(devices)
		allocatable := cloneDeviceQuantities(devices)
		if allocatable == nil && len(node.Allocated.Devices) > 0 {
			allocatable = make(map[string]int64, len(node.Allocated.Devices))
			capacity = make(map[string]int64, len(node.Allocated.Devices))
		}
		for name, allocated := range node.Allocated.Devices {
			if allocated > allocatable[name] {
				allocatable[name] = allocated
				capacity[name] = allocated
			}
		}
		node.Capacity.Devices = capacity
		node.Allocatable.Devices = allocatable
		if numa != nil {
			node.NUMATopology = cloneNUMATopology(numa)
		}
		if deviceTopology != nil {
			node.DeviceTopology = cloneDeviceTopology(deviceTopology)
		}
		node.UpdatedAt = service.now().UTC()
		return nil
	}, nil)
}

func (service *Service) ReportHardwarePerformance(ctx context.Context, nodeID, cpuModel string, cpuFeatures []string, benchmarkScore, computeWeight float64) (domain.Node, error) {
	if nodeID == "" || len(cpuModel) > 512 || strings.ContainsRune(cpuModel, '\x00') || len(cpuFeatures) > 1024 ||
		benchmarkScore < 0 || benchmarkScore > 1_000_000 || computeWeight < 0 || computeWeight > 1_000_000 {
		return domain.Node{}, ErrInvalidRequest
	}
	features := append([]string(nil), cpuFeatures...)
	for _, feature := range features {
		if !validTaintText(feature, 64, false) {
			return domain.Node{}, ErrInvalidRequest
		}
	}
	return service.updateNode(ctx, nodeID, func(node *domain.Node) error {
		node.CPUModel, node.CPUFeatures = cpuModel, features
		node.CPUBenchmarkScore, node.ComputeWeight = benchmarkScore, computeWeight
		node.UpdatedAt = service.now().UTC()
		return nil
	}, nil)
}

func (service *Service) ReportRuntimeFacts(ctx context.Context, nodeID string, facts map[string]string) (domain.Node, error) {
	if nodeID == "" || runtimecompat.ValidateFacts(facts) != nil {
		return domain.Node{}, ErrInvalidRequest
	}
	cloned := cloneTextValues(facts)
	return service.updateNode(ctx, nodeID, func(node *domain.Node) error {
		node.RuntimeFacts = cloned
		node.UpdatedAt = service.now().UTC()
		return nil
	}, nil)
}

func (service *Service) ListAssignmentReports(ctx context.Context, limit int) ([]domain.AssignmentReport, error) {
	return service.repository.ListAssignmentReports(ctx, limit)
}

func (service *Service) updateNode(ctx context.Context, id string, update func(*domain.Node) error, audit *domain.AuditEvent) (domain.Node, error) {
	node, err := service.repository.UpdateNode(ctx, id, func(node *domain.Node) error {
		if node.RemovedAt != nil {
			return ErrInvalidState
		}
		return update(node)
	}, audit)
	if err == nil {
		service.publishNode(node)
	}
	return node, err
}

func (service *Service) publishNode(node domain.Node) {
	service.observerMu.RLock()
	observer := service.observer
	service.observerMu.RUnlock()
	if observer != nil {
		observer(node)
	}
}

func assignmentState(state xirangv1.AssignmentState) (domain.AssignmentState, bool) {
	switch state {
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_ACCEPTED:
		return domain.AssignmentStateAccepted, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_PULLING:
		return domain.AssignmentStatePulling, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_RUNNING:
		return domain.AssignmentStateRunning, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_SUSPENDED:
		return domain.AssignmentStateSuspended, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_SUCCEEDED:
		return domain.AssignmentStateSucceeded, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_FAILED:
		return domain.AssignmentStateFailed, true
	case xirangv1.AssignmentState_ASSIGNMENT_STATE_CANCELED:
		return domain.AssignmentStateCanceled, true
	default:
		return "", false
	}
}

func (service *Service) auditEvent(actorID, actorType, action, targetID string, details map[string]string) (*domain.AuditEvent, error) {
	id, err := randomText(service.random, 12)
	if err != nil {
		return nil, err
	}
	return &domain.AuditEvent{
		ID: "audit_" + id, ActorID: actorID, ActorType: actorType, Action: action,
		TargetKind: "node", TargetID: targetID, Outcome: "success",
		OccurredAt: service.now().UTC(), Details: details,
	}, nil
}

func validateInventory(inventory domain.InventorySummary) error {
	if strings.TrimSpace(inventory.Hostname) == "" || inventory.OS != "linux" ||
		strings.TrimSpace(inventory.Architecture) == "" || inventory.CPUCount <= 0 || inventory.MemoryBytes <= 0 ||
		inventory.EphemeralStorageBytes < 0 || len(inventory.CPUModel) > 512 || strings.ContainsRune(inventory.CPUModel, '\x00') ||
		len(inventory.CPUFeatures) > 1024 || inventory.CPUBenchmarkScore < 0 || inventory.CPUBenchmarkScore > 1_000_000 ||
		inventory.ComputeWeight < 0 || inventory.ComputeWeight > 1_000_000 || runtimecompat.ValidateFacts(inventory.RuntimeFacts) != nil {
		return fmt.Errorf("%w: incomplete or unsupported inventory", ErrInvalidRequest)
	}
	for _, feature := range inventory.CPUFeatures {
		if !validTaintText(feature, 64, false) {
			return fmt.Errorf("%w: invalid CPU feature", ErrInvalidRequest)
		}
	}
	if len(inventory.Devices) > 64 {
		return fmt.Errorf("%w: too many device resource types", ErrInvalidRequest)
	}
	for name, quantity := range inventory.Devices {
		if !validTaintText(name, 128, false) || quantity <= 0 || quantity > 1_000_000 {
			return fmt.Errorf("%w: invalid device resource", ErrInvalidRequest)
		}
	}
	if err := validateHardwareTopology(inventory.NUMATopology, inventory.DeviceTopology); err != nil {
		return err
	}
	return nil
}

func validateHardwareTopology(numa []domain.NUMANode, deviceTopology map[string]map[string]int64) error {
	if len(numa) > 1024 || len(deviceTopology) > 64 {
		return ErrInvalidRequest
	}
	seen := make(map[uint32]bool, len(numa))
	for _, node := range numa {
		if seen[node.ID] || !validCPUList(node.CPUs) || node.MemoryBytes < 0 {
			return ErrInvalidRequest
		}
		seen[node.ID] = true
	}
	for resource, byNode := range deviceTopology {
		if !validTaintText(resource, 128, false) || len(byNode) > 1024 {
			return ErrInvalidRequest
		}
		for node, quantity := range byNode {
			if _, err := strconv.ParseUint(node, 10, 32); err != nil || quantity <= 0 || quantity > 1_000_000 {
				return ErrInvalidRequest
			}
		}
	}
	return nil
}

func cloneTextValues(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func validCPUList(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && character != ',' && character != '-' {
			return false
		}
	}
	return true
}

func cloneNUMATopology(values []domain.NUMANode) []domain.NUMANode {
	return append([]domain.NUMANode(nil), values...)
}

func cloneDeviceTopology(values map[string]map[string]int64) map[string]map[string]int64 {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]map[string]int64, len(values))
	for resource, byNode := range values {
		cloned[resource] = cloneDeviceQuantities(byNode)
	}
	return cloned
}

func cloneDeviceQuantities(values map[string]int64) map[string]int64 {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]int64, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func parseToken(value string) (string, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "xet" || parts[1] == "" || parts[2] == "" {
		return "", "", ErrInvalidToken
	}
	return parts[1], parts[2], nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func randomText(reader io.Reader, size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", fmt.Errorf("generate secure random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
