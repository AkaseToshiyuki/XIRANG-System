package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	boltstore "github.com/xirang-grid/xirang/internal/store/bolt"
)

type staticOIDCVerifier struct{ claims identity.OIDCClaims }

func (verifier staticOIDCVerifier) Verify(context.Context, string) (identity.OIDCClaims, error) {
	return verifier.claims, nil
}

func TestAPITokenRoleAndRevocation(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	admin := domain.Principal{Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	created, err := service.CreateToken(context.Background(), "build automation", "builder", time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(context.Background(), created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(principal, identity.PermissionBuildsManage); err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(principal, identity.PermissionNodesManage); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("builder received node permission: %v", err)
	}
	if _, err := service.RevokeToken(context.Background(), created.Token.ID, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), created.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("revoked token authenticated: %v", err)
	}
	events, err := store.ListAuditEvents(context.Background(), 10)
	if err != nil || len(events) != 2 || events[0].Action != "access.api_token.revoked" ||
		events[1].Action != "access.api_token.created" {
		t.Fatalf("unexpected API token audit trail: events=%#v err=%v", events, err)
	}
}

func TestOIDCGroupsUnionIncomparableRolePermissions(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, staticOIDCVerifier{claims: identity.OIDCClaims{
		Subject: "operator-builder", Groups: []string{"operations", "builds"},
	}}, map[string]string{"operations": "operator", "builds": "builder"})
	principal, err := service.Authenticate(context.Background(), "signed.jwt.value")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{identity.PermissionJobsManage, identity.PermissionBuildsManage} {
		if err := service.Authorize(principal, permission); err != nil {
			t.Fatalf("combined OIDC groups lost %s: %v", permission, err)
		}
	}
}

