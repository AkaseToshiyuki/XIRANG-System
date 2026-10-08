package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"golang.org/x/crypto/argon2"
)

var (
	ErrUnauthorized = errors.New("identity authentication failed")
	ErrForbidden    = errors.New("permission denied")
	ErrInvalid      = errors.New("invalid access request")
	ErrNotFound     = errors.New("access resource not found")
	ErrConflict     = errors.New("access resource conflict")
)

const (
	PermissionRead         = "cluster.read"
	PermissionJobsManage   = "jobs.manage"
	PermissionJobTemplates = "job_templates.manage"
	PermissionNodesManage  = "nodes.manage"
	PermissionQueuesManage = "queues.manage"
	PermissionBuildsManage = "builds.manage"
	PermissionFilesWrite   = "shared_files.write"
	PermissionAccessManage = "access.manage"
	PermissionAuditRead    = "audit.read"
)

var builtInRoles = []domain.Role{
	{Name: "viewer", Description: "Read cluster and node state", Permissions: []string{PermissionRead}},
	{Name: "user", Description: "Read cluster state and manage only owned jobs and files", Permissions: []string{PermissionRead, PermissionJobsManage, PermissionFilesWrite}},
	{Name: "operator", Description: "Read cluster state and manage only owned jobs and files", Permissions: []string{PermissionRead, PermissionJobsManage, PermissionFilesWrite}},
	{Name: "builder", Description: "Read cluster state and submit native builds", Permissions: []string{PermissionRead, PermissionBuildsManage, PermissionFilesWrite}},
	{Name: "admin", Description: "Full control-plane administration", Permissions: []string{"*"}},
}

type AccessRepository interface {
	CreateAPIToken(context.Context, domain.APIToken, [sha256.Size]byte, domain.AuditEvent) error
	GetAPITokenByDigest(context.Context, [sha256.Size]byte) (domain.APIToken, error)
	ListAPITokens(context.Context) ([]domain.APIToken, error)
	UpdateAPIToken(context.Context, string, func(*domain.APIToken) error, *domain.AuditEvent) (domain.APIToken, error)
}

type AccountRepository interface {
	HasUsers(context.Context) (bool, error)
	CreateFirstUser(context.Context, domain.User, string, domain.AuditEvent) error
	CreateUser(context.Context, domain.User, string, domain.AuditEvent) (domain.User, error)
	GetUser(context.Context, string) (domain.User, string, error)
	GetUserByUsername(context.Context, string) (domain.User, string, error)
	ListUsers(context.Context) ([]domain.User, error)
	UpdateUser(context.Context, string, func(*domain.User) error, domain.AuditEvent) (domain.User, error)
	CreateUserGroup(context.Context, domain.UserGroup, domain.AuditEvent) error
	GetUserGroup(context.Context, string) (domain.UserGroup, error)
	ListUserGroups(context.Context) ([]domain.UserGroup, error)
	UpdateUserGroup(context.Context, string, func(*domain.UserGroup) error, domain.AuditEvent) (domain.UserGroup, error)
	DeleteUserGroup(context.Context, string, domain.AuditEvent) ([]domain.User, error)
	GetAccessSettings(context.Context) (domain.AccessSettings, error)
	PutAccessSettings(context.Context, domain.AccessSettings, domain.AuditEvent) error
}

type OIDCVerifier interface {
	Verify(context.Context, string) (OIDCClaims, error)
}

type OIDCClaims struct {
	Subject string
	Name    string
	Groups  []string
}

type AccessService struct {
	repository    AccessRepository
	accounts      AccountRepository
	oidc          OIDCVerifier
	groupRoles    map[string]string
	now           func() time.Time
	passwordSlots chan struct{}
	loginMu       sync.Mutex
	loginFailures map[string]loginAttemptState
}

// loginAttemptState backs a per-account sign-in backoff. Rate limiting by peer
// address alone does not slow an attacker who rotates source addresses, and the
// Argon2 concurrency limit turns a guessing loop into a denial of service
// against everyone else's sign-in rather than a defence.
type loginAttemptState struct {
	failures  int
	notBefore time.Time
}

const (
	loginFailureThreshold = 5
	loginBackoffBase      = 2 * time.Second
	loginBackoffMaximum   = 15 * time.Minute
	loginFailureCapacity  = 4096
)

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal domain.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (domain.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(domain.Principal)
	return principal, ok
}

