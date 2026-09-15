import type { AccessPrincipal, AccessRole, AccessSettings, Alert, AlertDelivery, AlertWebhookStatus, APIToken, BackupManifest, BillingRate, BuildCleanupResult, BuildRetentionPolicy, BuildWorker, ClusterJoinToken, ClusterNode, DiscoveryBootstrap, DiscoveryCandidate, DiscoveryScan, ElasticitySnapshot, Job, JobTemplate, LocalUser, NativeBuild, NodeFlowObservation, NodeInventorySnapshot, NodeResourceObservation, QueuePolicy, SharedFile, SharedSpace, SubmitJobRequest, TelemetryIntervalSettings, TopologySnapshot, UsageSummary, UserGroup, VersionInfo } from "./contracts";

type Method = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

type RequestOptions = {
  /** Bearer credential. Omitted for the pre-authentication endpoints. */
  token?: string;
  method?: Method;
  /** Names the operation in the fallback error message. */
  label: string;
  /** Serialized as a JSON body. Use `body` for anything that is not JSON. */
  json?: unknown;
  body?: BodyInit;
  contentType?: string;
  accept?: string;
  signal?: AbortSignal;
};

/** Carries the HTTP status so callers can branch on it without parsing text. */
export class ApiRequestError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
  }
}

async function apiError(response: Response, action: string): Promise<string> {
  const detail = await response.json().catch(() => null) as { error?: string } | null;
  return detail?.error ?? `${action} returned ${response.status}`;
}

/**
 * request centralises the control-plane calling convention: same-origin
 * credentials, no caching of management state, the bearer header, and an error
 * that prefers the server's own message over a bare status code.
 */
async function request(path: string, options: RequestOptions): Promise<Response> {
  const headers: Record<string, string> = { Accept: options.accept ?? "application/json" };
  if (options.token) headers.Authorization = `Bearer ${options.token}`;
  if (options.json !== undefined) headers["Content-Type"] = "application/json";
  if (options.contentType) headers["Content-Type"] = options.contentType;
  const response = await fetch(path, {
    method: options.method ?? "GET",
    headers,
    credentials: "same-origin",
    cache: "no-store",
    signal: options.signal,
    body: options.json !== undefined ? JSON.stringify(options.json) : options.body,
  });
  if (!response.ok) throw new ApiRequestError(await apiError(response, options.label), response.status);
  return response;
}

async function requestJSON<T>(path: string, options: RequestOptions): Promise<T> {
  return (await request(path, options)).json() as Promise<T>;
}

/**
 * requestItems unwraps the `{ items: [...] }` envelope the list endpoints use,
 * tolerating both a missing key and an explicit null so an empty collection
 * never reaches a page as undefined.
 */
async function requestItems<T>(path: string, options: RequestOptions): Promise<T[]> {
  const payload = await requestJSON<{ items?: T[] | null }>(path, options);
  return Array.isArray(payload.items) ? payload.items : [];
}

async function requestVoid(path: string, options: RequestOptions): Promise<void> {
  await request(path, options);
}

/**
 * streamEvents consumes one server-sent event stream until the caller aborts.
 * When eventType is set only events of that name are delivered; the node stream
 * sends unnamed events and takes every frame.
 */
async function streamEvents<T>(path: string, options: {
  token: string;
  label: string;
  eventType?: string;
  onEvent: (payload: T) => void;
  onOpen?: () => void;
  signal: AbortSignal;
}): Promise<void> {
  const response = await request(path, {
    token: options.token, label: options.label, accept: "text/event-stream", signal: options.signal,
  });
  if (!response.body) throw new Error(`${options.label} returned no body`);
  options.onOpen?.();
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = "";
  while (!options.signal.aborted) {
    const { value, done } = await reader.read();
    if (done) return;
    buffered += decoder.decode(value, { stream: true });
    let boundary = buffered.indexOf("\n\n");
    while (boundary >= 0) {
      const frame = buffered.slice(0, boundary);
      buffered = buffered.slice(boundary + 2);
      const lines = frame.split("\n");
      const name = lines.find((line) => line.startsWith("event: "))?.slice(7);
      const data = lines.filter((line) => line.startsWith("data: ")).map((line) => line.slice(6)).join("\n");
      if (data && (options.eventType === undefined || name === options.eventType)) {
        options.onEvent(JSON.parse(data) as T);
      }
      boundary = buffered.indexOf("\n\n");
    }
  }
}

