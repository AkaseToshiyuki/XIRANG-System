package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

func TestAPITokenHasOneConcurrentWinnerAndStaysConsumedAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	services := []*identity.AccessService{identity.NewAccessService(store, nil, nil), identity.NewAccessService(store, nil, nil)}
	created, err := services[0].CreateToken(ctx, "one use", "viewer", time.Hour, domain.Principal{Subject: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		workers.Go(func() {
			<-start
			_, err := services[i%2].Authenticate(ctx, created.Secret)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, identity.ErrUnauthorized) {
				t.Errorf("authentication: %v", err)
			}
		})
	}
	close(start)
	workers.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful authentications = %d, want 1", successes.Load())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = boltstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	if _, err := service.Authenticate(ctx, created.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("consumed token survived restart: %v", err)
	}
	tokens, err := service.ListTokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedAt == nil {
		t.Fatalf("missing consumption record: %+v %v", tokens, err)
	}
	events, err := store.ListAuditEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	consumed := 0
	for _, event := range events {
		if event.Action == "access.api_token.consumed" {
			consumed++
		}
	}
	if consumed != 1 {
		t.Fatalf("consumption audit records = %d", consumed)
	}
}

type failingTokenUpdate struct{ identity.AccessRepository }

func (s failingTokenUpdate) UpdateAPIToken(context.Context, string, func(*domain.APIToken) error, *domain.AuditEvent) (domain.APIToken, error) {
	return domain.APIToken{}, errors.New("storage unavailable")
}

func TestAPITokenCannotAuthenticateWithoutDurableConsumption(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	created, err := service.CreateToken(ctx, "one use", "viewer", time.Hour, domain.Principal{Subject: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	broken := identity.NewAccessService(failingTokenUpdate{store}, nil, nil)
	if principal, err := broken.Authenticate(ctx, created.Secret); err == nil || principal.Subject != "" {
		t.Fatalf("issued principal without committed consumption: %+v %v", principal, err)
	}
	if _, err := service.Authenticate(ctx, created.Secret); err != nil {
		t.Fatalf("failed storage attempt burned token: %v", err)
	}
}

func TestPasswordSessionIsIndependentOfCreatedAPIToken(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	credentials := identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"}
	if _, err := service.SetupAdmin(ctx, credentials); err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(ctx, credentials.Username, credentials.Password)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(ctx, session.Secret)
	if err != nil || principal.AuthMethod != "local_password" {
		t.Fatalf("password sign-in mislabeled: %+v %v", principal, err)
	}
	created, err := service.CreateToken(ctx, "one use", "viewer", time.Hour, principal)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p, err := service.Authenticate(ctx, session.Secret)
		if err != nil || p.AuthMethod != "local_password" || p.Role != "admin" {
			t.Fatalf("creating API token changed password session: %+v %v", p, err)
		}
	}
	if p, err := service.Authenticate(ctx, created.Secret); err != nil || p.Role != "viewer" || p.AuthMethod != "api_token" {
		t.Fatalf("API token: %+v %v", p, err)
	}
	if _, err := service.Authenticate(ctx, created.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("API token reused: %v", err)
	}
	if _, err := service.ExchangeToken(ctx, session.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("browser session renewed by exchange: %v", err)
	}
}

func TestTokenExchangeRetainsPermissionAndExpiryCeilings(t *testing.T) {
	ctx := context.Background()
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	admin, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", Permissions: []string{"*"}}
	owner, err := service.Register(ctx, identity.LocalCredentials{Username: "token-owner", Password: "another sufficiently long secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateUser(ctx, owner.ID, domain.UserStatusActive, "user", actor); err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateToken(ctx, "limited", "builder", time.Hour, domain.Principal{UserID: owner.ID, Subject: owner.Username})
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.ExchangeToken(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if !session.Token.Interactive || !session.Token.ExpiresAt.Equal(created.Token.ExpiresAt) {
		t.Fatalf("invalid exchanged session: %+v", session.Token)
	}
	if _, err := service.ExchangeToken(ctx, created.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("token exchanged twice: %v", err)
	}
	if _, err := service.UpdateUser(ctx, owner.ID, "", "admin", actor); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p, err := service.Authenticate(ctx, session.Secret)
		if err != nil || p.AuthMethod != "api_token" || p.Role == "admin" {
			t.Fatalf("session identity: %+v %v", p, err)
		}
		for permission, allowed := range map[string]bool{identity.PermissionRead: true, identity.PermissionFilesWrite: true, identity.PermissionBuildsManage: false, identity.PermissionJobsManage: false, identity.PermissionAccessManage: false} {
			if got := service.Authorize(p, permission); (got == nil) != allowed {
				t.Fatalf("promotion changed permission %s: %v", permission, got)
			}
		}
	}
	if _, err := service.UpdateUser(ctx, owner.ID, domain.UserStatusDisabled, "", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, session.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("disabled owner's session accepted: %v", err)
	}
}
