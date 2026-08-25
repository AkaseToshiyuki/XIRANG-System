import type { AccessPrincipal, AccessRole, AccessSettings, Alert, AlertDelivery, AlertWebhookStatus, APIToken, BackupManifest, BillingRate, BuildCleanupResult, BuildRetentionPolicy, BuildWorker, ClusterJoinToken, ClusterNode, DiscoveryBootstrap, DiscoveryCandidate, DiscoveryScan, ElasticitySnapshot, Job, JobTemplate, LocalUser, NativeBuild, NodeFlowObservation, NodeInventorySnapshot, NodeResourceObservation, QueuePolicy, SharedFile, SharedSpace, SubmitJobRequest, TelemetryIntervalSettings, TopologySnapshot, UsageSummary, UserGroup, VersionInfo } from "./contracts";

const jsonHeaders = { Accept: "application/json" };

async function identityRequest(path: string, payload?: Record<string, string>): Promise<Response> {
  return fetch(path, {
    method: payload ? "POST" : "GET",
    headers: payload ? { ...jsonHeaders, "Content-Type": "application/json" } : jsonHeaders,
    credentials: "same-origin",
    cache: "no-store",
    body: payload ? JSON.stringify(payload) : undefined,
  });
}

export async function getSetupStatus(): Promise<boolean> {
  const response = await identityRequest("/api/v1/setup/status");
  if (!response.ok) throw new Error(`Setup API returned ${response.status}`);
  return ((await response.json()) as { setupRequired: boolean }).setupRequired;
}

export async function setupAdmin(payload: { username: string; password: string; displayName: string }): Promise<void> {
  const response = await identityRequest("/api/v1/setup/admin", payload);
  if (!response.ok) throw new Error(((await response.json().catch(() => null)) as { error?: string } | null)?.error ?? `Setup returned ${response.status}`);
}

export async function registerLocalUser(payload: { username: string; password: string; displayName: string }): Promise<void> {
  const response = await identityRequest("/api/v1/auth/register", payload);
  if (!response.ok) throw new Error(((await response.json().catch(() => null)) as { error?: string } | null)?.error ?? `Registration returned ${response.status}`);
}

export async function loginLocalUser(username: string, password: string, language: "zh" | "en" = "en"): Promise<string> {
  const response = await identityRequest("/api/v1/auth/login", { username, password });
  if (!response.ok) throw new Error(language === "zh" ? "账户未获批准、已被禁用或凭据无效" : "The account is not approved, is disabled, or the credentials are invalid");
  return ((await response.json()) as { secret: string }).secret;
}

export async function getVersion(signal?: AbortSignal): Promise<VersionInfo> {
  const response = await fetch("/api/v1/version", {
    headers: jsonHeaders,
    credentials: "same-origin",
    signal,
  });
  if (!response.ok) {
    throw new Error(`Control plane returned ${response.status}`);
  }
  return response.json() as Promise<VersionInfo>;
}

export async function getTelemetryInterval(token: string, signal?: AbortSignal): Promise<TelemetryIntervalSettings> {
  const response = await fetch("/api/v1/system/telemetry-interval", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) throw new Error(`Telemetry interval API returned ${response.status}`);
  return response.json() as Promise<TelemetryIntervalSettings>;
}