// Identity and bootstrap

export async function getSetupStatus(): Promise<boolean> {
  const payload = await requestJSON<{ setupRequired: boolean }>("/api/v1/setup/status", { label: "Setup API" });
  return payload.setupRequired;
}

export function setupAdmin(payload: { username: string; password: string; displayName: string }): Promise<void> {
  return requestVoid("/api/v1/setup/admin", { method: "POST", json: payload, label: "Setup" });
}

export function registerLocalUser(payload: { username: string; password: string; displayName: string }): Promise<void> {
  return requestVoid("/api/v1/auth/register", { method: "POST", json: payload, label: "Registration" });
}

export async function loginLocalUser(username: string, password: string, language: "zh" | "en" = "en"): Promise<string> {
  // Sign-in keeps its own message so a rejected attempt never hints at whether
  // the account exists, and so a throttled attempt is not reported as a bad
  // password.
  const response = await fetch("/api/v1/auth/login", {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    credentials: "same-origin",
    cache: "no-store",
    body: JSON.stringify({ username, password }),
  });
  if (response.status === 429) {
    throw new Error(language === "zh" ? "登录尝试过于频繁，请稍后再试" : "Too many sign-in attempts, retry later");
  }
  if (!response.ok) {
    throw new Error(language === "zh" ? "账户未获批准、已被禁用或凭据无效" : "The account is not approved, is disabled, or the credentials are invalid");
  }
  return ((await response.json()) as { secret: string }).secret;
}

export function getVersion(signal?: AbortSignal): Promise<VersionInfo> {
  return requestJSON<VersionInfo>("/api/v1/version", { label: "Control plane", signal });
}

export function getPrincipal(token: string, signal?: AbortSignal): Promise<AccessPrincipal> {
  return requestJSON<AccessPrincipal>("/api/v1/access/me", { token, label: "Identity API", signal });
}

// System settings

export function getTelemetryInterval(token: string, signal?: AbortSignal): Promise<TelemetryIntervalSettings> {
  return requestJSON<TelemetryIntervalSettings>("/api/v1/system/telemetry-interval", { token, label: "Telemetry interval API", signal });
}

export function updateTelemetryInterval(intervalSeconds: number, token: string): Promise<TelemetryIntervalSettings> {
  return requestJSON<TelemetryIntervalSettings>("/api/v1/system/telemetry-interval", {
    token, method: "PUT", json: { intervalSeconds }, label: "Telemetry interval update",
  });
}

export function getBackups(token: string, signal?: AbortSignal): Promise<BackupManifest[]> {
  return requestItems<BackupManifest>("/api/v1/system/backups", { token, label: "Backup API", signal });
}

export function createBackup(token: string): Promise<BackupManifest> {
  return requestJSON<BackupManifest>("/api/v1/system/backups", { token, method: "POST", label: "Backup creation" });
}

export function verifyBackup(id: string, token: string): Promise<BackupManifest> {
  return requestJSON<BackupManifest>(`/api/v1/system/backups/${encodeURIComponent(id)}/verify`, {
    token, method: "POST", label: "Backup verification",
  });
}

// Nodes

export function getNodes(token: string, signal?: AbortSignal): Promise<ClusterNode[]> {
  return requestItems<ClusterNode>("/api/v1/nodes", { token, label: "Node API", signal });
}

export function updateNodeConfiguration(id: string, payload: Pick<ClusterNode, "roles" | "managementAddress" | "sshUser" | "group" | "computePriority">, token: string): Promise<ClusterNode> {
  return requestJSON<ClusterNode>(`/api/v1/nodes/${encodeURIComponent(id)}/configuration`, {
    token, method: "PUT", json: payload, label: "Node configuration",
  });
}