type CreatedAPIToken struct {
	Token  domain.APIToken `json:"token"`
	Secret string          `json:"secret"`
}

// EffectiveGroupPolicy is the deterministic, most-restrictive policy produced
// from every group assigned to a user. Zero quota values remain unlimited and
// therefore do not override a finite limit from another group.
type EffectiveGroupPolicy struct {
	PrimaryGroupID       string
	GroupIDs             []string
	ResourceQuota        domain.ResourceVector
	StorageQuotaBytes    int64
	MaxConcurrentJobs    int32
	MaxJobTimeoutSeconds int64
	Disabled             bool
}

func NewAccessService(repository AccessRepository, oidc OIDCVerifier, groupRoles map[string]string) *AccessService {
	cloned := make(map[string]string, len(groupRoles))
	for group, role := range groupRoles {
		cloned[group] = role
	}
	accounts, _ := repository.(AccountRepository)
	return &AccessService{
		repository: repository, accounts: accounts, oidc: oidc, groupRoles: cloned,
		now: time.Now, passwordSlots: make(chan struct{}, 4),
		loginFailures: map[string]loginAttemptState{},
	}
}

func (service *AccessService) Roles() []domain.Role {
	roles := make([]domain.Role, 0, len(builtInRoles))
	for index := range builtInRoles {
		roles = append(roles, cloneRole(builtInRoles[index]))
	}
	return roles
}

// cloneRole detaches a role from the package-level table. Copying the struct
// alone leaves Permissions aliasing the shared backing array, which would let
// any caller — including every Principal handed a role's permission slice —
// rewrite the permissions of a built-in role for the whole process.
func cloneRole(role domain.Role) domain.Role {
	role.Permissions = append([]string(nil), role.Permissions...)
	return role
}

func (service *AccessService) CreateToken(ctx context.Context, name, role string, ttl time.Duration, actor domain.Principal) (CreatedAPIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 || roleByName(role) == nil || ttl <= 0 || ttl > 365*24*time.Hour {
		return CreatedAPIToken{}, ErrInvalid
	}
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return CreatedAPIToken{}, err
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return CreatedAPIToken{}, err
	}
	secret := "xat." + base64.RawURLEncoding.EncodeToString(idBytes) + "." + base64.RawURLEncoding.EncodeToString(randomBytes)
	now := service.now().UTC()
	// Binding the owning account lets Authenticate refuse the token once that
	// account is disabled or demoted, instead of leaving it valid until its
	// natural expiry. A bootstrap actor has no account and stays unbound.
	token := domain.APIToken{
		ID: "api_" + base64.RawURLEncoding.EncodeToString(idBytes), Name: name, Role: role,
		Subject: actor.Subject, UserID: actor.UserID, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := service.repository.CreateAPIToken(ctx, token, sha256.Sum256([]byte(secret)), domain.AuditEvent{
		ID: "api_token_created_" + token.ID, ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.api_token.created", TargetKind: "api_token", TargetID: token.ID,
		Outcome: "succeeded", OccurredAt: now,
	}); err != nil {
		return CreatedAPIToken{}, err
	}
	return CreatedAPIToken{Token: token, Secret: secret}, nil
}

type LocalCredentials struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
}

func (service *AccessService) SetupRequired(ctx context.Context) (bool, error) {
	if service.accounts == nil {
		return false, nil
	}
	hasUsers, err := service.accounts.HasUsers(ctx)
	return !hasUsers, err
}

func (service *AccessService) SetupAdmin(ctx context.Context, credentials LocalCredentials) (domain.User, error) {
	if service.accounts == nil {
		return domain.User{}, ErrInvalid
	}
	user, hash, err := service.newLocalUser(ctx, credentials, "admin", domain.UserStatusActive)
	if err != nil {
		return domain.User{}, err
	}
	now := service.now().UTC()
	user.ApprovedAt, user.ApprovedBy = &now, user.ID
	err = service.accounts.CreateFirstUser(ctx, user, hash, domain.AuditEvent{
		ID: "first_admin_created_" + user.ID, ActorType: "local_setup", ActorID: user.ID,
		Action: "access.user.first_admin_created", TargetKind: "user", TargetID: user.ID,
		Outcome: "succeeded", OccurredAt: now,
	})
	return user, err
}

