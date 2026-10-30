// Package alertpolicy manages audited, durable alert and notification policy.
package alertpolicy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
)

var ErrInvalid = errors.New("invalid alert policy")
var ErrConflict = errors.New("alert policy changed; reload before saving")

type Repository interface {
	GetAlertPolicy(context.Context) (domain.AlertPolicy, error)
	PutAlertPolicy(context.Context, uint64, domain.AlertPolicy, domain.AuditEvent) error
}

type Service struct {
	repository Repository
	defaults   domain.AlertPolicy
}

func Defaults(trigger, resolve time.Duration) domain.AlertPolicy {
	if trigger <= 0 {
		trigger = 30 * time.Second
	}
	if resolve <= 0 {
		resolve = 30 * time.Second
	}
	return domain.AlertPolicy{TriggerAfterSeconds: trigger.Seconds(), ResolveAfterSeconds: resolve.Seconds(),
		RouteEnabled: true, MinimumSeverity: domain.AlertSeverityInfo,
		Kinds: []string{"interface_health", "job_storage", "group_storage", "node_storage"}, DeliveryTargetSeconds: 300}
}

func New(repository Repository, defaults domain.AlertPolicy) *Service {
	defaults.Kinds = slices.Clone(defaults.Kinds)
	return &Service{repository: repository, defaults: defaults}
}

func (service *Service) Get(ctx context.Context) (domain.AlertPolicy, error) {
	value, err := service.repository.GetAlertPolicy(ctx)
	if err == nil && value.Revision == 0 {
		value = service.defaults
		value.Kinds = slices.Clone(value.Kinds)
	}
	return value, err
}

func (service *Service) Update(ctx context.Context, value domain.AlertPolicy, actor domain.Principal) (domain.AlertPolicy, error) {
	if actor.Role != "admin" {
		return domain.AlertPolicy{}, identity.ErrForbidden
	}
	now := time.Now().UTC()
	for _, duration := range []float64{value.TriggerAfterSeconds, value.ResolveAfterSeconds} {
		if math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0.001 || duration > 86400 {
			return domain.AlertPolicy{}, fmt.Errorf("%w: trigger/resolve durations must be 0.001–86400 seconds", ErrInvalid)
		}
	}
	if value.Revision == ^uint64(0) || value.DeliveryTargetSeconds < 1 || value.DeliveryTargetSeconds > 86400 || severity(value.MinimumSeverity) < 0 {
		return domain.AlertPolicy{}, ErrInvalid
	}
	if len(value.Kinds) > 4 {
		return domain.AlertPolicy{}, ErrInvalid
	}
	seen := map[string]bool{}
	for _, kind := range value.Kinds {
		if !slices.Contains([]string{"interface_health", "job_storage", "group_storage", "node_storage"}, kind) || seen[kind] {
			return domain.AlertPolicy{}, fmt.Errorf("%w: unsupported or repeated condition type", ErrInvalid)
		}
		seen[kind] = true
	}
	value.Kinds = append([]string{}, value.Kinds...)
	if value.SilenceUntil != nil {
		if value.SilenceUntil.After(now.Add(30 * 24 * time.Hour)) {
			return domain.AlertPolicy{}, fmt.Errorf("%w: silence must end within 30 days", ErrInvalid)
		}
		if !value.SilenceUntil.After(now) {
			value.SilenceUntil = nil
		}
	}
	expected := value.Revision
	value.Revision, value.UpdatedAt = expected+1, now
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return domain.AlertPolicy{}, err
	}
	err := service.repository.PutAlertPolicy(ctx, expected, value, domain.AuditEvent{
		ID: "audit_" + hex.EncodeToString(id[:]), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "alerts.policy.update", TargetKind: "cluster", TargetID: "cluster", Outcome: "success", OccurredAt: now,
		Details: map[string]string{"revision": fmt.Sprint(value.Revision), "routeEnabled": fmt.Sprint(value.RouteEnabled), "minimumSeverity": string(value.MinimumSeverity)},
	})
	return value, err
}

// SuppressionReason never hides or resolves the underlying alert. It only
// controls delivery to the existing host-configured webhook.
func SuppressionReason(policy domain.AlertPolicy, alert domain.Alert, now time.Time) string {
	switch {
	case !policy.RouteEnabled:
		return "notification route disabled by policy"
	case policy.SilenceUntil != nil && now.Before(*policy.SilenceUntil):
		return "notification silenced by policy"
	case severity(alert.Severity) < severity(policy.MinimumSeverity):
		return "severity excluded by notification route"
	case !slices.Contains(policy.Kinds, alert.Kind):
		return "condition type excluded by notification route"
	default:
		return ""
	}
}

func severity(value domain.AlertSeverity) int {
	switch value {
	case domain.AlertSeverityInfo:
		return 0
	case domain.AlertSeverityWarning:
		return 1
	case domain.AlertSeverityCritical:
		return 2
	default:
		return -1
	}
}
