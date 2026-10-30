// Package alerting manages durable, duration-gated operator alerts.
package alerting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

var (
	ErrNotFound     = errors.New("alert not found")
	ErrInvalidState = errors.New("invalid alert state")
)

type Repository interface {
	List(context.Context) ([]domain.Alert, error)
	Get(context.Context, string) (domain.Alert, error)
	Put(context.Context, domain.Alert) error
	PutTransition(context.Context, domain.Alert) error
	Delete(context.Context, string) error
	Close() error
}

type Config struct {
	TriggerAfter      time.Duration
	ResolveAfter      time.Duration
	RecordTransitions bool
	Policy            func(context.Context) (domain.AlertPolicy, error)
}

type Service struct {
	repository   Repository
	config       Config
	now          func() time.Time
	mu           sync.Mutex
	subscriberMu sync.Mutex
	subscribers  map[uint64]chan domain.Alert
	nextID       uint64
}

func NewService(repository Repository, config Config) *Service {
	if config.TriggerAfter <= 0 {
		config.TriggerAfter = 30 * time.Second
	}
	if config.ResolveAfter <= 0 {
		config.ResolveAfter = 30 * time.Second
	}
	return &Service{repository: repository, config: config, now: time.Now, subscribers: make(map[uint64]chan domain.Alert)}
}

func (service *Service) Evaluate(ctx context.Context, snapshot domain.TopologySnapshot) ([]domain.Alert, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	now := service.now().UTC()
	triggerAfter, resolveAfter := service.config.TriggerAfter, service.config.ResolveAfter
	if service.config.Policy != nil {
		policy, err := service.config.Policy(ctx)
		if err != nil {
			return nil, err
		}
		triggerAfter = time.Duration(policy.TriggerAfterSeconds * float64(time.Second))
		resolveAfter = time.Duration(policy.ResolveAfterSeconds * float64(time.Second))
	}
	alerts, err := service.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	current := make(map[string]domain.Alert)
	for _, alert := range alerts {
		if alert.Status == domain.AlertStatusResolved {
			continue
		}
		if existing, ok := current[alert.Fingerprint]; !ok || alert.FirstObservedAt.After(existing.FirstObservedAt) {
			current[alert.Fingerprint] = alert
		}
	}
	seen := make(map[string]struct{})
	for _, signal := range topologySignals(snapshot) {
		seen[signal.fingerprint] = struct{}{}
		alert, exists := current[signal.fingerprint]
		if signal.health == "healthy" {
			if !exists {
				continue
			}
			if alert.Status == domain.AlertStatusPending {
				if err := service.repository.Delete(ctx, alert.ID); err != nil {
					return nil, err
				}
				delete(current, signal.fingerprint)
				continue
			}
			alert.LastObservedAt = now
			alert.ObservedHealth = signal.health
			if alert.HealthySince == nil {
				alert.HealthySince = timePointer(now)
			}
			publish := false
			if now.Sub(*alert.HealthySince) >= resolveAfter {
				alert.Status = domain.AlertStatusResolved
				alert.ResolvedAt = timePointer(now)
				publish = true
				delete(current, signal.fingerprint)
			}
			alert.Revision++
			if err := service.persist(ctx, alert, publish); err != nil {
				return nil, err
			}
			if publish {
				service.publish(alert)
			}
			continue
		}
		publish := false
		if !exists {
			alert = newPendingAlert(signal, now)
			current[signal.fingerprint] = alert
		} else {
			previousSeverity := alert.Severity
			previousHealth := alert.ObservedHealth
			alert.LastObservedAt = now
			alert.HealthySince = nil
			applySignal(&alert, signal)
			alert.Revision++
			publish = alert.Status != domain.AlertStatusPending && (alert.Severity != previousSeverity || alert.ObservedHealth != previousHealth)
		}
		if alert.Status == domain.AlertStatusPending && now.Sub(alert.FirstObservedAt) >= triggerAfter {
			alert.Status = domain.AlertStatusFiring
			alert.FiringAt = timePointer(now)
			publish = true
		}
		if err := service.persist(ctx, alert, publish); err != nil {
			return nil, err
		}
		current[signal.fingerprint] = alert
		if publish {
			service.publish(alert)
		}
	}
	// A pending condition must be observed continuously for the full trigger
	// window. Missing telemetry is unknown rather than healthy, so it leaves
	// visible alerts open but restarts the clock for hidden pending state.
	for fingerprint, alert := range current {
		if alert.Status != domain.AlertStatusPending {
			continue
		}
		if _, observed := seen[fingerprint]; observed {
			continue
		}
		if err := service.repository.Delete(ctx, alert.ID); err != nil {
			return nil, err
		}
	}
	return service.visibleAlerts(ctx, false)
}

