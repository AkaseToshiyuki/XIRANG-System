package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
	"github.com/xirang-grid/xirang/internal/enrollment"
	"github.com/xirang-grid/xirang/internal/identity"
	"github.com/xirang-grid/xirang/internal/jobspec"
	"github.com/xirang-grid/xirang/internal/jobtemplate"
	"github.com/xirang-grid/xirang/internal/scheduler"
)

func registerQueueRoutes(mux *http.ServeMux, queues QueueManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/elasticity", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		snapshot, err := queues.Elasticity(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, snapshot)
	}))
	mux.HandleFunc("GET /api/v1/queues", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		policies, err := queues.ListQueuePolicies(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": policies})
	}))
	mux.HandleFunc("PUT /api/v1/queues/{name}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Weight                  int32                 `json:"weight"`
			MaxConcurrent           int32                 `json:"maxConcurrent"`
			MaxConcurrentPerProject int32                 `json:"maxConcurrentPerProject"`
			ResourceQuota           domain.ResourceVector `json:"resourceQuota"`
			ProjectResourceQuota    domain.ResourceVector `json:"projectResourceQuota"`
			Backfill                *bool                 `json:"backfill"`
			AllowPreemption         bool                  `json:"allowPreemption"`
			Preemptible             bool                  `json:"preemptible"`
			PreemptionGraceSeconds  int32                 `json:"preemptionGraceSeconds"`
			MaxPreemptionsPerJob    int32                 `json:"maxPreemptionsPerJob"`
			MaxRecommendedScaleOut  int32                 `json:"maxRecommendedScaleOut"`
			ElasticityBudget        domain.ResourceVector `json:"elasticityBudget"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		backfill := true
		if payload.Backfill != nil {
			backfill = *payload.Backfill
		}
		policy, err := queues.PutQueuePolicy(request.Context(), domain.QueuePolicy{
			Name: request.PathValue("name"), Weight: payload.Weight, MaxConcurrent: payload.MaxConcurrent,
			MaxConcurrentPerProject: payload.MaxConcurrentPerProject, ResourceQuota: payload.ResourceQuota,
			ProjectResourceQuota: payload.ProjectResourceQuota, Backfill: backfill,
			AllowPreemption: payload.AllowPreemption, Preemptible: payload.Preemptible,
			PreemptionGraceSeconds: payload.PreemptionGraceSeconds, MaxPreemptionsPerJob: payload.MaxPreemptionsPerJob,
			MaxRecommendedScaleOut: payload.MaxRecommendedScaleOut, ElasticityBudget: payload.ElasticityBudget,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, policy)
	}))
}

func registerJobTemplateRoutes(mux *http.ServeMux, templates JobTemplateManager, adminToken string) {
	mux.HandleFunc("GET /api/v1/job-templates", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		items, err := templates.List(request.Context())
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	mux.HandleFunc("POST /api/v1/job-templates", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload jobTemplateRequest
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		if payload.Revision != 0 {
			writeError(writer, jobtemplate.ErrInvalid)
			return
		}
		template, err := templates.Create(request.Context(), payload.Name, payload.Description, payload.Script, requestPrincipal(request))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, template)
	}))
	mux.HandleFunc("GET /api/v1/job-templates/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		template, err := templates.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, template)
	}))
	mux.HandleFunc("PUT /api/v1/job-templates/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload jobTemplateRequest
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		template, err := templates.Update(
			request.Context(), request.PathValue("id"), payload.Revision,
			payload.Name, payload.Description, payload.Script, requestPrincipal(request),
		)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, template)
	}))
	mux.HandleFunc("DELETE /api/v1/job-templates/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		revision, err := strconv.ParseUint(request.URL.Query().Get("revision"), 10, 64)
		if err != nil || revision == 0 {
			writeError(writer, jobtemplate.ErrInvalid)
			return
		}
		if err := templates.Delete(request.Context(), request.PathValue("id"), revision, requestPrincipal(request)); err != nil {
			writeError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
}

func registerJobRoutes(mux *http.ServeMux, jobs JobManager, builds BuildManager, shared SharedSpaceManager, access *identity.AccessService, adminToken string) {
	mux.HandleFunc("GET /api/v1/jobs", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		limit := 100
		if raw := request.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1000 {
				writeError(writer, fmt.Errorf("%w: limit must be between 1 and 1000", scheduler.ErrInvalidJob))
				return
			}
			limit = parsed
		}
		// Non-administrators are paged over their own jobs in the store rather
		// than filtered afterwards, so a busy cluster cannot push every one of
		// a user's jobs out of the requested page.
		principal := requestPrincipal(request)
		var items []domain.Job
		var err error
		if principal.Role == "admin" {
			items, err = jobs.List(request.Context(), limit)
		} else {
			items, err = jobs.ListForOwner(request.Context(), principalIdentity(principal), limit)
		}
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"items": items})
	}))
	resolveApplication := func(ctx context.Context, payload *submitJobRequest) error {
		if payload.Application == "" || len(payload.ArtifactVariants) > 0 {
			return nil
		}
		resolver, ok := builds.(applicationBuildResolver)
		if !ok {
			return fmt.Errorf("%w: application build routing is unavailable", scheduler.ErrInvalidJob)
		}
		variants, err := resolver.ResolveApplication(ctx, payload.Application)
		if err != nil {
			return fmt.Errorf("%w: application %q has no runnable build mapping: %v", scheduler.ErrInvalidJob, payload.Application, err)
		}
		payload.Artifact = domain.NativeArtifact{}
		payload.ArtifactVariants = variants
		return nil
	}
	submitOne := func(request *http.Request, payload submitJobRequest) (domain.Job, error) {
		if err := resolveApplication(request.Context(), &payload); err != nil {
			return domain.Job{}, err
		}
		principal := requestPrincipal(request)
		projectID, groupID := payload.ProjectID, ""
		policyGroupIDs := []string(nil)
		var storageQuotaBytes int64
		maxJobTimeoutSeconds := int64(0)
		if principal.Role != "admin" {
			maxJobTimeoutSeconds = domain.DefaultMaxJobTimeoutSeconds
			if len(principal.GroupIDs) > 0 && access != nil {
				groupQuotaMu.Lock()
				defer groupQuotaMu.Unlock()
				policy, groupErr := access.ResolveUserGroupPolicy(request.Context(), principal.GroupIDs)
				if groupErr != nil {
					return domain.Job{}, groupErr
				}
				if policy.Disabled {
					return domain.Job{}, fmt.Errorf("%w: one or more user groups are temporarily disabled", identity.ErrForbidden)
				}
				active, listErr := jobs.ListActiveForOwner(request.Context(), principalIdentity(principal))
				if listErr != nil {
					return domain.Job{}, listErr
				}
				if err := enforceGroupQuota(policy, active, payload.Resources); err != nil {
					return domain.Job{}, err
				}
				groupID, policyGroupIDs = policy.PrimaryGroupID, policy.GroupIDs
				storageQuotaBytes, maxJobTimeoutSeconds = policy.StorageQuotaBytes, policy.MaxJobTimeoutSeconds
			}
			projectID = groupID
			if projectID == "" {
				projectID = "user-" + principalIdentity(principal)
			}
		}
		effectiveTimeout, timeoutErr := effectiveJobTimeoutSeconds(payload.TimeoutSeconds, maxJobTimeoutSeconds, principal.Role == "admin")
		if timeoutErr != nil {
			return domain.Job{}, timeoutErr
		}
		payload.TimeoutSeconds = effectiveTimeout
		job, err := jobs.Submit(request.Context(), scheduler.SubmitRequest{
			OwnerID: principalIdentity(principal), OwnerName: principal.DisplayName, GroupID: groupID, PolicyGroupIDs: policyGroupIDs,
			ProjectID: projectID, Queue: payload.Queue, Priority: payload.Priority, MaxAttempts: payload.MaxAttempts,
			StorageQuotaBytes: storageQuotaBytes,
			ArrayID:           payload.ArrayID, ArrayIndex: payload.ArrayIndex, ArraySize: payload.ArraySize,
			Dependencies: payload.Dependencies, AllowCrossOwnerDependencies: principal.Role == "admin",
			Task: domain.TaskSpec{
				Artifact: payload.Artifact, Application: payload.Application, ArtifactVariants: payload.ArtifactVariants,
				Arguments: payload.Arguments, WorkingDirectory: payload.WorkingDirectory,
				Environment: payload.Environment, Resources: payload.Resources,
				Placement: domain.Placement{
					RequiredArchitecture: payload.RequiredArchitecture, RequiredNodeID: payload.RequiredNodeID, RequiredLabels: payload.RequiredLabels,
					RequiredTopology: payload.RequiredTopology, ExcludedNodeIDs: payload.ExcludedNodeIDs,
					Tolerations: payload.Tolerations, PreferredZone: payload.PreferredZone, MinimumNetworkBits: payload.MinimumNetworkBits,
					CPUSet: payload.CPUSet, NUMANodes: payload.NUMANodes,
				},
				Parallel: payload.Parallel,
				Timeout:  time.Duration(payload.TimeoutSeconds) * time.Second,
			},
		})
		return job, err
	}
	mux.HandleFunc("POST /api/v1/jobs/script", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Script string `json:"script"`
		}
		if err := decodeJSON(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		spec, err := jobspec.Parse(strings.NewReader(input.Script))
		if err != nil {
			writeError(writer, fmt.Errorf("%w: %v", scheduler.ErrInvalidJob, err))
			return
		}
		base := submitJobRequest{
			ProjectID: spec.ProjectID, Queue: spec.Queue, Priority: spec.Priority, MaxAttempts: spec.MaxAttempts,
			Artifact: spec.Artifact, Application: spec.Application, Arguments: spec.Arguments, WorkingDirectory: spec.WorkingDirectory,
			Environment: spec.Environment, Resources: spec.Resources, RequiredArchitecture: spec.Artifact.Architecture,
			Parallel: spec.Parallel, TimeoutSeconds: int64(spec.Timeout.Seconds()), Dependencies: spec.Dependencies,
		}
		if err := resolveApplication(request.Context(), &base); err != nil {
			writeError(writer, err)
			return
		}
		indices := spec.ArrayIndices
		items := []string(nil)
		pinnedNodes := []string(nil)
		if spec.BatchPath != "" {
			if shared == nil {
				writeError(writer, fmt.Errorf("%w: shared storage is unavailable", scheduler.ErrInvalidJob))
				return
			}
			content := &limitedTextWriter{limit: 1 << 20}
			if err := shared.DownloadFile(request.Context(), spec.BatchSpaceID, spec.BatchPath, requestPrincipal(request), content); err != nil {
				writeError(writer, err)
				return
			}
			for _, line := range strings.Split(content.String(), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if len(line) > 4096 || strings.ContainsRune(line, '\x00') || len(items) == 1000 {
					writeError(writer, fmt.Errorf("%w: batch manifest must contain at most 1000 items of at most 4096 bytes", scheduler.ErrInvalidJob))
					return
				}
				items = append(items, line)
			}
			if len(items) == 0 {
				writeError(writer, fmt.Errorf("%w: batch manifest has no work items", scheduler.ErrInvalidJob))
				return
			}
			planner, ok := jobs.(batchJobPlanner)
			if !ok {
				writeError(writer, fmt.Errorf("%w: weighted batch planning is unavailable", scheduler.ErrInvalidJob))
				return
			}
			planTask := domain.TaskSpec{Artifact: base.Artifact, Application: base.Application, ArtifactVariants: base.ArtifactVariants,
				Arguments: base.Arguments, WorkingDirectory: base.WorkingDirectory, Environment: base.Environment,
				Resources: base.Resources, Parallel: base.Parallel}
			pinnedNodes, err = planner.PlanBatch(request.Context(), planTask, len(items), spec.BatchNodes)
			if err != nil {
				writeError(writer, err)
				return
			}
			indices = make([]int32, len(items))
			for index := range indices {
				indices[index] = int32(index)
			}
		} else if len(indices) == 0 {
			indices = []int32{0}
		}
		arrayID := ""
		if len(spec.ArrayIndices) > 0 || len(items) > 0 {
			value := make([]byte, 12)
			if _, err := rand.Read(value); err != nil {
				writeError(writer, err)
				return
			}
			prefix := "array_"
			if len(items) > 0 {
				prefix = "batch_"
			}
			arrayID = prefix + hex.EncodeToString(value)
		}
		created := make([]domain.Job, 0, len(indices))
		for _, index := range indices {
			payload := base
			payload.Arguments = append([]string(nil), base.Arguments...)
			payload.ArrayID, payload.ArrayIndex = arrayID, index
			if len(spec.ArrayIndices) > 0 || len(items) > 0 {
				payload.ArraySize = int32(len(indices))
			}
			if len(items) > 0 {
				if spec.BatchArgument != "" {
					payload.Arguments = append(payload.Arguments, spec.BatchArgument)
				}
				payload.Arguments = append(payload.Arguments, items[index])
				payload.RequiredNodeID = pinnedNodes[index]
			}
			job, err := submitOne(request, payload)
			if err != nil {
				writeError(writer, err)
				return
			}
			created = append(created, job)
		}
		if len(spec.ArrayIndices) == 0 && len(items) == 0 {
			writeJSON(writer, http.StatusCreated, created[0])
			return
		}
		response := map[string]any{"arrayId": arrayID, "items": created}
		if len(items) > 0 {
			response["batchId"] = arrayID
			response["nodePlan"] = pinnedNodes
		}
		writeJSON(writer, http.StatusCreated, response)
	}))
	mux.HandleFunc("POST /api/v1/jobs", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		var payload submitJobRequest
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		job, err := submitOne(request, payload)
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusCreated, job)
	}))
	mux.HandleFunc("GET /api/v1/jobs/{id}", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		job, err := jobs.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessJob(requestPrincipal(request), job) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		writeJSON(writer, http.StatusOK, job)
	}))
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs/stream", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			writeError(writer, enrollment.ErrUnavailable)
			return
		}
		job, err := jobs.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessJob(requestPrincipal(request), job) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		_ = http.NewResponseController(writer).SetWriteDeadline(time.Time{})
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache, no-transform")
		writer.Header().Set("X-Accel-Buffering", "no")
		writer.WriteHeader(http.StatusOK)
		lastUpdate := time.Time{}
		send := func(current domain.Job) bool {
			if !current.UpdatedAt.After(lastUpdate) {
				return true
			}
			encoded, err := json.Marshal(current)
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(writer, "event: logs\ndata: %s\n\n", encoded); err != nil {
				return false
			}
			lastUpdate = current.UpdatedAt
			flusher.Flush()
			return true
		}
		if !send(job) {
			return
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-request.Context().Done():
				return
			case <-ticker.C:
				current, err := jobs.Get(request.Context(), request.PathValue("id"))
				if err != nil || !send(current) {
					return
				}
			}
		}
	}))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", requireAuthenticated(adminToken, func(writer http.ResponseWriter, request *http.Request) {
		existing, err := jobs.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeError(writer, err)
			return
		}
		if !canAccessJob(requestPrincipal(request), existing) {
			writeError(writer, identity.ErrForbidden)
			return
		}
		var payload struct {
			Reason             string `json:"reason"`
			GracePeriodSeconds *int64 `json:"gracePeriodSeconds"`
		}
		if err := decodeJSON(writer, request, &payload); err != nil {
			writeError(writer, err)
			return
		}
		graceSeconds := int64(5)
		if payload.GracePeriodSeconds != nil {
			graceSeconds = *payload.GracePeriodSeconds
		}
		if graceSeconds < 0 || graceSeconds > 300 {
			writeError(writer, fmt.Errorf("%w: gracePeriodSeconds must be between 0 and 300", scheduler.ErrInvalidJob))
			return
		}
		job, err := jobs.Cancel(request.Context(), request.PathValue("id"), scheduler.CancelRequest{
			Reason: payload.Reason, GracePeriod: time.Duration(graceSeconds) * time.Second,
		})
		if err != nil {
			writeError(writer, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, job)
	}))
}

func principalIdentity(principal domain.Principal) string {
	if principal.UserID != "" {
		return principal.UserID
	}
	return principal.Subject
}

func ownsJob(principal domain.Principal, job domain.Job) bool {
	return job.OwnerID != "" && subtle.ConstantTimeCompare([]byte(job.OwnerID), []byte(principalIdentity(principal))) == 1
}

func canAccessJob(principal domain.Principal, job domain.Job) bool {
	return principal.Role == "admin" || ownsJob(principal, job)
}

func canAccessBuild(principal domain.Principal, build domain.Build) bool {
	return principal.Role == "admin" || build.OwnerID != "" && subtle.ConstantTimeCompare([]byte(build.OwnerID), []byte(principalIdentity(principal))) == 1
}

// enforceGroupQuota applies the allowance a user derives from their group
// membership to that user's own active jobs. The policy is per user by
// construction — see identity.EffectiveGroupPolicy — so the limits below are
// each member's allowance, not a pool shared across the group. Callers must
// pass every non-terminal job the owner holds; a truncated list under-counts
// usage and lets the quota be exceeded.

func enforceGroupQuota(policy identity.EffectiveGroupPolicy, active []domain.Job, requested domain.ResourceVector) error {
	var used domain.ResourceVector
	var concurrent int32
	for _, job := range active {
		concurrent++
		used = used.Add(job.Task.Resources)
	}
	if policy.MaxConcurrentJobs > 0 && concurrent >= policy.MaxConcurrentJobs {
		return fmt.Errorf("%w: per-user concurrent job quota from the user group reached", scheduler.ErrQuotaExceeded)
	}
	total := used.Add(requested)
	quota := policy.ResourceQuota
	if (quota.MilliCPU > 0 && total.MilliCPU > quota.MilliCPU) ||
		(quota.MemoryBytes > 0 && total.MemoryBytes > quota.MemoryBytes) {
		return fmt.Errorf("%w: per-user active resource quota from the user group exceeded", scheduler.ErrQuotaExceeded)
	}
	for name, quantity := range total.Devices {
		if quota.Devices[name] > 0 && quantity > quota.Devices[name] {
			return fmt.Errorf("%w: per-user device quota from the user group exceeded", scheduler.ErrQuotaExceeded)
		}
	}
	return nil
}

func effectiveJobTimeoutSeconds(requested, maximum int64, administrator bool) (int64, error) {
	if requested < 0 || requested > domain.MaxSupportedJobTimeoutSeconds || maximum < 0 || maximum > domain.MaxSupportedJobTimeoutSeconds {
		return 0, fmt.Errorf("%w: timeoutSeconds is outside the supported duration range", scheduler.ErrInvalidJob)
	}
	if administrator {
		return requested, nil
	}
	if maximum == 0 {
		maximum = domain.DefaultMaxJobTimeoutSeconds
	}
	if requested == 0 {
		return maximum, nil
	}
	if requested > maximum {
		return 0, fmt.Errorf("%w: requested timeout %s exceeds the user group's maximum %s", scheduler.ErrInvalidJob,
			formatPolicyDuration(requested), formatPolicyDuration(maximum))
	}
	return requested, nil
}