export function updateNodeStatus(nodeId: string, action: "approve" | "reject", token: string): Promise<ClusterNode> {
  return requestJSON<ClusterNode>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/${action}`, {
    token, method: "POST", label: "Node action",
  });
}

export function revokeNodeCertificate(nodeId: string, token: string): Promise<ClusterNode> {
  return requestJSON<ClusterNode>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/certificate/revoke`, {
    token, method: "POST", label: "Certificate revocation",
  });
}

export function removeNode(nodeId: string, token: string): Promise<ClusterNode> {
  return requestJSON<ClusterNode>(`/api/v1/nodes/${encodeURIComponent(nodeId)}`, {
    token, method: "DELETE", label: "Node removal",
  });
}

export async function rotateNodeCertificate(nodeId: string, token: string): Promise<ClusterNode> {
  const payload = await requestJSON<{ node: ClusterNode }>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/certificate/rotate-command`, {
    token, method: "POST", label: "Certificate rotation command",
  });
  return payload.node;
}

export async function setNodeMode(nodeId: string, mode: "ready" | "cordoned" | "draining", token: string): Promise<ClusterNode> {
  const payload = await requestJSON<{ node: ClusterNode }>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/mode`, {
    token, method: "POST", json: { mode }, label: "Node mode command",
  });
  return payload.node;
}

export function setNodeTaints(nodeId: string, taints: NonNullable<ClusterNode["taints"]>, token: string): Promise<ClusterNode> {
  return requestJSON<ClusterNode>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/taints`, {
    token, method: "PUT", json: { taints }, label: "Node taint update",
  });
}

export function getNodeInventory(nodeId: string, token: string, signal?: AbortSignal): Promise<NodeInventorySnapshot> {
  return requestJSON<NodeInventorySnapshot>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/inventory`, {
    token, label: "Inventory API", signal,
  });
}

export function getNodeObservations(nodeId: string, token: string, limit = 100, signal?: AbortSignal): Promise<NodeResourceObservation[]> {
  return requestItems<NodeResourceObservation>(`/api/v1/nodes/${encodeURIComponent(nodeId)}/observations?limit=${limit}`, {
    token, label: "Observation API", signal,
  });
}

export function watchNodes(token: string, onNode: (node: ClusterNode) => void, signal: AbortSignal): Promise<void> {
  return streamEvents<ClusterNode>("/api/v1/events/nodes", { token, label: "Node event stream", onEvent: onNode, signal });
}

// Topology and flows

export async function getTopology(token: string, version?: number, signal?: AbortSignal): Promise<TopologySnapshot> {
  const query = version ? `?version=${version}` : "";
  try {
    return await requestJSON<TopologySnapshot>(`/api/v1/topology${query}`, { token, label: "Topology API", signal });
  } catch (error) {
    // An empty cluster has simply never reported, which is not a failure worth
    // showing as one. The status is matched rather than the message, which
    // carries the server's own wording and never mentions the code.
    if (error instanceof ApiRequestError && error.status === 404) {
      throw new Error("No topology snapshot has been reported yet");
    }
    throw error;
  }
}

export function watchTopology(token: string, onSnapshot: (snapshot: TopologySnapshot) => void, onOpen: () => void, signal: AbortSignal): Promise<void> {
  return streamEvents<TopologySnapshot>("/api/v1/events/topology", {
    token, label: "Topology event stream", eventType: "topology", onEvent: onSnapshot, onOpen, signal,
  });
}

export function getLatestFlows(token: string, signal?: AbortSignal): Promise<NodeFlowObservation[]> {
  return requestItems<NodeFlowObservation>("/api/v1/flows", { token, label: "Flow API", signal });
}

// Shared storage

export function getSharedSpaces(token: string, signal?: AbortSignal): Promise<SharedSpace[]> {
  return requestItems<SharedSpace>("/api/v1/shared-spaces", { token, label: "Shared space API", signal });
}