func (service *Service) List(ctx context.Context, includeResolved bool) ([]domain.Alert, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.visibleAlerts(ctx, includeResolved)
}

// ReportCondition creates or updates an immediately visible operational alert.
// It is used for controller/Agent safety actions that must not wait for the
// duration gate used by noisy interface telemetry.
func (service *Service) ReportCondition(ctx context.Context, fingerprint, kind, nodeID string, severity domain.AlertSeverity, summary string) (domain.Alert, error) {
	if fingerprint == "" || kind == "" || summary == "" ||
		(severity != domain.AlertSeverityInfo && severity != domain.AlertSeverityWarning && severity != domain.AlertSeverityCritical) {
		return domain.Alert{}, ErrInvalidState
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	now := service.now().UTC()
	alerts, err := service.repository.List(ctx)
	if err != nil {
		return domain.Alert{}, err
	}
	var alert domain.Alert
	found := false
	for _, candidate := range alerts {
		if candidate.Fingerprint == fingerprint && candidate.Status != domain.AlertStatusResolved {
			alert, found = candidate, true
			break
		}
	}
	transition := !found
	if !found {
		digest := sha256.Sum256([]byte(fingerprint + "\x00" + now.Format(time.RFC3339Nano)))
		alert = domain.Alert{
			ID: "alert_" + hex.EncodeToString(digest[:12]), Fingerprint: fingerprint, Kind: kind,
			SourceNodeID: nodeID, Status: domain.AlertStatusFiring, FirstObservedAt: now,
			FiringAt: timePointer(now), Revision: 1,
		}
	} else {
		transition = alert.Kind != kind || alert.SourceNodeID != nodeID ||
			alert.Severity != severity || alert.Summary != summary
		alert.Revision++
	}
	alert.LastObservedAt, alert.ObservedHealth = now, "unhealthy"
	alert.Kind, alert.SourceNodeID = kind, nodeID
	alert.Severity, alert.Summary = severity, summary
	if err := service.persist(ctx, alert, transition); err != nil {
		return domain.Alert{}, err
	}
	if transition {
		service.publish(alert)
	}
	return alert, nil
}

func (service *Service) ResolveCondition(ctx context.Context, fingerprint, summary string) (domain.Alert, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	alerts, err := service.repository.List(ctx)
	if err != nil {
		return domain.Alert{}, err
	}
	for _, alert := range alerts {
		if alert.Fingerprint != fingerprint || alert.Status == domain.AlertStatusResolved {
			continue
		}
		now := service.now().UTC()
		alert.Status, alert.LastObservedAt, alert.ObservedHealth = domain.AlertStatusResolved, now, "healthy"
		alert.ResolvedAt, alert.HealthySince = timePointer(now), timePointer(now)
		if strings.TrimSpace(summary) != "" {
			alert.Summary = strings.TrimSpace(summary)
		}
		alert.Revision++
		if err := service.persist(ctx, alert, true); err != nil {
			return domain.Alert{}, err
		}
		service.publish(alert)
		return alert, nil
	}
	return domain.Alert{}, ErrNotFound
}

// ResetPending discards hidden pre-firing state after controller startup. Time
// spent while the evaluator was not running cannot prove continuous observation.
func (service *Service) ResetPending(ctx context.Context) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	alerts, err := service.repository.List(ctx)
	if err != nil {
		return err
	}
	for _, alert := range alerts {
		if alert.Status == domain.AlertStatusPending {
			if err := service.repository.Delete(ctx, alert.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (service *Service) visibleAlerts(ctx context.Context, includeResolved bool) ([]domain.Alert, error) {
	alerts, err := service.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	visible := alerts[:0]
	for _, alert := range alerts {
		if alert.Status == domain.AlertStatusPending || (!includeResolved && alert.Status == domain.AlertStatusResolved) {
			continue
		}
		visible = append(visible, alert)
	}
	sort.Slice(visible, func(left, right int) bool { return visible[left].LastObservedAt.After(visible[right].LastObservedAt) })
	return visible, nil
}

func (service *Service) Acknowledge(ctx context.Context, id, actor string) (domain.Alert, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	alert, err := service.repository.Get(ctx, id)
	if err != nil {
		return domain.Alert{}, err
	}
	if alert.Status == domain.AlertStatusAcknowledged {
		return alert, nil
	}
	if alert.Status != domain.AlertStatusFiring {
		return domain.Alert{}, ErrInvalidState
	}
	alert.Status = domain.AlertStatusAcknowledged
	alert.AcknowledgedAt = timePointer(service.now().UTC())
	alert.AcknowledgedBy = actor
	alert.Revision++
	if err := service.persist(ctx, alert, true); err != nil {
		return domain.Alert{}, err
	}
	service.publish(alert)
	return alert, nil
}

func (service *Service) Subscribe(ctx context.Context) <-chan domain.Alert {
	updates := make(chan domain.Alert, 32)
	service.subscriberMu.Lock()
	service.nextID++
	id := service.nextID
	service.subscribers[id] = updates
	service.subscriberMu.Unlock()
	go func() {
		<-ctx.Done()
		service.subscriberMu.Lock()
		delete(service.subscribers, id)
		close(updates)
		service.subscriberMu.Unlock()
	}()
	return updates
}

func (service *Service) publish(alert domain.Alert) {
	service.subscriberMu.Lock()
	defer service.subscriberMu.Unlock()
	for _, subscriber := range service.subscribers {
		select {
		case subscriber <- alert:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- alert:
			default:
			}
		}
	}
}

func (service *Service) persist(ctx context.Context, alert domain.Alert, transition bool) error {
	if transition && service.config.RecordTransitions {
		return service.repository.PutTransition(ctx, alert)
	}
	return service.repository.Put(ctx, alert)
}

type interfaceSignal struct {
	fingerprint             string
	nodeID                  string
	interfaceName           string
	health                  string
	observedBits            int64
	droppedPacketsPerSecond float64
	errorPacketsPerSecond   float64
	dropRatio               float64
}

func topologySignals(snapshot domain.TopologySnapshot) []interfaceSignal {
	signals := map[string]interfaceSignal{}
	for _, edge := range snapshot.Edges {
		if edge.SourceID == "" || edge.SourceInterface == "" || edge.Health == "" {
			continue
		}
		fingerprint := interfaceFingerprint(edge.SourceID, edge.SourceInterface)
		signal := interfaceSignal{
			fingerprint: fingerprint, nodeID: edge.SourceID, interfaceName: edge.SourceInterface,
			health: edge.Health, observedBits: edge.ObservedBits,
			droppedPacketsPerSecond: edge.DroppedPacketsPerSecond, errorPacketsPerSecond: edge.ErrorPacketsPerSecond,
			dropRatio: edge.DropRatio,
		}
		if existing, ok := signals[fingerprint]; !ok || healthPriority(signal.health) > healthPriority(existing.health) {
			signals[fingerprint] = signal
		}
	}
	result := make([]interfaceSignal, 0, len(signals))
	for _, signal := range signals {
		result = append(result, signal)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].fingerprint < result[right].fingerprint })
	return result
}

func newPendingAlert(signal interfaceSignal, now time.Time) domain.Alert {
	digest := sha256.Sum256([]byte(signal.fingerprint + "\x00" + now.Format(time.RFC3339Nano)))
	alert := domain.Alert{
		ID: "alert_" + hex.EncodeToString(digest[:12]), Fingerprint: signal.fingerprint, Kind: "interface_health",
		SourceNodeID: signal.nodeID, SourceInterface: signal.interfaceName,
		Status: domain.AlertStatusPending, FirstObservedAt: now, LastObservedAt: now, Revision: 1,
	}
	applySignal(&alert, signal)
	return alert
}

func applySignal(alert *domain.Alert, signal interfaceSignal) {
	alert.ObservedHealth = signal.health
	alert.Severity = domain.AlertSeverityWarning
	if signal.health == "critical" {
		alert.Severity = domain.AlertSeverityCritical
	}
	alert.Summary = fmt.Sprintf("interface %s on node %s reports %s health", signal.interfaceName, signal.nodeID, signal.health)
	alert.ObservedBits = signal.observedBits
	alert.DroppedPacketsPerSecond = signal.droppedPacketsPerSecond
	alert.ErrorPacketsPerSecond = signal.errorPacketsPerSecond
	alert.DropRatio = signal.dropRatio
}

func healthPriority(health string) int {
	return map[string]int{"healthy": 1, "degraded": 2, "critical": 3}[health]
}

func timePointer(value time.Time) *time.Time { return &value }

func interfaceFingerprint(nodeID, interfaceName string) string {
	digest := sha256.Sum256([]byte(nodeID + "\x00" + interfaceName))
	return "interface_" + hex.EncodeToString(digest[:12])
}