func (service *AccessService) Register(ctx context.Context, credentials LocalCredentials) (domain.User, error) {
	if service.accounts == nil {
		return domain.User{}, ErrInvalid
	}
	setup, err := service.SetupRequired(ctx)
	if err != nil || setup {
		return domain.User{}, ErrForbidden
	}
	user, hash, err := service.newLocalUser(ctx, credentials, "user", domain.UserStatusPending)
	if err != nil {
		return domain.User{}, err
	}
	user, err = service.accounts.CreateUser(ctx, user, hash, domain.AuditEvent{
		ID: "user_registered_" + user.ID, ActorType: "local_registration", ActorID: user.ID,
		Action: "access.user.registered", TargetKind: "user", TargetID: user.ID,
		Outcome: "succeeded", OccurredAt: user.CreatedAt,
	})
	return user, err
}

func (service *AccessService) GetAccessSettings(ctx context.Context) (domain.AccessSettings, error) {
	if service.accounts == nil {
		return domain.AccessSettings{}, ErrInvalid
	}
	return service.accounts.GetAccessSettings(ctx)
}

func (service *AccessService) PutAccessSettings(ctx context.Context, defaultGroupID string, actor domain.Principal) (domain.AccessSettings, error) {
	if service.accounts == nil {
		return domain.AccessSettings{}, ErrInvalid
	}
	defaultGroupID = strings.TrimSpace(defaultGroupID)
	if defaultGroupID != "" {
		if _, err := service.accounts.GetUserGroup(ctx, defaultGroupID); err != nil {
			return domain.AccessSettings{}, err
		}
	}
	now := service.now().UTC()
	settings := domain.AccessSettings{DefaultUserGroupID: defaultGroupID, UpdatedAt: now}
	err := service.accounts.PutAccessSettings(ctx, settings, domain.AuditEvent{
		ID: "access_settings_updated_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.settings.updated", TargetKind: "access_settings", TargetID: "cluster", Outcome: "succeeded", OccurredAt: now,
		Details: map[string]string{"defaultUserGroupId": defaultGroupID},
	})
	return settings, err
}

