package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/domain"
)

func TestPolicySuppressionIsDurableWithoutSendingAndExpiryAllowsNewEvents(t *testing.T) {
	base := time.Now().UTC()
	policy := alertpolicy.Defaults(time.Second, time.Second)
	until := base.Add(time.Hour)
	policy.SilenceUntil, policy.Revision = &until, 4
	repository := newMemoryRepository(deliveryAt("silenced", base))
	transport := &recordingTransport{status: 204}
	service, err := NewService(repository, transport, Config{URL: "https://alerts.example.test", Secret: []byte(strings.Repeat("x", 32)), Policy: func(context.Context) (domain.AlertPolicy, error) { return policy, nil }})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base }
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	suppressed := repository.deliveries["silenced"]
	if transport.request != nil || suppressed.State != domain.AlertDeliverySuppressed || suppressed.Attempts != 0 || suppressed.PolicyRevision != 4 || suppressed.NextAttemptAt != nil || suppressed.LastError == "" {
		t.Fatalf("silence was not retained without HTTP: %+v", suppressed)
	}
	if _, err := service.Retry(context.Background(), "silenced"); !errors.Is(err, ErrInvalidState) {
		t.Fatal("suppressed event replayed")
	}
	base = until.Add(time.Second)
	repository.deliveries["new"] = deliveryAt("new", base)
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repository.deliveries["new"].State != domain.AlertDeliveryDelivered || repository.deliveries["silenced"].State != domain.AlertDeliverySuppressed {
		t.Fatal("expiry did not deliver only new eligible events")
	}
}

func TestPolicyReadFailureNeverSendsAndDeliveryUsesCompletionTime(t *testing.T) {
	base := time.Now().UTC()
	repository := newMemoryRepository(deliveryAt("one", base))
	transport := &recordingTransport{status: 204}
	readErr := errors.New("policy unavailable")
	policy := alertpolicy.Defaults(time.Second, time.Second)
	policy.DeliveryTargetSeconds = 3
	service, err := NewService(repository, transport, Config{URL: "https://alerts.example.test", Secret: []byte(strings.Repeat("x", 32)), Policy: func(context.Context) (domain.AlertPolicy, error) { return policy, readErr }})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time {
		if transport.request != nil {
			return base.Add(5 * time.Second)
		}
		return base
	}
	if err := service.DispatchDue(context.Background()); err == nil || transport.request != nil {
		t.Fatal("policy failure allowed HTTP")
	}
	readErr = nil
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	delivered := repository.deliveries["one"]
	if delivered.TargetSeconds != 3 || delivered.DeliveredAt == nil || !delivered.DeliveredAt.Equal(base.Add(5*time.Second)) {
		t.Fatalf("latency excludes request duration: %+v", delivered)
	}
}