func TestLocalRegistrationRequiresApprovalAndUsesCurrentRole(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	required, err := service.SetupRequired(ctx)
	if err != nil || !required {
		t.Fatalf("expected first-run setup, required=%v err=%v", required, err)
	}
	admin, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	user, err := service.Register(ctx, identity.LocalCredentials{Username: "researcher", Password: "a sufficiently long password"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, "researcher", "a sufficiently long password"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("pending user authenticated: %v", err)
	}
	actor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}
	if _, err := service.UpdateUser(ctx, user.ID, domain.UserStatusActive, "", actor); err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(ctx, "researcher", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(ctx, session.Secret)
	if err != nil || principal.Role != "user" {
		t.Fatalf("unexpected local principal: %#v err=%v", principal, err)
	}
	if _, err := service.UpdateUser(ctx, user.ID, "", "admin", actor); err != nil {
		t.Fatal(err)
	}
	principal, err = service.Authenticate(ctx, session.Secret)
	if err != nil || principal.Role != "admin" {
		t.Fatalf("session did not use current account role: %#v err=%v", principal, err)
	}
}

func TestUserGroupTimeoutDefaultsAndUpdates(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	actor := domain.Principal{Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	group, err := service.CreateUserGroup(context.Background(), domain.UserGroup{
		Name: "research", ResourceQuota: domain.ResourceVector{MilliCPU: 1000, MemoryBytes: 1 << 30},
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if group.MaxJobTimeoutSeconds != domain.DefaultMaxJobTimeoutSeconds {
		t.Fatalf("default timeout = %d", group.MaxJobTimeoutSeconds)
	}
	group.MaxJobTimeoutSeconds = 30 * 86400
	updated, err := service.UpdateUserGroup(context.Background(), group.ID, group, actor)
	if err != nil {
		t.Fatal(err)
	}
	if updated.MaxJobTimeoutSeconds != 30*86400 {
		t.Fatalf("updated timeout = %d", updated.MaxJobTimeoutSeconds)
	}
	legacy := domain.UserGroup{ID: "group_legacy", Name: "legacy", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.CreateUserGroup(context.Background(), legacy, domain.AuditEvent{ID: "legacy_group", Action: "test", OccurredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	loaded, err := service.GetUserGroup(context.Background(), legacy.ID)
	if err != nil || loaded.MaxJobTimeoutSeconds != domain.DefaultMaxJobTimeoutSeconds {
		t.Fatalf("legacy timeout was not normalized: %#v err=%v", loaded, err)
	}
}

func TestUserGroupRemovalAtomicallyDetachesAssignedMembers(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	actor := domain.Principal{Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	admin, err := service.SetupAdmin(context.Background(), identity.LocalCredentials{Username: "root", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	group, err := service.CreateUserGroup(context.Background(), domain.UserGroup{Name: "temporary"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	survivor, err := service.CreateUserGroup(context.Background(), domain.UserGroup{Name: "survivor"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.Register(context.Background(), identity.LocalCredentials{Username: "member", Password: "another-correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutUserGroups(context.Background(), admin.ID, []string{group.ID, survivor.ID}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutUserGroups(context.Background(), member.ID, []string{group.ID}, actor); err != nil {
		t.Fatal(err)
	}
	affected, err := service.DeleteUserGroup(context.Background(), group.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 2 {
		t.Fatalf("unexpected detached users: %#v", affected)
	}
	updated, _, err := store.GetUser(context.Background(), admin.ID)
	if err != nil || len(updated.GroupIDs) != 1 || updated.GroupIDs[0] != survivor.ID {
		t.Fatalf("persisted user memberships = %v err=%v", updated.GroupIDs, err)
	}
	updatedMember, _, err := store.GetUser(context.Background(), member.ID)
	if err != nil || len(updatedMember.GroupIDs) != 0 {
		t.Fatalf("single-group member memberships = %v err=%v", updatedMember.GroupIDs, err)
	}
	if _, err := service.GetUserGroup(context.Background(), group.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("removed group lookup error = %v", err)
	}
	events, err := store.ListAuditEvents(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Action == "access.group.deleted" && event.Details["detachedUsers"] == "2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("group deletion audit missing detached member count: %#v", events)
	}
}

func TestEffectiveUserGroupPolicyUsesMostRestrictiveNonZeroLimits(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	actor := domain.Principal{Subject: "root", Role: "admin", AuthMethod: "bootstrap"}
	wide, err := service.CreateUserGroup(context.Background(), domain.UserGroup{
		Name: "wide", ResourceQuota: domain.ResourceVector{MilliCPU: 32_000, MemoryBytes: 128 << 30, Devices: map[string]int64{"gpu": 4}},
		StorageQuotaBytes: 2 << 40, MaxConcurrentJobs: 16, MaxJobTimeoutSeconds: 30 * 86400,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	strict, err := service.CreateUserGroup(context.Background(), domain.UserGroup{
		Name: "strict", ResourceQuota: domain.ResourceVector{MilliCPU: 8_000, MemoryBytes: 0, Devices: map[string]int64{"gpu": 1}},
		StorageQuotaBytes: 512 << 30, MaxConcurrentJobs: 4, MaxJobTimeoutSeconds: 7 * 86400, Disabled: true,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := service.ResolveUserGroupPolicy(context.Background(), []string{wide.ID, strict.ID, wide.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.GroupIDs) != 2 || policy.PrimaryGroupID != policy.GroupIDs[0] || !policy.Disabled ||
		policy.ResourceQuota.MilliCPU != 8_000 || policy.ResourceQuota.MemoryBytes != 128<<30 || policy.ResourceQuota.Devices["gpu"] != 1 ||
		policy.StorageQuotaBytes != 512<<30 || policy.MaxConcurrentJobs != 4 || policy.MaxJobTimeoutSeconds != 7*86400 {
		t.Fatalf("unexpected effective policy: %#v", policy)
	}
}

func TestNewAccountsReceiveConfiguredDefaultGroupAndDeletionClearsIt(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	admin, err := service.SetupAdmin(context.Background(), identity.LocalCredentials{Username: "admin", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}
	group, err := service.CreateUserGroup(context.Background(), domain.UserGroup{Name: "new-users"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := service.PutAccessSettings(context.Background(), group.ID, actor)
	if err != nil || settings.DefaultUserGroupID != group.ID {
		t.Fatalf("settings=%#v err=%v", settings, err)
	}
	member, err := service.Register(context.Background(), identity.LocalCredentials{Username: "member-one", Password: "another-correct-horse-battery-staple"})
	if err != nil || len(member.GroupIDs) != 1 || member.GroupIDs[0] != group.ID {
		t.Fatalf("registered member=%#v err=%v", member, err)
	}
	if _, err := service.DeleteUserGroup(context.Background(), group.ID, actor); err != nil {
		t.Fatal(err)
	}
	settings, err = service.GetAccessSettings(context.Background())
	if err != nil || settings.DefaultUserGroupID != "" {
		t.Fatalf("settings after group deletion=%#v err=%v", settings, err)
	}
	member, err = service.Register(context.Background(), identity.LocalCredentials{Username: "member-two", Password: "yet-another-correct-horse-battery-staple"})
	if err != nil || len(member.GroupIDs) != 0 {
		t.Fatalf("unassigned member=%#v err=%v", member, err)
	}
}

func TestRevokeGroupSessionsLeavesAutomationTokensUntouched(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	admin, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", AuthMethod: "local_password"}
	group, err := service.CreateUserGroup(ctx, domain.UserGroup{Name: "operators"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutUserGroups(ctx, admin.ID, []string{group.ID}, actor); err != nil {
		t.Fatal(err)
	}
	session, err := service.Login(ctx, admin.Username, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	automation, err := service.CreateToken(ctx, "automation", "admin", time.Hour, actor)
	if err != nil {
		t.Fatal(err)
	}
	users, count, err := service.RevokeGroupSessions(ctx, group.ID, actor)
	if err != nil || count != 1 || len(users) != 1 || users[0] != admin.ID {
		t.Fatalf("revocation users=%v count=%d err=%v", users, count, err)
	}
	if _, err := service.Authenticate(ctx, session.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("revoked browser session authenticated: %v", err)
	}
	if _, err := service.Authenticate(ctx, automation.Secret); err != nil {
		t.Fatalf("automation token was unexpectedly revoked: %v", err)
	}
}

func TestAutomationTokenTracksOwnerAccountWithoutEscalating(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	admin, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	adminActor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}

	// A deliberately narrow token minted from an administrator account must not
	// inherit that account's privileges.
	narrow, err := service.CreateToken(ctx, "read only", "viewer", time.Hour, adminActor)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(ctx, narrow.Secret)
	if err != nil || principal.Role != "viewer" {
		t.Fatalf("narrow token did not stay narrow: %#v err=%v", principal, err)
	}
	if err := service.Authorize(principal, identity.PermissionAccessManage); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("narrow token inherited administrator permissions: %v", err)
	}

	// A second administrator, so the token owner below can be demoted at all.
	owner, err := service.Register(ctx, identity.LocalCredentials{Username: "automation-owner", Password: "another sufficiently long secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateUser(ctx, owner.ID, domain.UserStatusActive, "admin", adminActor); err != nil {
		t.Fatal(err)
	}
	ownerActor := domain.Principal{UserID: owner.ID, Subject: owner.Username, Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}
	wide, err := service.CreateToken(ctx, "automation", "admin", time.Hour, ownerActor)
	if err != nil {
		t.Fatal(err)
	}
	if principal, err = service.Authenticate(ctx, wide.Secret); err != nil || principal.Role != "admin" {
		t.Fatalf("administrator token was not usable: %#v err=%v", principal, err)
	}

	// Demoting the owner narrows the token immediately, without revoking it.
	if _, err := service.UpdateUser(ctx, owner.ID, "", "user", adminActor); err != nil {
		t.Fatal(err)
	}
	principal, err = service.Authenticate(ctx, wide.Secret)
	if err != nil || principal.Role != "user" {
		t.Fatalf("demoting the owner did not narrow the token: %#v err=%v", principal, err)
	}
	if err := service.Authorize(principal, identity.PermissionAccessManage); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("token kept administrator permissions after the owner was demoted: %v", err)
	}

	// Disabling the owner stops the token from authenticating at all.
	if _, err := service.UpdateUser(ctx, owner.ID, domain.UserStatusDisabled, "", adminActor); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, wide.Secret); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("token of a disabled account still authenticated: %v", err)
	}
}

func TestAutomationTokenPermissionsIntersectCurrentAccount(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	admin, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}
	owner, err := service.Register(ctx, identity.LocalCredentials{Username: "token-owner", Password: "another sufficiently long secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		issued, current     string
		jobs, builds, files bool
	}{
		{"builder", "user", false, false, true},
		{"builder", "operator", false, false, true},
		{"user", "builder", false, false, true},
		{"admin", "builder", false, true, true},
		{"builder", "admin", false, true, true},
		{"viewer", "admin", false, false, false},
		{"admin", "viewer", false, false, false},
	} {
		t.Run(tc.issued+"_"+tc.current, func(t *testing.T) {
			if _, err := service.UpdateUser(ctx, owner.ID, domain.UserStatusActive, "admin", actor); err != nil {
				t.Fatal(err)
			}
			token, err := service.CreateToken(ctx, "automation", tc.issued, time.Hour, domain.Principal{UserID: owner.ID, Subject: owner.Username, Role: "admin", Permissions: []string{"*"}})
			if err != nil {
				t.Fatal(err)
			}
			// Account management currently exposes admin/user. Seed the other
			// built-in roles through persistence to cover legacy account state.
			if _, err := store.UpdateUser(ctx, owner.ID, func(user *domain.User) error {
				user.Role = tc.current
				return nil
			}, domain.AuditEvent{}); err != nil {
				t.Fatal(err)
			}
			principal, err := service.Authenticate(ctx, token.Secret)
			if err != nil {
				t.Fatal(err)
			}
			if principal.Role == "admin" {
				t.Fatal("restricted token retained admin ownership bypass")
			}
			for permission, allowed := range map[string]bool{identity.PermissionRead: true, identity.PermissionJobsManage: tc.jobs, identity.PermissionBuildsManage: tc.builds, identity.PermissionFilesWrite: tc.files, identity.PermissionAccessManage: false} {
				if got := service.Authorize(principal, permission); (got == nil) != allowed {
					t.Fatalf("permission %s: allowed=%v err=%v", permission, allowed, got)
				}
			}
		})
	}
}

func TestLastAdministratorCannotBeDemotedOrDisabled(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	admin, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Principal{UserID: admin.ID, Subject: admin.Username, Role: "admin", Permissions: []string{"*"}, AuthMethod: "local_password"}
	if _, err := service.UpdateUser(ctx, admin.ID, "", "user", actor); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("administrator demoted their own account: %v", err)
	}
	if _, err := service.UpdateUser(ctx, admin.ID, domain.UserStatusDisabled, "", actor); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("administrator disabled their own account: %v", err)
	}
	// A no-op reaffirmation stays allowed.
	if _, err := service.UpdateUser(ctx, admin.ID, domain.UserStatusActive, "admin", actor); err != nil {
		t.Fatalf("reaffirming an administrator was rejected: %v", err)
	}
	// The bootstrap token owns no account, so the self guard does not apply to
	// it; the last-administrator guard still does.
	bootstrap := domain.Principal{Subject: "bootstrap-admin", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	if _, err := service.UpdateUser(ctx, admin.ID, "", "user", bootstrap); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("bootstrap removed the last administrator: %v", err)
	}
	second, err := service.Register(ctx, identity.LocalCredentials{Username: "second-admin", Password: "another sufficiently long secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateUser(ctx, second.ID, domain.UserStatusActive, "admin", actor); err != nil {
		t.Fatal(err)
	}
	// With a second administrator in place the demotion is safe and permitted.
	if _, err := service.UpdateUser(ctx, admin.ID, "", "user", bootstrap); err != nil {
		t.Fatalf("demotion was refused despite a remaining administrator: %v", err)
	}
}

func TestRepeatedSignInFailuresBackOffPerAccount(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	const password = "correct horse battery staple"
	if _, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: password}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := service.Login(ctx, "cluster-admin", "not the password"); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatalf("attempt %d did not fail: %v", attempt, err)
		}
	}
	// The correct password is now refused too: the guessing loop has locked the
	// name out rather than being allowed to continue at full speed.
	if _, err := service.Login(ctx, "cluster-admin", password); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("sign-in was not throttled after repeated failures: %v", err)
	}
	// A name that never failed is unaffected, so one account cannot lock out
	// another.
	if _, err := service.Login(ctx, "someone-else", "not the password"); !errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("unexpected error for an unrelated name: %v", err)
	}
}

func TestSuccessfulSignInClearsTheBackoff(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	ctx := context.Background()
	const password = "correct horse battery staple"
	if _, err := service.SetupAdmin(ctx, identity.LocalCredentials{Username: "cluster-admin", Password: password}); err != nil {
		t.Fatal(err)
	}
	// Below the threshold a mistyped password must not penalise the next try.
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := service.Login(ctx, "cluster-admin", "not the password"); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatalf("attempt %d did not fail: %v", attempt, err)
		}
	}
	if _, err := service.Login(ctx, "cluster-admin", password); err != nil {
		t.Fatalf("a valid sign-in below the threshold was refused: %v", err)
	}
	// The counter is reset, so the allowance starts over.
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := service.Login(ctx, "cluster-admin", "not the password"); !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatalf("post-reset attempt %d did not fail: %v", attempt, err)
		}
	}
	if _, err := service.Login(ctx, "cluster-admin", password); err != nil {
		t.Fatalf("the failure counter was not cleared by a successful sign-in: %v", err)
	}
}

func TestBuiltInRolesCannotBeMutatedThroughAReturnedRole(t *testing.T) {
	store, err := boltstore.Open(filepath.Join(t.TempDir(), "xirang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := identity.NewAccessService(store, nil, nil)
	roles := service.Roles()
	for index := range roles {
		if roles[index].Name != "viewer" {
			continue
		}
		roles[index].Permissions[0] = "*"
	}
	for _, role := range service.Roles() {
		if role.Name == "viewer" && role.Permissions[0] != identity.PermissionRead {
			t.Fatalf("a caller rewrote the built-in viewer role: %#v", role.Permissions)
		}
	}
	// A principal's permissions must be detached from the shared table too.
	admin := domain.Principal{Subject: "root", Role: "admin", Permissions: []string{"*"}, AuthMethod: "bootstrap"}
	created, err := service.CreateToken(context.Background(), "read only", "viewer", time.Hour, admin)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.Authenticate(context.Background(), created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	principal.Permissions[0] = "*"
	fresh, err := service.Authenticate(context.Background(), created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Authorize(fresh, identity.PermissionAccessManage); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("mutating one principal escalated the shared viewer role: %v", err)
	}
}
