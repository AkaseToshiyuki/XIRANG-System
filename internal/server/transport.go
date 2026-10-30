package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	buildmanager "github.com/xirang-grid/xirang/internal/build"

	"github.com/xirang-grid/xirang/internal/accounting"
	"github.com/xirang-grid/xirang/internal/alerting"
	"github.com/xirang-grid/xirang/internal/alertpolicy"
	"github.com/xirang-grid/xirang/internal/backup"
	"github.com/xirang-grid/xirang/internal/branding"
	"github.com/xirang-grid/xirang/internal/clusterops"
	"github.com/xirang-grid/xirang/internal/discovery"
	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/flow"
	"github.com/xirang-grid/xirang/internal/hosthealth"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	"github.com/xirang-grid/xirang/internal/notification"
	"github.com/xirang-grid/xirang/internal/scheduler"
	"github.com/xirang-grid/xirang/internal/telemetry"
	"github.com/xirang-grid/xirang/internal/terminal"
	"github.com/xirang-grid/xirang/internal/topology"
)

type limitedTextWriter struct {
	builder strings.Builder
	limit   int
}

func (writer *limitedTextWriter) Write(value []byte) (int, error) {
	if len(value) > writer.limit-writer.builder.Len() {
		return 0, fmt.Errorf("payload exceeds %d bytes", writer.limit)
	}
	return writer.builder.Write(value)
}

func (writer *limitedTextWriter) String() string { return writer.builder.String() }

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func nodeAction(action func(context.Context, string) (domain.Node, error)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		node, err := action(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, node)
	}
}

// requireAuthenticated admits a request that already carries an authenticated
// principal, or that presents the bootstrap administrator token directly. It
// deliberately performs no permission check: per-route authorization is decided
// centrally by requiredPermission inside accessMiddleware, and ownership is
// enforced by the individual handlers.

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) error {
	defer request.Body.Close()
	reader := http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: malformed JSON: %v", enrollment.ErrInvalidRequest, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: request must contain one JSON value", enrollment.ErrInvalidRequest)
	}
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, alertpolicy.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, alertpolicy.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, hosthealth.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, hosthealth.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, hosthealth.ErrUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, branding.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, branding.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, scheduler.ErrUnsatisfiable):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, scheduler.ErrInvalidJob):
		status = http.StatusBadRequest
	case errors.Is(err, enrollment.ErrInvalidRequest), errors.Is(err, discovery.ErrInvalidRequest), errors.Is(err, buildmanager.ErrInvalidRequest), errors.Is(err, identity.ErrInvalid), errors.Is(err, backup.ErrInvalid), errors.Is(err, clusterops.ErrInvalid), errors.Is(err, jobtemplate.ErrInvalid), errors.Is(err, accounting.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, flow.ErrInvalidReport):
		status = http.StatusBadRequest
	case errors.Is(err, enrollment.ErrInvalidToken), errors.Is(err, enrollment.ErrUnauthorized), errors.Is(err, identity.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, identity.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, buildmanager.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, clusterops.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, enrollment.ErrNotFound), errors.Is(err, discovery.ErrNotFound), errors.Is(err, buildmanager.ErrNotFound), errors.Is(err, identity.ErrNotFound), errors.Is(err, backup.ErrNotFound), errors.Is(err, jobtemplate.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, telemetry.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, topology.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, alerting.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, notification.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, scheduler.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, clusterops.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, enrollment.ErrConflict), errors.Is(err, enrollment.ErrInvalidState), errors.Is(err, discovery.ErrConflict), errors.Is(err, buildmanager.ErrConflict), errors.Is(err, identity.ErrConflict), errors.Is(err, alerting.ErrInvalidState), errors.Is(err, notification.ErrInvalidState), errors.Is(err, jobtemplate.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, scheduler.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, scheduler.ErrQuotaExceeded):
		status = http.StatusConflict
	case errors.Is(err, enrollment.ErrUnavailable), errors.Is(err, discovery.ErrDisabled), errors.Is(err, buildmanager.ErrUnavailable), errors.Is(err, terminal.ErrUnavailable):
		status = http.StatusServiceUnavailable
	}
	message := http.StatusText(status)
	if status < 500 {
		message = err.Error()
	}
	var unsatisfiable *scheduler.UnsatisfiableError
	if errors.As(err, &unsatisfiable) {
		writeJSON(writer, status, map[string]any{"error": message, "reason": unsatisfiable.Reason})
		return
	}
	writeJSON(writer, status, map[string]string{"error": message})
}

func formatPolicyDuration(seconds int64) string {
	if seconds > 0 && seconds%(24*60*60) == 0 {
		return strconv.FormatInt(seconds/(24*60*60), 10) + "d"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func randomIdentifier() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}
