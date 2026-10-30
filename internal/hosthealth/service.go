// Package hosthealth stores bounded host observations without treating a
// corrected hardware error or a recovery message as proof of root-cause repair.
package hosthealth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
)

const Capability = "host-health-v1"
const OutboxCapability = "host-event-outbox-v1"
const IdentifyCapability = "bmc-identify-v1"
const MaxEvents = 256
const MaxSensors = 256
const RetainedEvents = 2000
const Retention = 30 * 24 * time.Hour

var ErrInvalid = errors.New("invalid host health data")
var ErrNotFound = errors.New("host health record not found")
var ErrUnavailable = errors.New("host health operation unavailable")

type Repository interface {
	PutNodeHealth(context.Context, domain.NodeHealthSnapshot, []domain.SystemEvent) error
	PutSystemEvents(context.Context, string, []domain.SystemEvent) error
	GetNodeHealth(context.Context, string) (domain.NodeHealthSnapshot, error)
	ListSystemEvents(context.Context, string, string, int) ([]domain.SystemEvent, error)
	AppendSystemNote(context.Context, domain.SystemEvent, domain.AuditEvent) error
	PutBMCAction(context.Context, domain.BMCAction, domain.AuditEvent) error
	GetBMCAction(context.Context, string) (domain.BMCAction, error)
}

type Service struct{ repository Repository }

func New(repository Repository) *Service { return &Service{repository: repository} }
func (service *Service) Get(ctx context.Context, nodeID string) (domain.NodeHealthSnapshot, error) {
	return service.repository.GetNodeHealth(ctx, nodeID)
}
func (service *Service) Events(ctx context.Context, nodeID, kind string, limit int) ([]domain.SystemEvent, error) {
	if limit < 1 || limit > 200 || (kind != "" && !knownKind(kind)) {
		return nil, ErrInvalid
	}
	return service.repository.ListSystemEvents(ctx, nodeID, kind, limit)
}

func (service *Service) Report(ctx context.Context, nodeID string, report *xirangv1.HostHealthReport) error {
	if report == nil || !text(nodeID, 256) || report.ObservedAt == nil || report.ObservedAt.CheckValid() != nil || len(report.Sources) > 8 || len(report.Sensors) > MaxSensors || len(report.Events) > MaxEvents {
		return ErrInvalid
	}
	now := time.Now().UTC()
	observed := report.ObservedAt.AsTime()
	if observed.After(now.Add(5*time.Minute)) || observed.Before(now.Add(-24*time.Hour)) {
		return ErrInvalid
	}
	snapshot := domain.NodeHealthSnapshot{NodeID: nodeID, ObservedAt: observed, ReceivedAt: now, Sources: []domain.HealthSource{}, Sensors: []domain.HardwareSensor{}, ChassisPower: report.ChassisPower, IdentifySupported: report.IdentifySupported}
	if snapshot.ChassisPower != "" && snapshot.ChassisPower != "on" && snapshot.ChassisPower != "off" && snapshot.ChassisPower != "unknown" {
		return ErrInvalid
	}
	seenSources := map[string]bool{}
	for _, source := range report.Sources {
		if source == nil || !text(source.Name, 64) || !bounded(source.Message, 512) || seenSources[source.Name] || source.WindowSeconds < 0 || source.WindowSeconds > 86400 || source.Limit > MaxEvents || source.Limit < 0 {
			return ErrInvalid
		}
		if source.Status != "available" && source.Status != "partial" && source.Status != "unavailable" && source.Status != "disabled" {
			return ErrInvalid
		}
		seenSources[source.Name] = true
		snapshot.Sources = append(snapshot.Sources, domain.HealthSource{Name: source.Name, Status: source.Status, Message: source.Message, WindowSeconds: source.WindowSeconds, Limit: int(source.Limit)})
	}
	seenSensors := map[string]bool{}
	for _, sensor := range report.Sensors {
		if sensor == nil || !text(sensor.Id, 128) || !text(sensor.Name, 128) || !text(sensor.Source, 64) || !bounded(sensor.Unit, 64) || !bounded(sensor.Status, 128) || seenSensors[sensor.Id] || !finite(sensor.Value) {
			return ErrInvalid
		}
		if (sensor.LowerCritical != nil && !finite(*sensor.LowerCritical)) || (sensor.UpperCritical != nil && !finite(*sensor.UpperCritical)) {
			return ErrInvalid
		}
		seenSensors[sensor.Id] = true
		snapshot.Sensors = append(snapshot.Sensors, domain.HardwareSensor{ID: sensor.Id, Name: sensor.Name, Source: sensor.Source, Value: sensor.Value, Available: sensor.Available, Unit: sensor.Unit, Status: sensor.Status, LowerCritical: sensor.LowerCritical, UpperCritical: sensor.UpperCritical})
	}
	events, err := validatedEvents(nodeID, report.Events, now)
	if err != nil {
		return err
	}
	return service.repository.PutNodeHealth(ctx, snapshot, events)
}