func TestServiceDeliversSignedWebhook(t *testing.T) {
	base := time.Date(2026, 7, 18, 1, 2, 3, 0, time.UTC)
	repository := newMemoryRepository(deliveryAt("delivery-1", base))
	client := &recordingTransport{status: http.StatusNoContent}
	secret := []byte("0123456789abcdef0123456789abcdef")
	service, err := NewService(repository, client, Config{URL: "https://alerts.example.test/xirang", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base.Add(time.Second) }
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	delivery := repository.deliveries["delivery-1"]
	if delivery.State != domain.AlertDeliveryDelivered || delivery.Attempts != 1 || delivery.ResponseStatus != http.StatusNoContent || delivery.DeliveredAt == nil {
		t.Fatalf("unexpected delivered state: %#v", delivery)
	}
	if client.request.Header.Get("X-XIRANG-Event-ID") != "delivery-1" || client.request.Header.Get("X-XIRANG-Timestamp") != base.Format(time.RFC3339Nano) {
		t.Fatalf("missing delivery identity headers: %#v", client.request.Header)
	}
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write(client.body)
	wantSignature := "sha256=" + hex.EncodeToString(digest.Sum(nil))
	if got := client.request.Header.Get("X-XIRANG-Signature"); got != wantSignature {
		t.Fatalf("signature = %q, want %q", got, wantSignature)
	}
	var event struct {
		ID    string       `json:"id"`
		Type  string       `json:"type"`
		Alert domain.Alert `json:"alert"`
	}
	if err := json.Unmarshal(client.body, &event); err != nil {
		t.Fatal(err)
	}
	if event.ID != "delivery-1" || event.Type != "alert.firing" || event.Alert.ID != "alert-1" {
		t.Fatalf("unexpected webhook payload: %#v", event)
	}
}

func TestServiceBacksOffFailsAndAllowsManualRetry(t *testing.T) {
	base := time.Date(2026, 7, 18, 1, 2, 3, 0, time.UTC)
	repository := newMemoryRepository(deliveryAt("delivery-1", base))
	client := &recordingTransport{status: http.StatusServiceUnavailable}
	service, err := NewService(repository, client, Config{
		URL: "https://alerts.example.test", Secret: []byte("0123456789abcdef0123456789abcdef"),
		BaseBackoff: 5 * time.Second, MaxBackoff: time.Minute, MaxAttempts: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base }
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := repository.deliveries["delivery-1"]
	if first.State != domain.AlertDeliveryRetrying || first.NextAttemptAt == nil || !first.NextAttemptAt.Equal(base.Add(5*time.Second)) {
		t.Fatalf("unexpected first failure: %#v", first)
	}
	service.now = func() time.Time { return base.Add(5 * time.Second) }
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := repository.deliveries["delivery-1"]
	if failed.State != domain.AlertDeliveryFailed || failed.Attempts != 2 || failed.NextAttemptAt != nil {
		t.Fatalf("unexpected terminal failure: %#v", failed)
	}
	retried, err := service.Retry(context.Background(), failed.ID)
	if err != nil || retried.State != domain.AlertDeliveryPending || retried.Attempts != 0 || retried.NextAttemptAt == nil {
		t.Fatalf("manual retry failed: %#v err=%v", retried, err)
	}
}

func TestServiceValidatesConfiguration(t *testing.T) {
	repository := newMemoryRepository()
	for _, test := range []Config{
		{URL: "http://alerts.example.test", Secret: []byte(strings.Repeat("x", 32))},
		{URL: "https://alerts.example.test", Secret: []byte("short")},
		{Secret: []byte(strings.Repeat("x", 32))},
	} {
		if _, err := NewService(repository, &recordingTransport{}, test); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %#v error = %v", test, err)
		}
	}
	disabled, err := NewService(repository, nil, Config{})
	if err != nil || disabled.Enabled() || disabled.Status().Destination != "" {
		t.Fatalf("disabled service = %#v err=%v", disabled, err)
	}
}

func TestServiceRedactsWebhookPathFromStoredFailure(t *testing.T) {
	base := time.Now().UTC()
	repository := newMemoryRepository(deliveryAt("delivery-1", base))
	client := &recordingTransport{err: errors.New(`Post "https://alerts.example.test/private/receiver": dial failed`)}
	service, err := NewService(repository, client, Config{
		URL: "https://alerts.example.test/private/receiver", Secret: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base }
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	lastError := repository.deliveries["delivery-1"].LastError
	if strings.Contains(lastError, "/private/receiver") || !strings.Contains(lastError, "webhook endpoint") {
		t.Fatalf("webhook URL was not redacted: %q", lastError)
	}
}

func TestServiceBoundsWebhookRequestDuration(t *testing.T) {
	base := time.Now().UTC()
	repository := newMemoryRepository(deliveryAt("delivery-1", base))
	service, err := NewService(repository, blockingTransport{}, Config{
		URL: "https://alerts.example.test", Secret: []byte("0123456789abcdef0123456789abcdef"), Timeout: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base }
	started := time.Now()
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("webhook timeout took %s", elapsed)
	}
	delivery := repository.deliveries["delivery-1"]
	if delivery.State != domain.AlertDeliveryRetrying || !strings.Contains(delivery.LastError, "deadline exceeded") {
		t.Fatalf("timeout was not retained as retryable: %#v", delivery)
	}
}

func TestServiceDoesNotFollowRedirects(t *testing.T) {
	base := time.Now().UTC()
	repository := newMemoryRepository(deliveryAt("delivery-1", base))
	transport := &redirectingTransport{}
	service, err := NewService(repository, transport, Config{
		URL: "https://alerts.example.test", Secret: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return base }
	if err := service.DispatchDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	delivery := repository.deliveries["delivery-1"]
	if transport.requests != 1 || delivery.State != domain.AlertDeliveryRetrying || delivery.ResponseStatus != http.StatusFound {
		t.Fatalf("redirect was followed: requests=%d delivery=%#v", transport.requests, delivery)
	}
}

type recordingTransport struct {
	status  int
	request *http.Request
	body    []byte
	err     error
}

type blockingTransport struct{}

func (blockingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func (client *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	client.request = request
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	client.body = body
	if client.err != nil {
		return nil, client.err
	}
	return &http.Response{StatusCode: client.status, Body: io.NopCloser(strings.NewReader("ignored response body"))}, nil
}

type redirectingTransport struct{ requests int }

func (transport *redirectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.requests++
	return &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{"https://redirected.example.test/receiver"}},
		Body:       io.NopCloser(strings.NewReader("redirect")),
	}, nil
}

type memoryRepository struct {
	deliveries map[string]domain.AlertDelivery
}

func newMemoryRepository(deliveries ...domain.AlertDelivery) *memoryRepository {
	repository := &memoryRepository{deliveries: make(map[string]domain.AlertDelivery)}
	for _, delivery := range deliveries {
		repository.deliveries[delivery.ID] = delivery
	}
	return repository
}

func (repository *memoryRepository) ListDeliveries(_ context.Context, limit int) ([]domain.AlertDelivery, error) {
	result := make([]domain.AlertDelivery, 0, len(repository.deliveries))
	for _, delivery := range repository.deliveries {
		result = append(result, delivery)
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (repository *memoryRepository) ListDueDeliveries(_ context.Context, now time.Time, _ int) ([]domain.AlertDelivery, error) {
	var result []domain.AlertDelivery
	for _, delivery := range repository.deliveries {
		if (delivery.State == domain.AlertDeliveryPending || delivery.State == domain.AlertDeliveryRetrying) &&
			(delivery.NextAttemptAt == nil || !delivery.NextAttemptAt.After(now)) {
			result = append(result, delivery)
		}
	}
	return result, nil
}

func (repository *memoryRepository) GetDelivery(_ context.Context, id string) (domain.AlertDelivery, error) {
	delivery, ok := repository.deliveries[id]
	if !ok {
		return domain.AlertDelivery{}, ErrNotFound
	}
	return delivery, nil
}

func (repository *memoryRepository) PutDelivery(_ context.Context, delivery domain.AlertDelivery) error {
	repository.deliveries[delivery.ID] = delivery
	return nil
}

func deliveryAt(id string, at time.Time) domain.AlertDelivery {
	return domain.AlertDelivery{
		ID: id, EventType: "alert.firing", AlertID: "alert-1", AlertRevision: 2,
		Alert: domain.Alert{ID: "alert-1", Kind: "interface_health", Severity: domain.AlertSeverityWarning, Status: domain.AlertStatusFiring, Revision: 2},
		State: domain.AlertDeliveryPending, CreatedAt: at, NextAttemptAt: &at,
	}
}
