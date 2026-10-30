package mountcontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Transport interface {
	EnqueueTransient(string, *xirangv1.ControlMessage) error
	ForgetTransient(string, string)
}
type pending struct {
	nodeID string
	result chan *xirangv1.SharedMountResult
}
type Broker struct {
	transport Transport
	mu        sync.Mutex
	pending   map[string]pending
	audit     func(context.Context, domain.AuditEvent) error
}

func New(transport Transport, audit ...func(context.Context, domain.AuditEvent) error) *Broker {
	broker := &Broker{transport: transport, pending: map[string]pending{}}
	if len(audit) > 0 {
		broker.audit = audit[0]
	}
	return broker
}

func (broker *Broker) Run(ctx context.Context, node domain.Node, operation *xirangv1.SharedMountOperation) (resultErr error) {
	if broker == nil || broker.transport == nil || !slices.Contains(node.Capabilities, Capability) {
		return errors.New("node has not enabled shared-mount-control-v1")
	}
	if operation == nil {
		return errors.New("shared mount operation is required")
	}
	operation = proto.Clone(operation).(*xirangv1.SharedMountOperation)
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	operation.OperationId = "mount_" + hex.EncodeToString(id)
	deadline, _ := ctx.Deadline()
	operation.ExpiresAt = timestamppb.New(deadline)
	if _, err := Arguments(operation, time.Now()); err != nil {
		return err
	}
	p := pending{nodeID: node.ID, result: make(chan *xirangv1.SharedMountResult, 1)}
	broker.mu.Lock()
	if len(broker.pending) >= 128 {
		broker.mu.Unlock()
		return errors.New("shared mount operation limit reached")
	}
	for _, current := range broker.pending {
		if current.nodeID == node.ID {
			broker.mu.Unlock()
			return errors.New("another shared mount operation is pending on this node")
		}
	}
	broker.pending[operation.OperationId] = p
	broker.mu.Unlock()
	defer func() {
		broker.transport.ForgetTransient(node.ID, operation.OperationId)
		broker.mu.Lock()
		delete(broker.pending, operation.OperationId)
		broker.mu.Unlock()
	}()
	actor, ok := identity.PrincipalFromContext(ctx)
	if !ok {
		actor = domain.Principal{Subject: "shared-space-reconciler", AuthMethod: "controller"}
	}
	audit := domain.AuditEvent{ID: "audit_" + operation.OperationId + "_requested", ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "shared.mount." + strings.ToLower(strings.TrimPrefix(operation.Action.String(), "ACTION_")), TargetKind: "node", TargetID: node.ID, Outcome: "requested", OccurredAt: time.Now().UTC(),
		Details: map[string]string{"operationID": operation.OperationId, "spaceID": operation.SpaceId, "sourcePath": operation.SourcePath, "mountPath": operation.MountPath}}
	if broker.audit != nil {
		if err := broker.audit(ctx, audit); err != nil {
			return fmt.Errorf("record mount intent before dispatch: %w", err)
		}
		defer func() {
			audit.Outcome = "succeeded"
			if resultErr != nil {
				audit.Outcome = "failed"
			}
			if ctx.Err() != nil {
				audit.Outcome = "unknown"
			}
			audit.ID = "audit_" + operation.OperationId + "_result"
			audit.OccurredAt = time.Now().UTC()
			auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer auditCancel()
			if err := broker.audit(auditCtx, audit); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("mount outcome %s; recording audit failed: %w", audit.Outcome, err))
			}
		}()
	}
	if err := broker.transport.EnqueueTransient(node.ID, &xirangv1.ControlMessage{MessageId: operation.OperationId, SentAt: timestamppb.Now(), Payload: &xirangv1.ControlMessage_SharedMountOperation{SharedMountOperation: operation}}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("shared mount result unavailable; outcome unknown: %w", ctx.Err())
	case result := <-p.result:
		if !result.Success {
			return fmt.Errorf("shared mount operation failed: %s", result.Message)
		}
		return nil
	}
}

// Unknown/late results are ignored. Only the authenticated target node can
// complete a pending operation; an ACK of acceptance is never completion.
func (broker *Broker) ReportMountResult(_ context.Context, nodeID string, result *xirangv1.SharedMountResult) error {
	if result == nil || !safeToken(result.OperationId, 96) || len(result.Message) > 1024 {
		return errors.New("invalid shared mount result")
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	p, ok := broker.pending[result.OperationId]
	if !ok {
		return nil
	}
	if p.nodeID != nodeID {
		return errors.New("shared mount result belongs to another node")
	}
	select {
	case p.result <- result:
	default:
	}
	return nil
}