func (service *Service) CommitEvents(ctx context.Context, nodeID string, batch *xirangv1.HostEventBatch) error {
	if !text(nodeID, 256) || batch == nil || !text(batch.BatchId, 128) || len(batch.Events) == 0 || len(batch.Events) > MaxEvents {
		return ErrInvalid
	}
	events, err := validatedEvents(nodeID, batch.Events, time.Now().UTC())
	if err != nil {
		return err
	}
	return service.repository.PutSystemEvents(ctx, nodeID, events)
}

func validatedEvents(nodeID string, input []*xirangv1.SystemEvent, now time.Time) ([]domain.SystemEvent, error) {
	events := make([]domain.SystemEvent, 0, len(input))
	for _, event := range input {
		if event == nil || (event.Source != "journal" && event.Source != "ipmi-sel") || !text(event.SourceRecordId, 1024) || !text(event.Message, 2048) || !bounded(event.Component, 256) || !knownKind(event.Kind) || event.Kind == "repair" || !severity(event.Severity) {
			return nil, ErrInvalid
		}
		value := domain.SystemEvent{NodeID: nodeID, Source: event.Source, SourceRecordID: event.SourceRecordId, Kind: event.Kind, Severity: event.Severity, Message: event.Message, Component: event.Component, ReceivedAt: now}
		if event.OccurredAt != nil {
			if event.OccurredAt.CheckValid() != nil || event.OccurredAt.AsTime().After(now.Add(24*time.Hour)) {
				return nil, ErrInvalid
			}
			at := event.OccurredAt.AsTime()
			value.OccurredAt = &at
		}
		digest := sha256.Sum256([]byte(nodeID + "\x00" + event.Source + "\x00" + event.SourceRecordId))
		value.ID = hex.EncodeToString(digest[:])
		events = append(events, value)
	}
	return events, nil
}

func (service *Service) AddNote(ctx context.Context, nodeID, message, related string, actor domain.Principal) (domain.SystemEvent, error) {
	if actor.Role != "admin" {
		return domain.SystemEvent{}, identity.ErrForbidden
	}
	message = strings.TrimSpace(message)
	if !text(nodeID, 256) || !text(message, 4096) || (related != "" && len(related) != 64) {
		return domain.SystemEvent{}, ErrInvalid
	}
	id, err := newID()
	if err != nil {
		return domain.SystemEvent{}, err
	}
	now := time.Now().UTC()
	event := domain.SystemEvent{ID: id, NodeID: nodeID, Source: "operator", Kind: "repair", Severity: "info", Message: message, Actor: actor.Subject, RelatedEventID: related, OccurredAt: &now, ReceivedAt: now}
	err = service.repository.AppendSystemNote(ctx, event, domain.AuditEvent{ID: "audit_" + id, ActorType: actor.AuthMethod, ActorID: actor.Subject, Action: "host.repair.record", TargetKind: "node", TargetID: nodeID, Outcome: "recorded", OccurredAt: now})
	return event, err
}

func (service *Service) RecordAction(ctx context.Context, value domain.BMCAction, actor domain.Principal) error {
	return service.repository.PutBMCAction(ctx, value, domain.AuditEvent{ID: "audit_" + value.ID + "_" + value.Status, ActorType: actor.AuthMethod, ActorID: actor.Subject, Action: "bmc." + value.Action, TargetKind: "node", TargetID: value.NodeID, Outcome: value.Status, OccurredAt: time.Now().UTC()})
}
func (service *Service) Action(ctx context.Context, id string) (domain.BMCAction, error) {
	return service.repository.GetBMCAction(ctx, id)
}
func (service *Service) ConfirmAction(ctx context.Context, nodeID, id string, accepted bool, reason string) error {
	action, err := service.repository.GetBMCAction(ctx, id)
	if err != nil {
		return err
	}
	if action.NodeID != nodeID {
		return ErrInvalid
	}
	if action.Status != "queued" {
		return nil
	}
	now := time.Now().UTC()
	action.CompletedAt = &now
	action.Status = "failed"
	if accepted {
		action.Status = "succeeded"
	}
	if !bounded(reason, 512) {
		reason = "BMC command failed"
	}
	action.Message = reason
	return service.RecordAction(ctx, action, domain.Principal{Subject: nodeID, AuthMethod: "agent"})
}

func newID() (string, error) {
	var id [32]byte
	_, err := rand.Read(id[:])
	return hex.EncodeToString(id[:]), err
}
func text(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && bounded(value, limit)
}
func bounded(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func knownKind(value string) bool {
	return value == "error" || value == "corrected" || value == "recovery" || value == "repair" || value == "info"
}
func severity(value string) bool { return value == "info" || value == "warning" || value == "critical" }
func NewAction(nodeID, action string, actor domain.Principal) (domain.BMCAction, error) {
	if actor.Role != "admin" {
		return domain.BMCAction{}, identity.ErrForbidden
	}
	if action != "identify-on" && action != "identify-off" {
		return domain.BMCAction{}, fmt.Errorf("%w: only timed locator control is supported", ErrInvalid)
	}
	id, err := newID()
	return domain.BMCAction{ID: id, NodeID: nodeID, Action: action, Actor: actor.Subject, Status: "queued", CreatedAt: time.Now().UTC()}, err
}
