// Package notification delivers durable alert transition events to an
// operator-configured HTTPS webhook.
package notification

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/domain"
)

var (
	ErrNotFound      = errors.New("alert delivery not found")
	ErrInvalidState  = errors.New("invalid alert delivery state")
	ErrInvalidConfig = errors.New("invalid alert webhook configuration")
)

type Repository interface {
	ListDeliveries(context.Context, int) ([]domain.AlertDelivery, error)
	ListDueDeliveries(context.Context, time.Time, int) ([]domain.AlertDelivery, error)
	GetDelivery(context.Context, string) (domain.AlertDelivery, error)
	PutDelivery(context.Context, domain.AlertDelivery) error
}

type Config struct {
	URL          string
	Secret       []byte
	Timeout      time.Duration
	PollInterval time.Duration
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	MaxAttempts  int
	BatchSize    int
	Policy       func(context.Context) (domain.AlertPolicy, error)
}

type Service struct {
	repository  Repository
	client      *http.Client
	config      Config
	endpoint    *url.URL
	destination string
	now         func() time.Time
}

func NewService(repository Repository, transport http.RoundTripper, config Config) (*Service, error) {
	service := &Service{repository: repository, config: config, now: time.Now}
	if config.URL == "" {
		if len(config.Secret) != 0 {
			return nil, errors.Join(ErrInvalidConfig, errors.New("secret requires a webhook URL"))
		}
		return service, nil
	}
	endpoint, err := url.Parse(config.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, errors.Join(ErrInvalidConfig, errors.New("webhook URL must be an HTTPS URL without user information or a fragment"))
	}
	if len(config.Secret) < 32 {
		return nil, errors.Join(ErrInvalidConfig, errors.New("webhook secret must contain at least 32 bytes"))
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.BaseBackoff <= 0 {
		config.BaseBackoff = 5 * time.Second
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = 15 * time.Minute
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 8
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 32
	}
	service.config = config
	service.config.Secret = append([]byte(nil), config.Secret...)
	service.endpoint = endpoint
	service.destination = endpoint.Host
	if transport == nil {
		transport = http.DefaultTransport
	}
	service.client = &http.Client{
		Transport: transport,
		Timeout:   config.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return service, nil
}

func (service *Service) Enabled() bool { return service.endpoint != nil }

func (service *Service) Status() domain.AlertWebhookStatus {
	return domain.AlertWebhookStatus{Enabled: service.Enabled(), Destination: service.destination}
}

func (service *Service) List(ctx context.Context, limit int) ([]domain.AlertDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	return service.repository.ListDeliveries(ctx, limit)
}

func (service *Service) Retry(ctx context.Context, id string) (domain.AlertDelivery, error) {
	delivery, err := service.repository.GetDelivery(ctx, id)
	if err != nil {
		return domain.AlertDelivery{}, err
	}
	if !service.Enabled() || delivery.State != domain.AlertDeliveryFailed {
		return domain.AlertDelivery{}, ErrInvalidState
	}
	now := service.now().UTC()
	delivery.State = domain.AlertDeliveryPending
	delivery.Attempts = 0
	delivery.ResponseStatus = 0
	delivery.LastError = ""
	delivery.NextAttemptAt = &now
	delivery.LastAttemptAt = nil
	delivery.DeliveredAt = nil
	if err := service.repository.PutDelivery(ctx, delivery); err != nil {
		return domain.AlertDelivery{}, err
	}
	return delivery, nil
}

func (service *Service) Run(ctx context.Context, onError func(error)) {
	if !service.Enabled() {
		return
	}
	dispatch := func() {
		if err := service.DispatchDue(ctx); err != nil && onError != nil && !errors.Is(err, context.Canceled) {
			onError(err)
		}
	}
	dispatch()
	ticker := time.NewTicker(service.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dispatch()
		}
	}
}

func (service *Service) DispatchDue(ctx context.Context) error {
	if !service.Enabled() {
		return nil
	}
	deliveries, err := service.repository.ListDueDeliveries(ctx, service.now().UTC(), service.config.BatchSize)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if service.config.Policy != nil {
			policy, err := service.config.Policy(ctx)
			if err != nil {
				return err
			}
			delivery.PolicyRevision = policy.Revision
			if delivery.TargetSeconds == 0 {
				delivery.TargetSeconds = policy.DeliveryTargetSeconds
			}
			if reason := alertpolicy.SuppressionReason(policy, delivery.Alert, service.now().UTC()); reason != "" {
				delivery.State, delivery.LastError, delivery.NextAttemptAt = domain.AlertDeliverySuppressed, reason, nil
				if err := service.repository.PutDelivery(ctx, delivery); err != nil {
					return err
				}
				continue
			}
		}
		if err := service.dispatch(ctx, delivery); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) sanitizeError(value string) string {
	if service.endpoint != nil {
		value = strings.ReplaceAll(value, service.endpoint.String(), "webhook endpoint")
	}
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}