export async function updateTelemetryInterval(intervalSeconds: number, token: string): Promise<TelemetryIntervalSettings> {
  const response = await fetch("/api/v1/system/telemetry-interval", {
    method: "PUT",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify({ intervalSeconds }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Telemetry interval update returned ${response.status}`);
  }
  return response.json() as Promise<TelemetryIntervalSettings>;
}

export async function getNodes(token: string, signal?: AbortSignal): Promise<ClusterNode[]> {
  const response = await fetch("/api/v1/nodes", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`Node API returned ${response.status}`);
  }
  const payload = (await response.json()) as { items: ClusterNode[] };
  return payload.items;
}

export async function updateNodeConfiguration(id: string, payload: Pick<ClusterNode, "roles" | "managementAddress" | "sshUser" | "group" | "computePriority">, token: string): Promise<ClusterNode> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(id)}/configuration`, {
    method: "PUT",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify(payload),
  });
	if (!response.ok) {
		const detail = await response.json().catch(() => null) as { error?: string } | null;
		throw new Error(detail?.error ?? `Node configuration returned ${response.status}`);
	}
  return response.json() as Promise<ClusterNode>;
}

export async function getSharedSpaces(token: string, signal?: AbortSignal): Promise<SharedSpace[]> {
  const response = await fetch("/api/v1/shared-spaces", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Shared space API returned ${response.status}`);
  const payload = await response.json() as { items?: SharedSpace[] | null };
  return Array.isArray(payload.items) ? payload.items : [];
}

export async function applySharedSpace(payload: Partial<SharedSpace>, token: string): Promise<SharedSpace> {
  const path = payload.id ? `/api/v1/shared-spaces/${encodeURIComponent(payload.id)}` : "/api/v1/shared-spaces";
  const response = await fetch(path, {
    method: payload.id ? "PUT" : "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Shared space apply returned ${response.status}`);
  }
  return response.json() as Promise<SharedSpace>;
}

export async function getSharedFiles(spaceId: string, path: string, token: string): Promise<SharedFile[]> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files?path=${encodeURIComponent(path)}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin", cache: "no-store",
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared file listing"));
  const payload = await response.json() as { items?: SharedFile[] | null };
  return Array.isArray(payload.items) ? payload.items : [];
}