export function applySharedSpace(payload: Partial<SharedSpace>, token: string): Promise<SharedSpace> {
  const path = payload.id ? `/api/v1/shared-spaces/${encodeURIComponent(payload.id)}` : "/api/v1/shared-spaces";
  return requestJSON<SharedSpace>(path, {
    token, method: payload.id ? "PUT" : "POST", json: payload, label: "Shared space apply",
  });
}

export function deleteSharedSpace(id: string, token: string): Promise<void> {
  return requestVoid(`/api/v1/shared-spaces/${encodeURIComponent(id)}`, { token, method: "DELETE", label: "Shared space removal" });
}

export function unshareSharedSpace(id: string, token: string): Promise<void> {
  return requestVoid(`/api/v1/shared-spaces/${encodeURIComponent(id)}/unshare`, { token, method: "POST", label: "Shared space unshare" });
}

export function getSharedFiles(spaceId: string, path: string, token: string): Promise<SharedFile[]> {
  return requestItems<SharedFile>(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files?path=${encodeURIComponent(path)}`, {
    token, label: "Shared file listing",
  });
}

export function uploadSharedFile(spaceId: string, path: string, file: File, token: string): Promise<void> {
  return requestVoid(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/content?path=${encodeURIComponent(path)}`, {
    token, method: "PUT", body: file, contentType: "application/octet-stream", label: "Shared file upload",
  });
}

export async function downloadSharedFile(spaceId: string, path: string, token: string): Promise<Blob> {
  const response = await request(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/content?path=${encodeURIComponent(path)}`, {
    token, label: "Shared file download",
  });
  return response.blob();
}

export function createSharedDirectory(spaceId: string, path: string, token: string): Promise<void> {
  return requestVoid(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/directory`, {
    token, method: "POST", json: { path }, label: "Shared directory creation",
  });
}

