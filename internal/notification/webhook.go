package notification

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

type webhookEvent struct {
	ID         string       `json:"id"`
	Type       string       `json:"type"`
	OccurredAt time.Time    `json:"occurredAt"`
	Alert      domain.Alert `json:"alert"`
}

func (service *Service) dispatch(ctx context.Context, delivery domain.AlertDelivery) error {
	now := service.now().UTC()
	delivery.Attempts++
	delivery.LastAttemptAt = &now
	delivery.ResponseStatus = 0

	payload, err := json.Marshal(webhookEvent{ID: delivery.ID, Type: delivery.EventType, OccurredAt: delivery.CreatedAt, Alert: delivery.Alert})
	if err == nil {
		requestContext, cancel := context.WithTimeout(ctx, service.config.Timeout)
		defer cancel()
		var request *http.Request
		request, err = http.NewRequestWithContext(requestContext, http.MethodPost, service.endpoint.String(), bytes.NewReader(payload))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "XIRANG-Webhook/1")
			request.Header.Set("X-XIRANG-Event-ID", delivery.ID)
			request.Header.Set("X-XIRANG-Timestamp", delivery.CreatedAt.UTC().Format(time.RFC3339Nano))
			request.Header.Set("X-XIRANG-Signature", signature(service.config.Secret, payload))
			var response *http.Response
			response, err = service.client.Do(request)
			if response != nil {
				delivery.ResponseStatus = response.StatusCode
				if response.Body != nil {
					_, _ = io.CopyN(io.Discard, response.Body, 4096)
					_ = response.Body.Close()
				}
				if err == nil && (response.StatusCode < 200 || response.StatusCode >= 300) {
					err = fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
				}
			} else if err == nil {
				err = errors.New("webhook returned no response")
			}
		}
	}
	if err == nil {
		delivery.State = domain.AlertDeliveryDelivered
		delivery.DeliveredAt = &now
		delivery.NextAttemptAt = nil
		delivery.LastError = ""
	} else {
		delivery.LastError = service.sanitizeError(err.Error())
		delivery.DeliveredAt = nil
		if delivery.Attempts >= service.config.MaxAttempts {
			delivery.State = domain.AlertDeliveryFailed
			delivery.NextAttemptAt = nil
		} else {
			delivery.State = domain.AlertDeliveryRetrying
			next := now.Add(service.backoff(delivery.Attempts))
			delivery.NextAttemptAt = &next
		}
	}
	return service.repository.PutDelivery(ctx, delivery)
}

func (service *Service) backoff(attempt int) time.Duration {
	backoff := service.config.BaseBackoff
	for current := 1; current < attempt && backoff < service.config.MaxBackoff; current++ {
		if backoff > service.config.MaxBackoff/2 {
			return service.config.MaxBackoff
		}
		backoff *= 2
	}
	if backoff > service.config.MaxBackoff {
		return service.config.MaxBackoff
	}
	return backoff
}

func signature(secret, payload []byte) string {
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write(payload)
	return "sha256=" + hex.EncodeToString(digest.Sum(nil))
}
