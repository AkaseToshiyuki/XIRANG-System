// Package branding manages the cluster's public display identity.
package branding

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
)

const MaxLogoBytes = 512 << 10
const MaxLogoDimension = 2048

var ErrInvalid = errors.New("invalid cluster branding")
var ErrConflict = errors.New("cluster branding changed; reload before saving")

type Repository interface {
	GetClusterBranding(context.Context) (domain.ClusterBranding, error)
	PutClusterBranding(context.Context, uint64, domain.ClusterBranding, domain.AuditEvent) error
}

type Service struct{ repository Repository }

func New(repository Repository) *Service { return &Service{repository: repository} }

func (service *Service) Get(ctx context.Context) (domain.ClusterBranding, error) {
	return service.repository.GetClusterBranding(ctx)
}

func (service *Service) Update(ctx context.Context, input domain.ClusterBranding, actor domain.Principal) (domain.ClusterBranding, error) {
	if actor.Role != "admin" {
		return domain.ClusterBranding{}, identity.ErrForbidden
	}
	input.Name = strings.TrimSpace(input.Name)
	if !utf8.ValidString(input.Name) || utf8.RuneCountInString(input.Name) < 1 || utf8.RuneCountInString(input.Name) > 64 || strings.ContainsFunc(input.Name, unicode.IsControl) {
		return domain.ClusterBranding{}, fmt.Errorf("%w: name must contain 1–64 characters without control characters", ErrInvalid)
	}
	logo, err := normalizeLogo(input.LogoDataURL)
	if err != nil {
		return domain.ClusterBranding{}, err
	}
	expected := input.Revision
	if expected == ^uint64(0) {
		return domain.ClusterBranding{}, ErrInvalid
	}
	input.LogoDataURL, input.Revision, input.UpdatedAt = logo, expected+1, time.Now().UTC()
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return domain.ClusterBranding{}, err
	}
	err = service.repository.PutClusterBranding(ctx, expected, input, domain.AuditEvent{
		ID: "audit_" + hex.EncodeToString(id[:]), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "cluster.branding.update", TargetKind: "cluster", TargetID: "cluster", Outcome: "success",
		OccurredAt: input.UpdatedAt, Details: map[string]string{"name": input.Name, "customLogo": fmt.Sprint(logo != "")},
	})
	return input, err
}

func normalizeLogo(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	invalid := func(reason string) (string, error) { return "", fmt.Errorf("%w: %s", ErrInvalid, reason) }
	header, data, ok := strings.Cut(value, ",")
	if !ok || (header != "data:image/png;base64" && header != "data:image/jpeg;base64") {
		return invalid("logo must be a PNG or JPEG data URL")
	}
	if len(data) > base64.StdEncoding.EncodedLen(MaxLogoBytes) {
		return invalid("logo exceeds 512 KiB")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil || len(decoded) > MaxLogoBytes {
		return invalid("invalid or oversized logo")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || (format != "png" && format != "jpeg") || header != "data:image/"+format+";base64" || config.Width < 1 || config.Height < 1 || config.Width > MaxLogoDimension || config.Height > MaxLogoDimension {
		return invalid("logo must be a valid PNG or JPEG no larger than 2048 × 2048 pixels")
	}
	img, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		return invalid("unable to decode logo")
	}
	// Re-encode pixels: strip metadata and trailing bytes instead of storing
	// arbitrary uploaded content on a public endpoint.
	var normalized bytes.Buffer
	if format == "png" {
		err = png.Encode(&normalized, img)
	} else {
		err = jpeg.Encode(&normalized, img, &jpeg.Options{Quality: 90})
	}
	if err != nil || normalized.Len() > MaxLogoBytes {
		return invalid("normalized logo exceeds 512 KiB; resize the image")
	}
	return header + "," + base64.StdEncoding.EncodeToString(normalized.Bytes()), nil
}