export function renameSharedFile(spaceId: string, oldPath: string, newPath: string, token: string): Promise<void> {
  return requestVoid(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files/rename`, {
    token, method: "POST", json: { oldPath, newPath }, label: "Shared file rename",
  });
}

export function deleteSharedFile(spaceId: string, path: string, token: string): Promise<void> {
  return requestVoid(`/api/v1/shared-spaces/${encodeURIComponent(spaceId)}/files?path=${encodeURIComponent(path)}`, {
    token, method: "DELETE", label: "Shared file deletion",
  });
}

// Terminal

export async function createTerminalTicket(nodeId: string, token: string): Promise<string> {
  const payload = await requestJSON<{ ticket: string }>("/api/v1/terminal/tickets", {
    token, method: "POST", json: { nodeId }, label: "Terminal ticket API",
  });
  return payload.ticket;
}

// Jobs

export function getJobs(token: string, limit = 250, signal?: AbortSignal): Promise<Job[]> {
  return requestItems<Job>(`/api/v1/jobs?limit=${limit}`, { token, label: "Job API", signal });
}

export function submitJob(payload: SubmitJobRequest, token: string): Promise<Job> {
  return requestJSON<Job>("/api/v1/jobs", { token, method: "POST", json: payload, label: "Job submission" });
}

export async function submitJobScript(script: string, token: string): Promise<Job[]> {
  // A script may expand into a job array, so the endpoint answers with either
  // a single job or an envelope of them.
  const payload = await requestJSON<Job | { items: Job[] }>("/api/v1/jobs/script", {
    token, method: "POST", json: { script }, label: "Job script submission",
  });
  return "items" in payload ? payload.items : [payload];
}

export function cancelJob(id: string, token: string, reason = "canceled from Web console"): Promise<Job> {
  return requestJSON<Job>(`/api/v1/jobs/${encodeURIComponent(id)}/cancel`, {
    token, method: "POST", json: { reason, gracePeriodSeconds: 5 }, label: "Job cancellation",
  });
}

export function watchJobLogs(id: string, token: string, onJob: (job: Job) => void, onOpen: () => void, signal: AbortSignal): Promise<void> {
  return streamEvents<Job>(`/api/v1/jobs/${encodeURIComponent(id)}/logs/stream`, {
    token, label: "Job log stream", eventType: "logs", onEvent: onJob, onOpen, signal,
  });
}

export function getJobTemplates(token: string, signal?: AbortSignal): Promise<JobTemplate[]> {
  return requestItems<JobTemplate>("/api/v1/job-templates", { token, label: "Job template API", signal });
}

export function createJobTemplate(payload: Pick<JobTemplate, "name" | "description" | "script">, token: string): Promise<JobTemplate> {
  return requestJSON<JobTemplate>("/api/v1/job-templates", {
    token, method: "POST", json: payload, label: "Job template creation",
  });
}

export function updateJobTemplate(template: Pick<JobTemplate, "id" | "name" | "description" | "script" | "revision">, token: string): Promise<JobTemplate> {
  return requestJSON<JobTemplate>(`/api/v1/job-templates/${encodeURIComponent(template.id)}`, {
    token, method: "PUT", label: "Job template update",
    json: { name: template.name, description: template.description ?? "", script: template.script, revision: template.revision },
  });
}

export function deleteJobTemplate(id: string, revision: number, token: string): Promise<void> {
  return requestVoid(`/api/v1/job-templates/${encodeURIComponent(id)}?revision=${revision}`, {
    token, method: "DELETE", label: "Job template removal",
  });
}

// Queues and elasticity

export function getQueuePolicies(token: string, signal?: AbortSignal): Promise<QueuePolicy[]> {
  return requestItems<QueuePolicy>("/api/v1/queues", { token, label: "Queue API", signal });
}

export function putQueuePolicy(name: string, policy: Omit<QueuePolicy, "name" | "updatedAt">, token: string): Promise<QueuePolicy> {
  return requestJSON<QueuePolicy>(`/api/v1/queues/${encodeURIComponent(name)}`, {
    token, method: "PUT", json: policy, label: "Queue update",
  });
}

export function getElasticity(token: string, signal?: AbortSignal): Promise<ElasticitySnapshot> {
  return requestJSON<ElasticitySnapshot>("/api/v1/elasticity", { token, label: "Elasticity API", signal });
}

// Enrollment and discovery

export async function getDiscoveryState(token: string, signal?: AbortSignal): Promise<{ scans: DiscoveryScan[]; candidates: DiscoveryCandidate[] }> {
  const [scans, candidates] = await Promise.all([
    requestItems<DiscoveryScan>("/api/v1/discovery/scans?limit=50", { token, label: "Discovery API", signal }),
    requestItems<DiscoveryCandidate>("/api/v1/discovery/candidates", { token, label: "Discovery API", signal }),
  ]);
  return { scans, candidates };
}

export function startDiscoveryScan(cidrs: string[], token: string): Promise<DiscoveryScan> {
  return requestJSON<DiscoveryScan>("/api/v1/discovery/scans", {
    token, method: "POST", json: { cidrs }, label: "Discovery scan",
  });
}

export function approveDiscoveryCandidate(id: string, pairingCode: string, token: string): Promise<DiscoveryBootstrap> {
  return requestJSON<DiscoveryBootstrap>(`/api/v1/discovery/candidates/${encodeURIComponent(id)}/approve`, {
    token, method: "POST", json: { pairingCode }, label: "Candidate approval",
  });
}

export function rejectDiscoveryCandidate(id: string, token: string): Promise<DiscoveryCandidate> {
  return requestJSON<DiscoveryCandidate>(`/api/v1/discovery/candidates/${encodeURIComponent(id)}/reject`, {
    token, method: "POST", label: "Candidate rejection",
  });
}

export function getClusterJoinToken(token: string, reset = false): Promise<ClusterJoinToken> {
  return requestJSON<ClusterJoinToken>(`/api/v1/enrollment/join-token${reset ? "/reset" : ""}`, {
    token, method: reset ? "POST" : "GET", label: "Cluster join token API",
  });
}

// Native builds

export async function getBuildState(token: string, signal?: AbortSignal): Promise<{ builds: NativeBuild[]; workers: BuildWorker[]; retention: BuildRetentionPolicy }> {
  const [builds, workers, retention] = await Promise.all([
    requestItems<NativeBuild>("/api/v1/builds?limit=100", { token, label: "Build API", signal }),
    requestItems<BuildWorker>("/api/v1/build-workers", { token, label: "Build API", signal }),
    requestJSON<BuildRetentionPolicy>("/api/v1/build-retention", { token, label: "Build API", signal }),
  ]);
  return { builds, workers, retention };
}

export function submitBuild(payload: Record<string, unknown>, token: string): Promise<NativeBuild> {
  return requestJSON<NativeBuild>("/api/v1/builds", { token, method: "POST", json: payload, label: "Build submission" });
}

export function cancelBuild(id: string, token: string): Promise<NativeBuild> {
  return requestJSON<NativeBuild>(`/api/v1/builds/${encodeURIComponent(id)}/cancel`, { token, method: "POST", label: "Build cancellation" });
}

export function updateBuildRuntimeTargets(id: string, nodeIds: string[], token: string): Promise<NativeBuild> {
  return requestJSON<NativeBuild>(`/api/v1/builds/${encodeURIComponent(id)}/runtime-targets`, {
    token, method: "PUT", json: { nodeIds }, label: "Build runtime target update",
  });
}

export function deleteBuild(id: string, token: string): Promise<BuildCleanupResult> {
  return requestJSON<BuildCleanupResult>(`/api/v1/builds/${encodeURIComponent(id)}`, { token, method: "DELETE", label: "Build removal" });
}

export function cleanupBuilds(token: string): Promise<BuildCleanupResult> {
  return requestJSON<BuildCleanupResult>("/api/v1/builds/cleanup", { token, method: "POST", label: "Build cleanup" });
}

export function updateBuildRetention(policy: BuildRetentionPolicy, token: string): Promise<BuildRetentionPolicy> {
  return requestJSON<BuildRetentionPolicy>("/api/v1/build-retention", { token, method: "PUT", json: policy, label: "Build retention update" });
}

// Access control

export async function getAccessState(token: string, signal?: AbortSignal): Promise<{ principal: AccessPrincipal; roles: AccessRole[]; tokens: APIToken[]; users: LocalUser[]; groups: UserGroup[]; settings: AccessSettings }> {
  const label = "Access API";
  const [principal, roles, tokens, users, groups, settings] = await Promise.all([
    requestJSON<AccessPrincipal>("/api/v1/access/me", { token, label, signal }),
    requestItems<AccessRole>("/api/v1/access/roles", { token, label, signal }),
    requestItems<APIToken>("/api/v1/access/tokens", { token, label, signal }),
    requestItems<LocalUser>("/api/v1/access/users", { token, label, signal }),
    requestItems<UserGroup>("/api/v1/access/groups", { token, label, signal }),
    requestJSON<AccessSettings>("/api/v1/access/settings", { token, label, signal }),
  ]);
  return { principal, roles, tokens, users, groups, settings };
}

export function updateAccessSettings(defaultUserGroupId: string, token: string): Promise<AccessSettings> {
  return requestJSON<AccessSettings>("/api/v1/access/settings", {
    token, method: "PUT", json: { defaultUserGroupId }, label: "Access settings update",
  });
}

export function updateLocalUser(id: string, payload: { status?: string; role?: string; groupIds?: string[] }, token: string): Promise<LocalUser> {
  return requestJSON<LocalUser>(`/api/v1/access/users/${encodeURIComponent(id)}`, {
    token, method: "PATCH", json: payload, label: "User update",
  });
}

export function createUserGroup(payload: Omit<UserGroup, "id" | "createdAt" | "updatedAt">, token: string): Promise<UserGroup> {
  return requestJSON<UserGroup>("/api/v1/access/groups", { token, method: "POST", json: payload, label: "Group creation" });
}

export function updateUserGroup(id: string, payload: Omit<UserGroup, "id" | "createdAt" | "updatedAt">, token: string): Promise<UserGroup> {
  return requestJSON<UserGroup>(`/api/v1/access/groups/${encodeURIComponent(id)}`, {
    token, method: "PUT", json: payload, label: "Group update",
  });
}

export function deleteUserGroup(id: string, token: string): Promise<void> {
  return requestVoid(`/api/v1/access/groups/${encodeURIComponent(id)}`, { token, method: "DELETE", label: "Group removal" });
}

export function createAPIToken(payload: { name: string; role: string; ttlSeconds: number }, token: string): Promise<{ token: APIToken; secret: string }> {
  return requestJSON<{ token: APIToken; secret: string }>("/api/v1/access/tokens", {
    token, method: "POST", json: payload, label: "API token creation",
  });
}

export function revokeAPIToken(id: string, token: string): Promise<APIToken> {
  return requestJSON<APIToken>(`/api/v1/access/tokens/${encodeURIComponent(id)}/revoke`, {
    token, method: "POST", label: "API token revocation",
  });
}

// Alerts

export function getAlerts(token: string, includeResolved = false, signal?: AbortSignal): Promise<Alert[]> {
  return requestItems<Alert>(`/api/v1/alerts?includeResolved=${includeResolved}`, { token, label: "Alert API", signal });
}

export function acknowledgeAlert(id: string, token: string): Promise<Alert> {
  return requestJSON<Alert>(`/api/v1/alerts/${encodeURIComponent(id)}/acknowledge`, {
    token, method: "POST", label: "Alert acknowledgement",
  });
}

export function watchAlerts(token: string, onAlert: (alert: Alert) => void, onOpen: () => void, signal: AbortSignal): Promise<void> {
  return streamEvents<Alert>("/api/v1/events/alerts", {
    token, label: "Alert event stream", eventType: "alert", onEvent: onAlert, onOpen, signal,
  });
}

export async function getAlertDeliveries(token: string, limit = 100, signal?: AbortSignal): Promise<{ webhook: AlertWebhookStatus; items: AlertDelivery[] }> {
  const payload = await requestJSON<{ webhook?: AlertWebhookStatus; items?: AlertDelivery[] | null }>(
    `/api/v1/alert-deliveries?limit=${limit}`, { token, label: "Alert delivery API", signal },
  );
  return {
    webhook: payload.webhook ?? { enabled: false },
    items: Array.isArray(payload.items) ? payload.items : [],
  };
}

export function retryAlertDelivery(id: string, token: string): Promise<AlertDelivery> {
  return requestJSON<AlertDelivery>(`/api/v1/alert-deliveries/${encodeURIComponent(id)}/retry`, {
    token, method: "POST", label: "Alert delivery retry",
  });
}

// Accounting

export function getUsageSummary(token: string, from: string, to: string, ownerId?: string, signal?: AbortSignal): Promise<UsageSummary> {
  const query = new URLSearchParams({ from, to });
  if (ownerId) query.set("ownerId", ownerId);
  return requestJSON<UsageSummary>(`/api/v1/accounting/summary?${query}`, { token, label: "Accounting API", signal });
}

export function getUserUsageSummaries(token: string, from: string, to: string, signal?: AbortSignal): Promise<UsageSummary[]> {
  const query = new URLSearchParams({ from, to });
  return requestItems<UsageSummary>(`/api/v1/accounting/users?${query}`, { token, label: "Accounting users API", signal });
}

export function getBillingRates(token: string, signal?: AbortSignal): Promise<BillingRate[]> {
  return requestItems<BillingRate>("/api/v1/accounting/rates", { token, label: "Billing rates API", signal });
}

export function createBillingRate(token: string, rate: BillingRate): Promise<BillingRate> {
  return requestJSON<BillingRate>("/api/v1/accounting/rates", {
    token, method: "POST", json: rate, label: "Billing rate creation",
  });
}
