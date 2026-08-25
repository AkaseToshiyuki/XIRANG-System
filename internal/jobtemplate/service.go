// Package jobtemplate manages administrator-curated XIRANG 1 job templates.
package jobtemplate

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/jobspec"
)

var (
	ErrInvalid  = errors.New("invalid job template")
	ErrNotFound = errors.New("job template not found")
	ErrConflict = errors.New("job template revision conflict")
)

const (
	MaxTemplates   = 100
	MaxNameLength  = 128
	MaxDescription = 1024
	MaxScriptBytes = 64 << 10
)

type Repository interface {
	CreateJobTemplate(context.Context, domain.JobTemplate, domain.AuditEvent) error
	GetJobTemplate(context.Context, string) (domain.JobTemplate, error)
	ListJobTemplates(context.Context) ([]domain.JobTemplate, error)
	UpdateJobTemplate(context.Context, string, uint64, domain.JobTemplate, domain.AuditEvent) (domain.JobTemplate, error)
	DeleteJobTemplate(context.Context, string, uint64, domain.AuditEvent) error
}

type Service struct {
	repository Repository
	now        func() time.Time
}

func New(repository Repository) *Service {
	return &Service{repository: repository, now: time.Now}
}

func (service *Service) List(ctx context.Context) ([]domain.JobTemplate, error) {
	templates, err := service.repository.ListJobTemplates(ctx)
	if err != nil {
		return nil, err
	}
	for index := range templates {
		templates[index].Script = withoutLegacyStorageDeclaration(templates[index].Script)
	}
	return templates, nil
}

func (service *Service) Get(ctx context.Context, id string) (domain.JobTemplate, error) {
	if strings.TrimSpace(id) == "" {
		return domain.JobTemplate{}, ErrInvalid
	}
	template, err := service.repository.GetJobTemplate(ctx, id)
	if err != nil {
		return domain.JobTemplate{}, err
	}
	template.Script = withoutLegacyStorageDeclaration(template.Script)
	return template, nil
}

func (service *Service) Create(ctx context.Context, name, description, script string, actor domain.Principal) (domain.JobTemplate, error) {
	name, description, script, err := validate(name, description, script)
	if err != nil {
		return domain.JobTemplate{}, err
	}
	existing, err := service.repository.ListJobTemplates(ctx)
	if err != nil {
		return domain.JobTemplate{}, err
	}
	if len(existing) >= MaxTemplates {
		return domain.JobTemplate{}, fmt.Errorf("%w: at most %d templates are allowed", ErrConflict, MaxTemplates)
	}
	now := service.now().UTC()
	template := domain.JobTemplate{
		ID: "template_" + randomID(), Name: name, Description: description, Script: script,
		CreatedBy: actor.Subject, UpdatedBy: actor.Subject, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	audit := auditEvent("created", template, actor, now)
	if err := service.repository.CreateJobTemplate(ctx, template, audit); err != nil {
		return domain.JobTemplate{}, err
	}
	return template, nil
}

func (service *Service) Update(ctx context.Context, id string, expectedRevision uint64, name, description, script string, actor domain.Principal) (domain.JobTemplate, error) {
	if strings.TrimSpace(id) == "" || expectedRevision == 0 {
		return domain.JobTemplate{}, ErrInvalid
	}
	name, description, script, err := validate(name, description, script)
	if err != nil {
		return domain.JobTemplate{}, err
	}
	now := service.now().UTC()
	replacement := domain.JobTemplate{
		Name: name, Description: description, Script: script,
		UpdatedBy: actor.Subject, UpdatedAt: now,
	}
	audit := domain.AuditEvent{
		ID: "job_template_updated_" + id + "_" + randomID(), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "job_template.updated", TargetKind: "job_template", TargetID: id,
		Outcome: "succeeded", OccurredAt: now,
	}
	return service.repository.UpdateJobTemplate(ctx, id, expectedRevision, replacement, audit)
}

func (service *Service) Delete(ctx context.Context, id string, expectedRevision uint64, actor domain.Principal) error {
	if strings.TrimSpace(id) == "" || expectedRevision == 0 {
		return ErrInvalid
	}
	now := service.now().UTC()
	return service.repository.DeleteJobTemplate(ctx, id, expectedRevision, domain.AuditEvent{
		ID: "job_template_deleted_" + id + "_" + randomID(), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "job_template.deleted", TargetKind: "job_template", TargetID: id,
		Outcome: "succeeded", OccurredAt: now,
	})
}

func validate(name, description, script string) (string, string, string, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	script = strings.TrimSpace(strings.ReplaceAll(script, "\r\n", "\n"))
	if name == "" || utf8.RuneCountInString(name) > MaxNameLength || utf8.RuneCountInString(description) > MaxDescription ||
		script == "" || len(script) > MaxScriptBytes || strings.ContainsRune(name, '\x00') ||
		strings.ContainsRune(description, '\x00') {
		return "", "", "", ErrInvalid
	}
	if _, err := jobspec.Parse(strings.NewReader(script)); err != nil {
		return "", "", "", fmt.Errorf("%w: script: %v", ErrInvalid, err)
	}
	return name, description, script + "\n", nil
}

func auditEvent(action string, template domain.JobTemplate, actor domain.Principal, now time.Time) domain.AuditEvent {
	return domain.AuditEvent{
		ID: "job_template_" + action + "_" + template.ID, ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "job_template." + action, TargetKind: "job_template", TargetID: template.ID,
		Outcome: "succeeded", OccurredAt: now,
	}
}

func randomID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

// Templates persisted before administrator-managed storage quotas may contain
// a storage declaration. It is removed at the read boundary so an old template
// cannot reintroduce a user-controlled storage request.
func withoutLegacyStorageDeclaration(script string) string {
	lines := strings.Split(strings.ReplaceAll(script, "\r\n", "\n"), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "storage") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}