func (service *AccessService) Login(ctx context.Context, username, password string) (CreatedAPIToken, error) {
	if service.accounts == nil {
		return CreatedAPIToken{}, ErrUnauthorized
	}
	// The backoff is keyed by the submitted name, whether or not an account
	// exists under it, so a locked-out name reveals nothing about who is real.
	attemptKey := strings.ToLower(strings.TrimSpace(username))
	if err := service.checkLoginBackoff(attemptKey); err != nil {
		return CreatedAPIToken{}, err
	}
	select {
	case service.passwordSlots <- struct{}{}:
		defer func() { <-service.passwordSlots }()
	case <-ctx.Done():
		return CreatedAPIToken{}, ErrUnauthorized
	}
	user, passwordHash, err := service.accounts.GetUserByUsername(ctx, username)
	if err != nil {
		passwordHash = "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	passwordValid := verifyPassword(password, passwordHash)
	if err != nil || !passwordValid || user.Status != domain.UserStatusActive {
		service.recordLoginFailure(attemptKey)
		return CreatedAPIToken{}, ErrUnauthorized
	}
	service.clearLoginFailures(attemptKey)
	randomBytes, idBytes := make([]byte, 32), make([]byte, 12)
	if _, err := rand.Read(randomBytes); err != nil {
		return CreatedAPIToken{}, err
	}
	if _, err := rand.Read(idBytes); err != nil {
		return CreatedAPIToken{}, err
	}
	secret := "xat." + base64.RawURLEncoding.EncodeToString(idBytes) + "." + base64.RawURLEncoding.EncodeToString(randomBytes)
	now := service.now().UTC()
	token := domain.APIToken{
		ID: "session_" + base64.RawURLEncoding.EncodeToString(idBytes), Name: "web session",
		Role: user.Role, Subject: user.Username, UserID: user.ID, CreatedAt: now, ExpiresAt: now.Add(12 * time.Hour),
		Interactive: true,
	}
	if err := service.repository.CreateAPIToken(ctx, token, sha256.Sum256([]byte(secret)), domain.AuditEvent{
		ID: "user_login_" + token.ID, ActorType: "local_password", ActorID: user.ID,
		Action: "access.user.logged_in", TargetKind: "user", TargetID: user.ID, Outcome: "succeeded", OccurredAt: now,
	}); err != nil {
		return CreatedAPIToken{}, err
	}
	return CreatedAPIToken{Token: token, Secret: secret}, nil
}

// checkLoginBackoff refuses a sign-in that arrives inside the penalty window
// earned by earlier failures for the same name.
func (service *AccessService) checkLoginBackoff(key string) error {
	if key == "" {
		return ErrUnauthorized
	}
	service.loginMu.Lock()
	defer service.loginMu.Unlock()
	if service.now().Before(service.loginFailures[key].notBefore) {
		return fmt.Errorf("%w: too many failed sign-in attempts, retry later", ErrUnauthorized)
	}
	return nil
}

// recordLoginFailure lengthens the penalty window for a name, doubling it on
// each failure past the threshold up to a ceiling.
func (service *AccessService) recordLoginFailure(key string) {
	if key == "" {
		return
	}
	now := service.now()
	service.loginMu.Lock()
	defer service.loginMu.Unlock()
	// Names whose penalty has already elapsed hold no useful state, so
	// collecting them keeps an attacker from growing the map with invented
	// names. The capacity check is the backstop when they all remain active.
	if len(service.loginFailures) >= loginFailureCapacity {
		for name, state := range service.loginFailures {
			if !now.Before(state.notBefore) {
				delete(service.loginFailures, name)
			}
		}
	}
	if len(service.loginFailures) >= loginFailureCapacity {
		if _, tracked := service.loginFailures[key]; !tracked {
			return
		}
	}
	state := service.loginFailures[key]
	state.failures++
	if state.failures >= loginFailureThreshold {
		penalty := loginBackoffBase << min(state.failures-loginFailureThreshold, 20)
		if penalty > loginBackoffMaximum || penalty <= 0 {
			penalty = loginBackoffMaximum
		}
		state.notBefore = now.Add(penalty)
	}
	service.loginFailures[key] = state
}

func (service *AccessService) clearLoginFailures(key string) {
	service.loginMu.Lock()
	defer service.loginMu.Unlock()
	delete(service.loginFailures, key)
}

func (service *AccessService) ListUsers(ctx context.Context) ([]domain.User, error) {
	if service.accounts == nil {
		return nil, ErrInvalid
	}
	return service.accounts.ListUsers(ctx)
}

func (service *AccessService) UpdateUser(ctx context.Context, id string, status domain.UserStatus, role string, actor domain.Principal) (domain.User, error) {
	if service.accounts == nil || (status != "" && status != domain.UserStatusActive && status != domain.UserStatusRejected && status != domain.UserStatusDisabled) ||
		(role != "" && role != "user" && role != "admin") {
		return domain.User{}, ErrInvalid
	}
	// Only an administrator reaches this call, so demoting another account can
	// never remove the last administrator. Self-demotion and self-disabling
	// can, and are the one-click path to locking every operator out of the
	// cluster. A bootstrap actor owns no account, so it keeps that escape
	// hatch and can always restore an administrator.
	demotes := role != "" && role != "admin"
	deactivates := status != "" && status != domain.UserStatusActive
	if actor.UserID != "" && actor.UserID == id && (demotes || deactivates) {
		return domain.User{}, fmt.Errorf("%w: an administrator cannot demote or disable their own account", ErrForbidden)
	}
	if demotes || deactivates {
		if err := service.ensureAnotherAdministratorRemains(ctx, id); err != nil {
			return domain.User{}, err
		}
	}
	now := service.now().UTC()
	return service.accounts.UpdateUser(ctx, id, func(user *domain.User) error {
		if status != "" {
			user.Status = status
			if status == domain.UserStatusActive {
				user.ApprovedAt, user.ApprovedBy = &now, actor.Subject
			}
		}
		if role != "" {
			user.Role = role
		}
		user.UpdatedAt = now
		return nil
	}, domain.AuditEvent{
		ID: "user_updated_" + id + "_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.user.updated", TargetKind: "user", TargetID: id, Outcome: "succeeded", OccurredAt: now,
	})
}

// ensureAnotherAdministratorRemains refuses a change that would leave the
// cluster with no active administrator account. It is a safety net rather than
// a transactional invariant: two administrators demoting each other at the same
// instant can still race, which is why UpdateUser also forbids self-demotion.
func (service *AccessService) ensureAnotherAdministratorRemains(ctx context.Context, excludedID string) error {
	users, err := service.accounts.ListUsers(ctx)
	if err != nil {
		return err
	}
	target, found := domain.User{}, false
	for _, user := range users {
		if user.ID == excludedID {
			target, found = user, true
			break
		}
	}
	if !found || target.Role != "admin" || target.Status != domain.UserStatusActive {
		return nil
	}
	for _, user := range users {
		if user.ID != excludedID && user.Role == "admin" && user.Status == domain.UserStatusActive {
			return nil
		}
	}
	return fmt.Errorf("%w: the last active administrator cannot be demoted or disabled", ErrForbidden)
}

func (service *AccessService) PutUserGroups(ctx context.Context, id string, groupIDs []string, actor domain.Principal) (domain.User, error) {
	if service.accounts == nil || len(groupIDs) > 32 {
		return domain.User{}, ErrInvalid
	}
	unique := make(map[string]struct{}, len(groupIDs))
	for _, groupID := range groupIDs {
		groupID = strings.TrimSpace(groupID)
		if groupID == "" {
			return domain.User{}, ErrInvalid
		}
		if _, err := service.accounts.GetUserGroup(ctx, groupID); err != nil {
			return domain.User{}, err
		}
		unique[groupID] = struct{}{}
	}
	normalized := make([]string, 0, len(unique))
	for groupID := range unique {
		normalized = append(normalized, groupID)
	}
	sort.Strings(normalized)
	now := service.now().UTC()
	return service.accounts.UpdateUser(ctx, id, func(user *domain.User) error {
		user.GroupIDs, user.UpdatedAt = normalized, now
		return nil
	}, domain.AuditEvent{
		ID: "user_groups_updated_" + id + "_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.user.groups_updated", TargetKind: "user", TargetID: id, Outcome: "succeeded", OccurredAt: now,
	})
}

func (service *AccessService) CreateUserGroup(ctx context.Context, group domain.UserGroup, actor domain.Principal) (domain.UserGroup, error) {
	if service.accounts == nil {
		return domain.UserGroup{}, ErrInvalid
	}
	group.Name = strings.TrimSpace(group.Name)
	if group.Name == "" || len(group.Name) > 128 || group.ResourceQuota.Validate() != nil ||
		group.StorageQuotaBytes < 0 || group.MaxConcurrentJobs < 0 || group.MaxJobTimeoutSeconds < 0 ||
		group.MaxJobTimeoutSeconds > domain.MaxSupportedJobTimeoutSeconds {
		return domain.UserGroup{}, ErrInvalid
	}
	group.MaxJobTimeoutSeconds = normalizedMaxJobTimeout(group.MaxJobTimeoutSeconds)
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return domain.UserGroup{}, err
	}
	now := service.now().UTC()
	group.ID, group.CreatedAt, group.UpdatedAt = "group_"+base64.RawURLEncoding.EncodeToString(idBytes), now, now
	err := service.accounts.CreateUserGroup(ctx, group, domain.AuditEvent{
		ID: "group_created_" + group.ID, ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.group.created", TargetKind: "user_group", TargetID: group.ID, Outcome: "succeeded", OccurredAt: now,
	})
	return group, err
}

