package enrollment

import (
	"context"
	"errors"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("resource already exists")
	ErrInvalidToken   = errors.New("invalid or unavailable enrollment token")
	ErrUnauthorized   = errors.New("node authentication failed")
	ErrInvalidState   = errors.New("invalid lifecycle transition")
	ErrInvalidRequest = errors.New("invalid request")
	ErrUnavailable    = errors.New("service unavailable")
)

// Repository defines the transactional state required by Phase 1 enrollment.
type Repository interface {
	CreateEnrollmentToken(context.Context, domain.EnrollmentToken) error
	EnrollNode(context.Context, string, string, domain.Node, time.Time) (domain.EnrollmentToken, error)
	GetNode(context.Context, string) (domain.Node, error)
	ListNodes(context.Context) ([]domain.Node, error)
	UpdateNode(context.Context, string, func(*domain.Node) error, *domain.AuditEvent) (domain.Node, error)
	ListAuditEvents(context.Context, int) ([]domain.AuditEvent, error)
	UpsertAssignmentReport(context.Context, domain.AssignmentReport) error
	ListAssignmentReports(context.Context, int) ([]domain.AssignmentReport, error)
	Close() error
}