export async function uploadSharedFile(spaceId: string, path: string, file: File, token: string): Promise<void> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/content?path=${encodeURIComponent(path)}`, {
    method: "PUT", headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/octet-stream" },
    credentials: "same-origin", body: file,
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared file upload"));
}

export async function downloadSharedFile(spaceId: string, path: string, token: string): Promise<Blob> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/content?path=${encodeURIComponent(path)}`, {
    headers: { Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared file download"));
  return response.blob();
}

export async function createSharedDirectory(spaceId: string, path: string, token: string): Promise<void> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/directory`, {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify({ path }),
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared directory creation"));
}

export async function renameSharedFile(spaceId: string, oldPath: string, newPath: string, token: string): Promise<void> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/rename`, {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify({ oldPath, newPath }),
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared file rename"));
}

export async function deleteSharedFile(spaceId: string, path: string, token: string): Promise<void> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files?path=${encodeURIComponent(path)}`, {
    method: "DELETE", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared file deletion"));
}

export async function deleteSharedSpace(id: string, token: string): Promise<void> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(id)}`, {
    method: "DELETE", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(`Shared space removal returned ${response.status}`);
}

export async function unshareSharedSpace(id: string, token: string): Promise<void> {
  const response = await fetch(`/api/v1/shared-spaces/${encodeURIComponent(id)}/unshare`, {
    method: "POST", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(await apiError(response, "Shared space unshare"));
}

export async function createTerminalTicket(nodeId: string, token: string): Promise<string> {
  const response = await fetch("/api/v1/terminal/tickets", {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify({ nodeId }),
  });
  if (!response.ok) throw new Error(`Terminal ticket API returned ${response.status}`);
  return ((await response.json()) as { ticket: string }).ticket;
}

export async function getJobs(token: string, limit = 250, signal?: AbortSignal): Promise<Job[]> {
  const response = await fetch(`/api/v1/jobs?limit=${limit}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) throw new Error(`Job API returned ${response.status}`);
  const payload = (await response.json()) as { items: Job[] };
  return payload.items;
}

export async function getJobTemplates(token: string, signal?: AbortSignal): Promise<JobTemplate[]> {
  const response = await fetch("/api/v1/job-templates", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) throw new Error(`Job template API returned ${response.status}`);
  const payload = await response.json() as { items?: JobTemplate[] | null };
  return Array.isArray(payload.items) ? payload.items : [];
}

export async function createJobTemplate(payload: Pick<JobTemplate, "name" | "description" | "script">, token: string): Promise<JobTemplate> {
  const response = await fetch("/api/v1/job-templates", {
    method: "POST",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify(payload),
  });
  if (!response.ok) throw new Error(await apiError(response, "Job template creation"));
  return response.json() as Promise<JobTemplate>;
}

export async function updateJobTemplate(template: Pick<JobTemplate, "id" | "name" | "description" | "script" | "revision">, token: string): Promise<JobTemplate> {
  const response = await fetch(`/api/v1/job-templates/${encodeURIComponent(template.id)}`, {
    method: "PUT",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify({ name: template.name, description: template.description ?? "", script: template.script, revision: template.revision }),
  });
  if (!response.ok) throw new Error(await apiError(response, "Job template update"));
  return response.json() as Promise<JobTemplate>;
}

export async function deleteJobTemplate(id: string, revision: number, token: string): Promise<void> {
  const response = await fetch(`/api/v1/job-templates/${encodeURIComponent(id)}?revision=${revision}`, {
    method: "DELETE",
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
  });
  if (!response.ok) throw new Error(await apiError(response, "Job template removal"));
}

async function apiError(response: Response, action: string): Promise<string> {
  const detail = await response.json().catch(() => null) as { error?: string } | null;
  return detail?.error ?? `${action} returned ${response.status}`;
}

export async function submitJob(payload: SubmitJobRequest, token: string): Promise<Job> {
  const response = await fetch("/api/v1/jobs", {
    method: "POST",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Job submission returned ${response.status}`);
  }
  return response.json() as Promise<Job>;
}

export async function submitJobScript(script: string, token: string): Promise<Job[]> {
  const response = await fetch("/api/v1/jobs/script", {
    method: "POST",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify({ script }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Job script submission returned ${response.status}`);
  }
  const payload = await response.json() as Job | { items: Job[] };
  return "items" in payload ? payload.items : [payload];
}

export async function cancelJob(id: string, token: string, reason = "canceled from Web console"): Promise<Job> {
  const response = await fetch(`/api/v1/jobs/${encodeURIComponent(id)}/cancel`, {
    method: "POST",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify({ reason, gracePeriodSeconds: 5 }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Job cancellation returned ${response.status}`);
  }
  return response.json() as Promise<Job>;
}

export async function watchJobLogs(id: string, token: string, onJob: (job: Job) => void, onOpen: () => void, signal: AbortSignal): Promise<void> {
  const response = await fetch(`/api/v1/jobs/${encodeURIComponent(id)}/logs/stream`, {
    headers: { Accept: "text/event-stream", Authorization: `Bearer ${token}` },
    credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok || !response.body) throw new Error(`Job log stream returned ${response.status}`);
  onOpen();
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  while (!signal.aborted) {
    const { value, done } = await reader.read();
    if (done) return;
    buffered += decoder.decode(value, { stream: true });
    let boundary = buffered.indexOf("\n\n");
    while (boundary >= 0) {
      const event = buffered.slice(0, boundary);
      buffered = buffered.slice(boundary + 2);
      const lines = event.split("\n");
      const eventType = lines.find((line) => line.startsWith("event: "))?.slice(7);
      const data = lines.filter((line) => line.startsWith("data: ")).map((line) => line.slice(6)).join("\n");
      if (eventType === "logs" && data) onJob(JSON.parse(data) as Job);
      boundary = buffered.indexOf("\n\n");
    }
  }
}

export async function getQueuePolicies(token: string, signal?: AbortSignal): Promise<QueuePolicy[]> {
  const response = await fetch("/api/v1/queues", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Queue API returned ${response.status}`);
  return ((await response.json()) as { items: QueuePolicy[] }).items;
}

export async function getElasticity(token: string, signal?: AbortSignal): Promise<ElasticitySnapshot> {
  const response = await fetch("/api/v1/elasticity", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Elasticity API returned ${response.status}`);
  return response.json() as Promise<ElasticitySnapshot>;
}

export async function getDiscoveryState(token: string, signal?: AbortSignal): Promise<{ scans: DiscoveryScan[]; candidates: DiscoveryCandidate[] }> {
  const headers = { ...jsonHeaders, Authorization: `Bearer ${token}` };
  const [scans, candidates] = await Promise.all([
    fetch("/api/v1/discovery/scans?limit=50", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/discovery/candidates", { headers, credentials: "same-origin", cache: "no-store", signal }),
  ]);
  if (!scans.ok || !candidates.ok) throw new Error(`Discovery API returned ${scans.ok ? candidates.status : scans.status}`);
  return {
    scans: ((await scans.json()) as { items: DiscoveryScan[] }).items,
    candidates: ((await candidates.json()) as { items: DiscoveryCandidate[] }).items,
  };
}

export async function startDiscoveryScan(cidrs: string[], token: string): Promise<DiscoveryScan> {
  const response = await fetch("/api/v1/discovery/scans", {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify({ cidrs }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Discovery scan returned ${response.status}`);
  }
  return response.json() as Promise<DiscoveryScan>;
}

export async function approveDiscoveryCandidate(id: string, pairingCode: string, token: string): Promise<DiscoveryBootstrap> {
  const response = await fetch(`/api/v1/discovery/candidates/${encodeURIComponent(id)}/approve`, {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` }, credentials: "same-origin",
    body: JSON.stringify({ pairingCode }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Candidate approval returned ${response.status}`);
  }
  return response.json() as Promise<DiscoveryBootstrap>;
}

export async function rejectDiscoveryCandidate(id: string, token: string): Promise<DiscoveryCandidate> {
  const response = await fetch(`/api/v1/discovery/candidates/${encodeURIComponent(id)}/reject`, {
    method: "POST", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(`Candidate rejection returned ${response.status}`);
  return response.json() as Promise<DiscoveryCandidate>;
}

export async function getClusterJoinToken(token: string, reset = false): Promise<ClusterJoinToken> {
  const response = await fetch(`/api/v1/enrollment/join-token${reset ? "/reset" : ""}`, {
    method: reset ? "POST" : "GET", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin", cache: "no-store",
  });
  if (!response.ok) throw new Error(`Cluster join token API returned ${response.status}`);
  return response.json() as Promise<ClusterJoinToken>;
}

export async function getBuildState(token: string, signal?: AbortSignal): Promise<{ builds: NativeBuild[]; workers: BuildWorker[]; retention: BuildRetentionPolicy }> {
  const headers = { ...jsonHeaders, Authorization: `Bearer ${token}` };
  const [builds, workers, retention] = await Promise.all([
    fetch("/api/v1/builds?limit=100", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/build-workers", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/build-retention", { headers, credentials: "same-origin", cache: "no-store", signal }),
  ]);
  if (!builds.ok || !workers.ok || !retention.ok) throw new Error(`Build API returned ${!builds.ok ? builds.status : !workers.ok ? workers.status : retention.status}`);
  const buildPayload = (await builds.json()) as { items?: NativeBuild[] | null };
  const workerPayload = (await workers.json()) as { items?: BuildWorker[] | null };
  return {
    builds: Array.isArray(buildPayload.items) ? buildPayload.items : [],
    workers: Array.isArray(workerPayload.items) ? workerPayload.items : [],
    retention: await retention.json() as BuildRetentionPolicy,
  };
}

export async function submitBuild(payload: Record<string, unknown>, token: string): Promise<NativeBuild> {
  const response = await fetch("/api/v1/builds", {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Build submission returned ${response.status}`);
  }
  return response.json() as Promise<NativeBuild>;
}

async function buildAction<T>(path: string, token: string, method: "POST" | "PUT" | "DELETE" = "POST", body?: unknown): Promise<T> {
  const response = await fetch(path, { method, headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` }, credentials: "same-origin", body: body === undefined ? undefined : JSON.stringify(body) });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Build operation returned ${response.status}`);
  }
  return response.json() as Promise<T>;
}

export function cancelBuild(id: string, token: string) { return buildAction<NativeBuild>(`/api/v1/builds/${encodeURIComponent(id)}/cancel`, token); }
export function updateBuildRuntimeTargets(id: string, nodeIds: string[], token: string) { return buildAction<NativeBuild>(`/api/v1/builds/${encodeURIComponent(id)}/runtime-targets`, token, "PUT", { nodeIds }); }
export function deleteBuild(id: string, token: string) { return buildAction<BuildCleanupResult>(`/api/v1/builds/${encodeURIComponent(id)}`, token, "DELETE"); }
export function cleanupBuilds(token: string) { return buildAction<BuildCleanupResult>("/api/v1/builds/cleanup", token); }
export function updateBuildRetention(policy: BuildRetentionPolicy, token: string) { return buildAction<BuildRetentionPolicy>("/api/v1/build-retention", token, "PUT", policy); }

export async function getAccessState(token: string, signal?: AbortSignal): Promise<{ principal: AccessPrincipal; roles: AccessRole[]; tokens: APIToken[]; users: LocalUser[]; groups: UserGroup[]; settings: AccessSettings }> {
  const headers = { ...jsonHeaders, Authorization: `Bearer ${token}` };
  const [me, roles, tokens, users, groups, settings] = await Promise.all([
    fetch("/api/v1/access/me", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/access/roles", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/access/tokens", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/access/users", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/access/groups", { headers, credentials: "same-origin", cache: "no-store", signal }),
    fetch("/api/v1/access/settings", { headers, credentials: "same-origin", cache: "no-store", signal }),
  ]);
  if (!me.ok || !roles.ok || !tokens.ok || !users.ok || !groups.ok || !settings.ok) throw new Error(`Access API returned ${!me.ok ? me.status : !roles.ok ? roles.status : !tokens.ok ? tokens.status : !users.ok ? users.status : !groups.ok ? groups.status : settings.status}`);
  return {
    principal: await me.json() as AccessPrincipal,
    roles: ((await roles.json()) as { items: AccessRole[] }).items,
    tokens: ((await tokens.json()) as { items: APIToken[] }).items,
    users: ((await users.json()) as { items: LocalUser[] }).items,
    groups: ((await groups.json()) as { items: UserGroup[] }).items,
    settings: await settings.json() as AccessSettings,
  };
}

export async function updateAccessSettings(defaultUserGroupId: string, token: string): Promise<AccessSettings> {
  const response = await fetch("/api/v1/access/settings", {
    method: "PUT", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify({ defaultUserGroupId }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Access settings update returned ${response.status}`);
  }
  return response.json() as Promise<AccessSettings>;
}

export async function updateLocalUser(id: string, payload: { status?: string; role?: string; groupIds?: string[] }, token: string): Promise<LocalUser> {
  const response = await fetch(`/api/v1/access/users/${encodeURIComponent(id)}`, {
    method: "PATCH", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `User update returned ${response.status}`);
  }
  return response.json() as Promise<LocalUser>;
}

export async function createUserGroup(payload: Omit<UserGroup, "id" | "createdAt" | "updatedAt">, token: string): Promise<UserGroup> {
  const response = await fetch("/api/v1/access/groups", {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(payload),
  });
  if (!response.ok) throw new Error(`Group creation returned ${response.status}`);
  return response.json() as Promise<UserGroup>;
}

export async function updateUserGroup(id: string, payload: Omit<UserGroup, "id" | "createdAt" | "updatedAt">, token: string): Promise<UserGroup> {
  const response = await fetch(`/api/v1/access/groups/${encodeURIComponent(id)}`, {
    method: "PUT", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(payload),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Group update returned ${response.status}`);
  }
  return response.json() as Promise<UserGroup>;
}

export async function deleteUserGroup(id: string, token: string): Promise<void> {
  const response = await fetch(`/api/v1/access/groups/${encodeURIComponent(id)}`, {
    method: "DELETE", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Group removal returned ${response.status}`);
  }
}

export async function getPrincipal(token: string, signal?: AbortSignal): Promise<AccessPrincipal> {
  const response = await fetch("/api/v1/access/me", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Identity API returned ${response.status}`);
  return response.json() as Promise<AccessPrincipal>;
}

export async function createAPIToken(payload: { name: string; role: string; ttlSeconds: number }, token: string): Promise<{ token: APIToken; secret: string }> {
  const response = await fetch("/api/v1/access/tokens", {
    method: "POST", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(payload),
  });
  if (!response.ok) throw new Error(`API token creation returned ${response.status}`);
  return response.json() as Promise<{ token: APIToken; secret: string }>;
}

export async function revokeAPIToken(id: string, token: string): Promise<APIToken> {
  const response = await fetch(`/api/v1/access/tokens/${encodeURIComponent(id)}/revoke`, {
    method: "POST", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(`API token revocation returned ${response.status}`);
  return response.json() as Promise<APIToken>;
}

export async function getBackups(token: string, signal?: AbortSignal): Promise<BackupManifest[]> {
  const response = await fetch("/api/v1/system/backups", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Backup API returned ${response.status}`);
  return ((await response.json()) as { items: BackupManifest[] }).items;
}

export async function createBackup(token: string): Promise<BackupManifest> {
  const response = await fetch("/api/v1/system/backups", {
    method: "POST", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(`Backup creation returned ${response.status}`);
  return response.json() as Promise<BackupManifest>;
}

export async function verifyBackup(id: string, token: string): Promise<BackupManifest> {
  const response = await fetch(`/api/v1/system/backups/${encodeURIComponent(id)}/verify`, {
    method: "POST", headers: { ...jsonHeaders, Authorization: `Bearer ${token}` }, credentials: "same-origin",
  });
  if (!response.ok) throw new Error(`Backup verification returned ${response.status}`);
  return response.json() as Promise<BackupManifest>;
}

export async function putQueuePolicy(name: string, policy: Omit<QueuePolicy, "name" | "updatedAt">, token: string): Promise<QueuePolicy> {
  const response = await fetch(`/api/v1/queues/${encodeURIComponent(name)}`, {
    method: "PUT", headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(policy),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Queue update returned ${response.status}`);
  }
  return response.json() as Promise<QueuePolicy>;
}

export async function getNodeInventory(nodeId: string, token: string, signal?: AbortSignal): Promise<NodeInventorySnapshot> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/inventory`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`Inventory API returned ${response.status}`);
  }
  return response.json() as Promise<NodeInventorySnapshot>;
}

export async function getNodeObservations(nodeId: string, token: string, limit = 100, signal?: AbortSignal): Promise<NodeResourceObservation[]> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/observations?limit=${limit}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`Observation API returned ${response.status}`);
  }
  const payload = (await response.json()) as { items: NodeResourceObservation[] };
  return payload.items;
}

export async function getTopology(token: string, version?: number, signal?: AbortSignal): Promise<TopologySnapshot> {
  const query = version ? `?version=${version}` : "";
  const response = await fetch(`/api/v1/topology${query}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(response.status === 404 ? "No topology snapshot has been reported yet" : `Topology API returned ${response.status}`);
  }
  return response.json() as Promise<TopologySnapshot>;
}

export async function getLatestFlows(token: string, signal?: AbortSignal): Promise<NodeFlowObservation[]> {
	const response = await fetch("/api/v1/flows", {
		headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
		credentials: "same-origin",
		cache: "no-store",
		signal,
	});
	if (!response.ok) throw new Error(`Flow API returned ${response.status}`);
	const payload = (await response.json()) as { items: NodeFlowObservation[] };
	return payload.items;
}

export async function updateNodeStatus(nodeId: string, action: "approve" | "reject", token: string): Promise<ClusterNode> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/${action}`, {
    method: "POST",
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
  });
  if (!response.ok) {
    throw new Error(`Node action returned ${response.status}`);
  }
  return response.json() as Promise<ClusterNode>;
}

export async function revokeNodeCertificate(nodeId: string, token: string): Promise<ClusterNode> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/certificate/revoke`, {
    method: "POST",
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
  });
  if (!response.ok) {
    throw new Error(`Certificate revocation returned ${response.status}`);
  }
  return response.json() as Promise<ClusterNode>;
}

export async function rotateNodeCertificate(nodeId: string, token: string): Promise<ClusterNode> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/certificate/rotate-command`, {
    method: "POST",
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
  });
  if (!response.ok) {
    throw new Error(`Certificate rotation command returned ${response.status}`);
  }
  const payload = (await response.json()) as { node: ClusterNode };
  return payload.node;
}

export async function setNodeMode(nodeId: string, mode: "ready" | "cordoned" | "draining", token: string): Promise<ClusterNode> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/mode`, {
    method: "POST",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify({ mode }),
  });
  if (!response.ok) {
    throw new Error(`Node mode command returned ${response.status}`);
  }
  const payload = (await response.json()) as { node: ClusterNode };
  return payload.node;
}

export async function setNodeTaints(nodeId: string, taints: NonNullable<ClusterNode["taints"]>, token: string): Promise<ClusterNode> {
  const response = await fetch(`/api/v1/nodes/${encodeURIComponent(nodeId)}/taints`, {
    method: "PUT",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    body: JSON.stringify({ taints }),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Node taint update returned ${response.status}`);
  }
  return response.json() as Promise<ClusterNode>;
}

export async function watchNodes(token: string, onNode: (node: ClusterNode) => void, signal: AbortSignal): Promise<void> {
  const response = await fetch("/api/v1/events/nodes", {
    headers: { Accept: "text/event-stream", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok || !response.body) {
    throw new Error(`Node event stream returned ${response.status}`);
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  while (!signal.aborted) {
    const { value, done } = await reader.read();
    if (done) return;
    buffered += decoder.decode(value, { stream: true });
    let boundary = buffered.indexOf("\n\n");
    while (boundary >= 0) {
      const event = buffered.slice(0, boundary);
      buffered = buffered.slice(boundary + 2);
      const data = event.split("\n").filter((line) => line.startsWith("data: ")).map((line) => line.slice(6)).join("\n");
      if (data) onNode(JSON.parse(data) as ClusterNode);
      boundary = buffered.indexOf("\n\n");
    }
  }
}

export async function watchTopology(token: string, onSnapshot: (snapshot: TopologySnapshot) => void, onOpen: () => void, signal: AbortSignal): Promise<void> {
  const response = await fetch("/api/v1/events/topology", {
    headers: { Accept: "text/event-stream", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok || !response.body) {
    throw new Error(`Topology event stream returned ${response.status}`);
  }
  onOpen();
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  while (!signal.aborted) {
    const { value, done } = await reader.read();
    if (done) return;
    buffered += decoder.decode(value, { stream: true });
    let boundary = buffered.indexOf("\n\n");
    while (boundary >= 0) {
      const event = buffered.slice(0, boundary);
      buffered = buffered.slice(boundary + 2);
      const lines = event.split("\n");
      const eventType = lines.find((line) => line.startsWith("event: "))?.slice(7);
      const data = lines.filter((line) => line.startsWith("data: ")).map((line) => line.slice(6)).join("\n");
      if (eventType === "topology" && data) onSnapshot(JSON.parse(data) as TopologySnapshot);
      boundary = buffered.indexOf("\n\n");
    }
  }
}

export async function getAlerts(token: string, includeResolved = false, signal?: AbortSignal): Promise<Alert[]> {
  const response = await fetch(`/api/v1/alerts?includeResolved=${includeResolved}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`Alert API returned ${response.status}`);
  }
  const payload = (await response.json()) as { items?: Alert[] | null };
  return Array.isArray(payload.items) ? payload.items : [];
}

export async function getUsageSummary(token: string, from: string, to: string, ownerId?: string, signal?: AbortSignal): Promise<UsageSummary> {
  const query = new URLSearchParams({ from, to });
  if (ownerId) query.set("ownerId", ownerId);
  const response = await fetch(`/api/v1/accounting/summary?${query}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Accounting API returned ${response.status}`);
  return response.json() as Promise<UsageSummary>;
}

export async function getUserUsageSummaries(token: string, from: string, to: string, signal?: AbortSignal): Promise<UsageSummary[]> {
  const query = new URLSearchParams({ from, to });
  const response = await fetch(`/api/v1/accounting/users?${query}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Accounting users API returned ${response.status}`);
  const payload = await response.json() as { items?: UsageSummary[] };
  return payload.items ?? [];
}

export async function getBillingRates(token: string, signal?: AbortSignal): Promise<BillingRate[]> {
  const response = await fetch("/api/v1/accounting/rates", {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin", cache: "no-store", signal,
  });
  if (!response.ok) throw new Error(`Billing rates API returned ${response.status}`);
  const payload = await response.json() as { items?: BillingRate[] };
  return payload.items ?? [];
}

export async function createBillingRate(token: string, rate: BillingRate): Promise<BillingRate> {
  const response = await fetch("/api/v1/accounting/rates", {
    method: "POST",
    headers: { ...jsonHeaders, "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    credentials: "same-origin", body: JSON.stringify(rate),
  });
  if (!response.ok) {
    const detail = await response.json().catch(() => null) as { error?: string } | null;
    throw new Error(detail?.error ?? `Billing rate creation returned ${response.status}`);
  }
  return response.json() as Promise<BillingRate>;
}

export async function acknowledgeAlert(id: string, token: string): Promise<Alert> {
  const response = await fetch(`/api/v1/alerts/${encodeURIComponent(id)}/acknowledge`, {
    method: "POST",
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
  });
  if (!response.ok) {
    throw new Error(`Alert acknowledgement returned ${response.status}`);
  }
  return response.json() as Promise<Alert>;
}

export async function watchAlerts(token: string, onAlert: (alert: Alert) => void, onOpen: () => void, signal: AbortSignal): Promise<void> {
  const response = await fetch("/api/v1/events/alerts", {
    headers: { Accept: "text/event-stream", Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok || !response.body) {
    throw new Error(`Alert event stream returned ${response.status}`);
  }
  onOpen();
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  while (!signal.aborted) {
    const { value, done } = await reader.read();
    if (done) return;
    buffered += decoder.decode(value, { stream: true });
    let boundary = buffered.indexOf("\n\n");
    while (boundary >= 0) {
      const event = buffered.slice(0, boundary);
      buffered = buffered.slice(boundary + 2);
      const lines = event.split("\n");
      const eventType = lines.find((line) => line.startsWith("event: "))?.slice(7);
      const data = lines.filter((line) => line.startsWith("data: ")).map((line) => line.slice(6)).join("\n");
      if (eventType === "alert" && data) onAlert(JSON.parse(data) as Alert);
      boundary = buffered.indexOf("\n\n");
    }
  }
}

export async function getAlertDeliveries(token: string, limit = 100, signal?: AbortSignal): Promise<{ webhook: AlertWebhookStatus; items: AlertDelivery[] }> {
  const response = await fetch(`/api/v1/alert-deliveries?limit=${limit}`, {
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
    cache: "no-store",
    signal,
  });
  if (!response.ok) {
    throw new Error(`Alert delivery API returned ${response.status}`);
  }
  const payload = (await response.json()) as { webhook?: AlertWebhookStatus; items?: AlertDelivery[] | null };
  return {
    webhook: payload.webhook ?? { enabled: false },
    items: Array.isArray(payload.items) ? payload.items : [],
  };
}

export async function retryAlertDelivery(id: string, token: string): Promise<AlertDelivery> {
  const response = await fetch(`/api/v1/alert-deliveries/${encodeURIComponent(id)}/retry`, {
    method: "POST",
    headers: { ...jsonHeaders, Authorization: `Bearer ${token}` },
    credentials: "same-origin",
  });
  if (!response.ok) {
    throw new Error(`Alert delivery retry returned ${response.status}`);
  }
  return response.json() as Promise<AlertDelivery>;
}