func (service *AccessService) GetUserGroup(ctx context.Context, id string) (domain.UserGroup, error) {
	if service.accounts == nil {
		return domain.UserGroup{}, ErrInvalid
	}
	group, err := service.accounts.GetUserGroup(ctx, id)
	if err == nil {
		group.MaxJobTimeoutSeconds = normalizedMaxJobTimeout(group.MaxJobTimeoutSeconds)
	}
	return group, err
}

func (service *AccessService) ListUserGroups(ctx context.Context) ([]domain.UserGroup, error) {
	if service.accounts == nil {
		return nil, ErrInvalid
	}
	groups, err := service.accounts.ListUserGroups(ctx)
	for index := range groups {
		groups[index].MaxJobTimeoutSeconds = normalizedMaxJobTimeout(groups[index].MaxJobTimeoutSeconds)
	}
	return groups, err
}

func (service *AccessService) ResolveUserGroupPolicy(ctx context.Context, groupIDs []string) (EffectiveGroupPolicy, error) {
	if service.accounts == nil || len(groupIDs) > 32 {
		return EffectiveGroupPolicy{}, ErrInvalid
	}
	unique := make(map[string]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			unique[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	policy := EffectiveGroupPolicy{GroupIDs: ids, ResourceQuota: domain.ResourceVector{Devices: map[string]int64{}}}
	if len(ids) > 0 {
		policy.PrimaryGroupID = ids[0]
	}
	for _, id := range ids {
		group, err := service.GetUserGroup(ctx, id)
		if err != nil {
			return EffectiveGroupPolicy{}, err
		}
		policy.Disabled = policy.Disabled || group.Disabled
		policy.ResourceQuota.MilliCPU = minimumPositive(policy.ResourceQuota.MilliCPU, group.ResourceQuota.MilliCPU)
		policy.ResourceQuota.MemoryBytes = minimumPositive(policy.ResourceQuota.MemoryBytes, group.ResourceQuota.MemoryBytes)
		policy.ResourceQuota.EphemeralStorageBytes = minimumPositive(policy.ResourceQuota.EphemeralStorageBytes, group.ResourceQuota.EphemeralStorageBytes)
		for name, quantity := range group.ResourceQuota.Devices {
			policy.ResourceQuota.Devices[name] = minimumPositive(policy.ResourceQuota.Devices[name], quantity)
		}
		policy.StorageQuotaBytes = minimumPositive(policy.StorageQuotaBytes, group.StorageQuotaBytes)
		policy.MaxConcurrentJobs = int32(minimumPositive(int64(policy.MaxConcurrentJobs), int64(group.MaxConcurrentJobs)))
		policy.MaxJobTimeoutSeconds = minimumPositive(policy.MaxJobTimeoutSeconds, group.MaxJobTimeoutSeconds)
	}
	return policy, nil
}

func minimumPositive(current, candidate int64) int64 {
	if candidate <= 0 || (current > 0 && current <= candidate) {
		return current
	}
	return candidate
}

func (service *AccessService) UpdateUserGroup(ctx context.Context, id string, group domain.UserGroup, actor domain.Principal) (domain.UserGroup, error) {
	if service.accounts == nil || strings.TrimSpace(group.Name) == "" || len(group.Name) > 128 ||
		group.ResourceQuota.Validate() != nil || group.StorageQuotaBytes < 0 || group.MaxConcurrentJobs < 0 || group.MaxJobTimeoutSeconds < 0 ||
		group.MaxJobTimeoutSeconds > domain.MaxSupportedJobTimeoutSeconds {
		return domain.UserGroup{}, ErrInvalid
	}
	now := service.now().UTC()
	return service.accounts.UpdateUserGroup(ctx, id, func(current *domain.UserGroup) error {
		current.Name = strings.TrimSpace(group.Name)
		current.ResourceQuota = group.ResourceQuota
		current.StorageQuotaBytes = group.StorageQuotaBytes
		current.MaxConcurrentJobs = group.MaxConcurrentJobs
		current.MaxJobTimeoutSeconds = normalizedMaxJobTimeout(group.MaxJobTimeoutSeconds)
		current.Disabled = group.Disabled
		current.DisableSuspendQueued = group.DisableSuspendQueued
		current.DisableEndSessions = group.DisableEndSessions
		current.UpdatedAt = now
		return nil
	}, domain.AuditEvent{
		ID: "group_updated_" + id + "_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.group.updated", TargetKind: "user_group", TargetID: id, Outcome: "succeeded", OccurredAt: now,
	})
}

func (service *AccessService) DeleteUserGroup(ctx context.Context, id string, actor domain.Principal) ([]domain.User, error) {
	if service.accounts == nil || strings.TrimSpace(id) == "" {
		return nil, ErrInvalid
	}
	now := service.now().UTC()
	return service.accounts.DeleteUserGroup(ctx, id, domain.AuditEvent{
		ID: "group_deleted_" + id + "_" + strconv.FormatInt(now.UnixNano(), 10), ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.group.deleted", TargetKind: "user_group", TargetID: id, Outcome: "succeeded", OccurredAt: now,
		Details: map[string]string{"membershipPolicy": "detach"},
	})
}

func normalizedMaxJobTimeout(seconds int64) int64 {
	if seconds == 0 {
		return domain.DefaultMaxJobTimeoutSeconds
	}
	return seconds
}

func (service *AccessService) newLocalUser(ctx context.Context, credentials LocalCredentials, role string, status domain.UserStatus) (domain.User, string, error) {
	username := strings.ToLower(strings.TrimSpace(credentials.Username))
	displayName := strings.TrimSpace(credentials.DisplayName)
	if !validUsername(username) || len(displayName) > 128 {
		return domain.User{}, "", ErrInvalid
	}
	select {
	case service.passwordSlots <- struct{}{}:
		defer func() { <-service.passwordSlots }()
	case <-ctx.Done():
		return domain.User{}, "", ErrUnauthorized
	}
	hash, err := hashPassword(credentials.Password)
	if err != nil {
		return domain.User{}, "", err
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return domain.User{}, "", err
	}
	now := service.now().UTC()
	return domain.User{
		ID: "user_" + base64.RawURLEncoding.EncodeToString(idBytes), Username: username,
		DisplayName: displayName, Role: role, Status: status, CreatedAt: now, UpdatedAt: now,
	}, hash, nil
}

func validUsername(value string) bool {
	if len(value) < 3 || len(value) > 64 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') ||
			(index > 0 && (character == '.' || character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

func hashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=4" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	expected, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt) != 16 || len(expected) != 32 || len(password) > 1024 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func (service *AccessService) Authenticate(ctx context.Context, bearer string) (domain.Principal, error) {
	bearer = strings.TrimSpace(bearer)
	if strings.HasPrefix(bearer, "xat.") {
		digest := sha256.Sum256([]byte(bearer))
		token, err := service.repository.GetAPITokenByDigest(ctx, digest)
		if err != nil || token.RevokedAt != nil || !service.now().Before(token.ExpiresAt) {
			return domain.Principal{}, ErrUnauthorized
		}
		role := roleByName(token.Role)
		if role == nil {
			return domain.Principal{}, ErrUnauthorized
		}
		var account domain.User
		if token.UserID != "" && service.accounts != nil {
			account, _, err = service.accounts.GetUser(ctx, token.UserID)
			if err != nil || account.Status != domain.UserStatusActive {
				return domain.Principal{}, ErrUnauthorized
			}
			accountRole := roleByName(account.Role)
			if accountRole == nil {
				return domain.Principal{}, ErrUnauthorized
			}
			// A browser session follows the account's current role in both
			// directions. An automation token never gains privilege from its
			// owner's role, only loses it: minting a narrow token from an
			// administrator account must stay narrow, while demoting that
			// account must immediately narrow the token further.
			if token.Interactive {
				role = accountRole
			} else {
				permissions := intersectPermissions(role.Permissions, accountRole.Permissions)
				// The role label is used by ownership checks as well as the UI.
				// Never retain an admin label after an account demotion.
				if roleRank(accountRole.Name) < roleRank(role.Name) {
					role = accountRole
				}
				role.Permissions = permissions
			}
		}
		now := service.now().UTC()
		_, _ = service.repository.UpdateAPIToken(context.WithoutCancel(ctx), token.ID, func(current *domain.APIToken) error {
			current.LastUsedAt = &now
			return nil
		}, nil)
		return domain.Principal{
			UserID: token.UserID, Subject: token.Subject, DisplayName: account.DisplayName,
			Role: role.Name, Permissions: role.Permissions, AuthMethod: "api_token", GroupIDs: account.GroupIDs,
		}, nil
	}
	if service.oidc == nil {
		return domain.Principal{}, ErrUnauthorized
	}
	claims, err := service.oidc.Verify(ctx, bearer)
	if err != nil {
		return domain.Principal{}, ErrUnauthorized
	}
	roleName := ""
	grants := map[string]struct{}{}
	for _, group := range claims.Groups {
		mapped := service.groupRoles[group]
		if roleRank(mapped) > roleRank(roleName) {
			roleName = mapped
		}
		if mappedRole := roleByName(mapped); mappedRole != nil {
			for _, permission := range mappedRole.Permissions {
				grants[permission] = struct{}{}
			}
		}
	}
	role := roleByName(roleName)
	if role == nil {
		return domain.Principal{}, ErrForbidden
	}
	permissions := make([]string, 0, len(grants))
	for permission := range grants {
		permissions = append(permissions, permission)
	}
	sort.Strings(permissions)
	return domain.Principal{Subject: claims.Subject, DisplayName: claims.Name, Role: role.Name, Permissions: permissions, AuthMethod: "oidc"}, nil
}

func (service *AccessService) Authorize(principal domain.Principal, permission string) error {
	for _, granted := range principal.Permissions {
		if subtle.ConstantTimeCompare([]byte(granted), []byte("*")) == 1 ||
			subtle.ConstantTimeCompare([]byte(granted), []byte(permission)) == 1 {
			return nil
		}
	}
	return ErrForbidden
}

func (service *AccessService) ListTokens(ctx context.Context) ([]domain.APIToken, error) {
	return service.repository.ListAPITokens(ctx)
}

func (service *AccessService) RevokeToken(ctx context.Context, id string, actor domain.Principal) (domain.APIToken, error) {
	now := service.now().UTC()
	return service.repository.UpdateAPIToken(ctx, id, func(token *domain.APIToken) error {
		if token.RevokedAt != nil {
			return ErrConflict
		}
		token.RevokedAt = &now
		return nil
	}, &domain.AuditEvent{
		ID: "api_token_revoked_" + id, ActorType: actor.AuthMethod, ActorID: actor.Subject,
		Action: "access.api_token.revoked", TargetKind: "api_token", TargetID: id,
		Outcome: "succeeded", OccurredAt: now,
	})
}

// RevokeGroupSessions invalidates browser sessions for every member of a
// group. Automation API tokens remain valid because they are not interactive
// sessions and can be revoked independently by an administrator.
func (service *AccessService) RevokeGroupSessions(ctx context.Context, groupID string, actor domain.Principal) ([]string, int, error) {
	if service.accounts == nil || strings.TrimSpace(groupID) == "" {
		return nil, 0, ErrInvalid
	}
	users, err := service.accounts.ListUsers(ctx)
	if err != nil {
		return nil, 0, err
	}
	memberIDs := make(map[string]struct{})
	for _, user := range users {
		for _, id := range user.GroupIDs {
			if id == groupID {
				memberIDs[user.ID] = struct{}{}
				break
			}
		}
	}
	tokens, err := service.repository.ListAPITokens(ctx)
	if err != nil {
		return nil, 0, err
	}
	now, revoked := service.now().UTC(), 0
	for _, token := range tokens {
		if !strings.HasPrefix(token.ID, "session_") || token.RevokedAt != nil {
			continue
		}
		if _, member := memberIDs[token.UserID]; !member {
			continue
		}
		_, err = service.repository.UpdateAPIToken(ctx, token.ID, func(current *domain.APIToken) error {
			current.RevokedAt = &now
			return nil
		}, &domain.AuditEvent{
			ID:        "group_session_revoked_" + token.ID + "_" + strconv.FormatInt(now.UnixNano(), 10),
			ActorType: actor.AuthMethod, ActorID: actor.Subject, Action: "access.group.session_revoked",
			TargetKind: "api_token", TargetID: token.ID, Outcome: "succeeded", OccurredAt: now,
			Details: map[string]string{"groupId": groupID},
		})
		if err != nil {
			return nil, revoked, err
		}
		revoked++
	}
	ids := make([]string, 0, len(memberIDs))
	for id := range memberIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, revoked, nil
}

func roleByName(name string) *domain.Role {
	for index := range builtInRoles {
		if builtInRoles[index].Name == name {
			role := cloneRole(builtInRoles[index])
			return &role
		}
	}
	return nil
}

func intersectPermissions(issued, current []string) []string {
	contains := func(permissions []string, permission string) bool {
		for _, granted := range permissions {
			if granted == permission || granted == "*" {
				return true
			}
		}
		return false
	}
	if contains(issued, "*") {
		return append([]string(nil), current...)
	}
	var permissions []string
	for _, permission := range issued {
		if contains(current, permission) {
			permissions = append(permissions, permission)
		}
	}
	return permissions
}

func roleRank(name string) int {
	switch name {
	case "viewer":
		return 1
	case "user":
		return 2
	case "builder":
		return 2
	case "operator":
		return 3
	case "admin":
		return 4
	}
	return 0
}
func (service *AccessService) ValidateRole(name string) error {
	if roleByName(name) == nil {
		return fmt.Errorf("%w: unknown role", ErrInvalid)
	}
	return nil
}
