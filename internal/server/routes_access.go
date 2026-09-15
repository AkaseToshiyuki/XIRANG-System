package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/terminal"
)

func registerTerminalRoutes(mux *http.ServeMux, service *terminal.Service, adminToken string) {
	mux.HandleFunc("POST /api/v1/terminal/tickets", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			NodeID string `json:"nodeId"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		ticket, expiresAt, err := service.Issue(request.Context(), input.NodeID, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, map[string]any{"ticket": ticket, "expiresAt": expiresAt})
	}))
	mux.HandleFunc("GET /api/v1/terminal/connect", service.Connect)
}

func registerAccessRoutes(mux *http.ServeMux, service *identity.AccessService, jobs JobManager, terminals *terminal.Service, adminToken string) {
	mux.HandleFunc("GET /api/v1/setup/status", func(writer http.ResponseWriter, request *http.Request) {
		required, err := service.SetupRequired(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]bool{"setupRequired": required})
	})
	mux.HandleFunc("POST /api/v1/setup/admin", func(writer http.ResponseWriter, request *http.Request) {
		var payload identity.LocalCredentials
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		user, err := service.SetupAdmin(request.Context(), payload)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, user)
	})
	mux.HandleFunc("POST /api/v1/auth/register", func(writer http.ResponseWriter, request *http.Request) {
		var payload identity.LocalCredentials
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		user, err := service.Register(request.Context(), payload)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, user)
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		session, err := service.Login(request.Context(), payload.Username, payload.Password)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, session)
	})
	mux.HandleFunc("GET /api/v1/access/me", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, requestPrincipal(request))
	}))
	mux.HandleFunc("GET /api/v1/access/roles", requireAuthenticated(adminToken, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"items": service.Roles()})
	}))
	mux.HandleFunc("GET /api/v1/access/tokens", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		tokens, err := service.ListTokens(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": tokens})
	}))
	mux.HandleFunc("GET /api/v1/access/users", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		users, err := service.ListUsers(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": users})
	}))
	mux.HandleFunc("PATCH /api/v1/access/users/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Status   domain.UserStatus `json:"status,omitempty"`
			Role     string            `json:"role,omitempty"`
			GroupIDs []string          `json:"groupIds,omitempty"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		user, err := service.UpdateUser(request.Context(), request.PathValue("id"), payload.Status, payload.Role, requestPrincipal(request))
		if err == nil && payload.GroupIDs != nil {
			user, err = service.PutUserGroups(request.Context(), request.PathValue("id"), payload.GroupIDs, requestPrincipal(request))
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, user)
	}))
	mux.HandleFunc("GET /api/v1/access/groups", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		groups, err := service.ListUserGroups(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": groups})
	}))
	mux.HandleFunc("GET /api/v1/access/settings", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		settings, err := service.GetAccessSettings(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, settings)
	}))
	mux.HandleFunc("PUT /api/v1/access/settings", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			DefaultUserGroupID string `json:"defaultUserGroupId"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		settings, err := service.PutAccessSettings(request.Context(), payload.DefaultUserGroupID, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, settings)
	}))
	mux.HandleFunc("POST /api/v1/access/groups", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload domain.UserGroup
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		group, err := service.CreateUserGroup(request.Context(), payload, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, group)
	}))
	mux.HandleFunc("PUT /api/v1/access/groups/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload domain.UserGroup
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		previous, err := service.GetUserGroup(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		actor := requestPrincipal(request)
		group, err := service.UpdateUserGroup(request.Context(), request.PathValue("id"), payload, actor)
		if err == nil {
			if updater, ok := jobs.(UserStorageQuotaUpdater); ok {
				var users []domain.User
				users, err = service.ListUsers(request.Context())
				for _, user := range users {
					if err != nil || !containsString(user.GroupIDs, group.ID) {
						continue
					}
					var policy identity.EffectiveGroupPolicy
					policy, err = service.ResolveUserGroupPolicy(request.Context(), user.GroupIDs)
					if err == nil {
						err = updater.UpdateUserStorageQuota(request.Context(), user.ID, policy.StorageQuotaBytes)
					}
				}
			} else if updater, ok := jobs.(StorageQuotaUpdater); ok {
				err = updater.UpdateGroupStorageQuota(request.Context(), group.ID, group.StorageQuotaBytes)
			}
		}
		if err == nil {
			if updater, ok := jobs.(GroupDisableUpdater); ok {
				if group.Disabled && group.DisableSuspendQueued && (!previous.Disabled || !previous.DisableSuspendQueued) {
					err = updater.SetGroupDisabled(request.Context(), group.ID, true)
				} else if previous.DisableSuspendQueued && (!group.Disabled || !group.DisableSuspendQueued) {
					err = updater.SetGroupDisabled(request.Context(), group.ID, false)
				}
			}
		}
		if err == nil && group.Disabled && group.DisableEndSessions && (!previous.Disabled || !previous.DisableEndSessions) {
			var userIDs []string
			userIDs, _, err = service.RevokeGroupSessions(request.Context(), group.ID, actor)
			if err == nil && terminals != nil {
				terminals.TerminateUsers(userIDs)
			}
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, group)
	}))
	mux.HandleFunc("DELETE /api/v1/access/groups/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		groupID := request.PathValue("id")
		previous, err := service.GetUserGroup(request.Context(), groupID)
		if err != nil {
			writeError(writer, err)
			return
		}
		affected, err := service.DeleteUserGroup(request.Context(), groupID, requestPrincipal(request))
		if err == nil && previous.DisableSuspendQueued {
			if updater, ok := jobs.(GroupDisableUpdater); ok {
				err = updater.SetGroupDisabled(request.Context(), groupID, false)
			}
		}
		if err == nil {
			if updater, ok := jobs.(UserStorageQuotaUpdater); ok {
				for _, user := range affected {
					policy, policyErr := service.ResolveUserGroupPolicy(request.Context(), user.GroupIDs)
					if policyErr != nil {
						err = policyErr
						break
					}
					if err = updater.UpdateUserStorageQuota(request.Context(), user.ID, policy.StorageQuotaBytes); err != nil {
						break
					}
				}
			} else if updater, ok := jobs.(StorageQuotaUpdater); ok {
				err = updater.UpdateGroupStorageQuota(request.Context(), groupID, 0)
			}
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writer.Header().Set("X-Xirang-Detached-Users", strconv.Itoa(len(affected)))
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/access/tokens", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Name       string `json:"name"`
			Role       string `json:"role"`
			TTLSeconds int64  `json:"ttlSeconds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		created, err := service.CreateToken(request.Context(), payload.Name, payload.Role, time.Duration(payload.TTLSeconds)*time.Second, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, created)
	}))
	mux.HandleFunc("POST /api/v1/access/tokens/{id}/revoke", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		token, err := service.RevokeToken(request.Context(), request.PathValue("id"), requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, token)
	}))
}
