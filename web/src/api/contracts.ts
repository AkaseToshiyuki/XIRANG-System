export interface VersionInfo {
  name: string;
  version: string;
  protocolVersion: string;
  commit: string;
  buildTime: string;
  goVersion: string;
  os: string;
  architecture: string;
}

export interface TelemetryIntervalSettings {
  intervalSeconds: number;
  minimumSeconds: number;
  maximumSeconds: number;
}

export interface ResourceSummary {
  label: string;
  used: number;
  capacity: number;
  unit: string;
}

export type NodeHealth = "ready" | "warning" | "offline";

export interface NodeSummary {
  id: string;
  name: string;
  architecture: string;
  health: NodeHealth;
  cpuPercent: number;
  memoryPercent: number;
}

export interface ResourceVector {
  milliCpu: number;
  memoryBytes: number;
  ephemeralStorageBytes: number;
  devices?: Record<string, number>;
}

export type JobStatus = "pending" | "queued" | "assigned" | "running" | "suspended" | "canceling" | "orphaned" | "succeeded" | "failed" | "canceled";

export interface NativeArtifact {
  uri: string;
  sha256: string;
  architecture: string;
  abi?: string;
  signature?: string;
  entrypoint: string;
  runtimeRequirements?: Record<string, string>;
}

export interface Job {
  id: string;
  ownerId: string;
  ownerName?: string;
  groupId?: string;
  policyGroupIds?: string[];
  suspendedPolicyGroupIds?: string[];
  arrayId?: string;
  arrayIndex?: number;
  arraySize?: number;
  dependencies?: Array<{ jobId: string; condition: "afterok" | "afterany" }>;
  projectId: string;
  queue: string;
  priority: number;
  status: JobStatus;
  task: {
    artifact: NativeArtifact;
    application?: string;
    artifactVariants?: Array<{ buildId: string; variant: string; nodeIds: string[]; artifact: NativeArtifact }>;
    arguments?: string[];
    workingDirectory?: string;
    environment?: Record<string, string>;
    resources: ResourceVector;
    parallel?: {
      nodes?: number;
      tasksPerNode?: number;
      launcher?: "native" | "openmpi" | "mpich" | "intelmpi";
    };
    placement?: {
      requiredNodeId?: string;
      requiredArchitecture?: string;
      requiredLabels?: Record<string, string>;
      requiredTopology?: Record<string, string>;
      excludedNodeIds?: string[];
      tolerations?: Toleration[];
      preferredZone?: string;
      minimumNetworkBits?: number;
      cpuSet?: string;
      numaNodes?: string;
    };
    timeout?: number;
  };
  maxAttempts: number;
  attempt: number;
  assignmentId?: string;
  nodeId?: string;
  allocation?: {
    id: string;
    attempt: number;
    state: "preparing" | "ready" | "running" | "canceling" | "succeeded" | "failed" | "canceled";
    worldSize: number;
    launcher: "native" | "openmpi" | "mpich" | "intelmpi";
    selectionOptimal: boolean;
    selectionSearchStates?: number;
    members: Array<{
      nodeId: string;
      assignmentId: string;
      nodeRank: number;
      rankStart: number;
      rankCount: number;
      networkInterface?: string;
      address?: string;
      state?: "accepted" | "pulling" | "ready" | "running" | "suspended" | "succeeded" | "failed" | "canceled";
      exitCode?: number;
      reason?: string;
      stdoutTail?: string;
      stderrTail?: string;
      logsTruncated?: boolean;
      observedAt?: string;
    }>;
    hosts: Array<{ nodeId: string; hostname: string; address?: string; networkInterface?: string; slots: number }>;
    taskGroups: Array<{ id: string; launcher: string; ranks: Array<{ rank: number; localRank: number; nodeRank: number; nodeId: string }> }>;
    createdAt: string;
    updatedAt: string;
  };
  exitCode?: number;
  reason?: string;
  blockReason?: {
    code: string;
    category: "dependency" | "queue" | "quota" | "policy" | "node" | "architecture" | "capability" | "device" | "placement" | "resource" | "topology" | "scheduler";
    message: string;
    details?: Record<string, string>;
    observedAt: string;
  };
  stdoutTail?: string;
  stderrTail?: string;
  logsTruncated?: boolean;
  logsObservedAt?: string;
  cancelGracePeriod?: number;
  preemptedByJobId?: string;
  preemptionCount?: number;
  preemptionNodeId?: string;
  preemptionVictims?: string[];
  createdAt: string;
  updatedAt: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface JobTemplate {
  id: string;
  name: string;
  description?: string;
  script: string;
  createdBy: string;
  updatedBy: string;
  revision: number;
  createdAt: string;
  updatedAt: string;
}

export interface SubmitJobRequest {
  projectId: string;
  queue: string;
  priority: number;
  maxAttempts: number;
  artifact: NativeArtifact;
  arguments?: string[];
  workingDirectory?: string;
  environment?: Record<string, string>;
  resources: ResourceVector;
  parallel?: { nodes: number; tasksPerNode: number; launcher: "native" | "openmpi" | "mpich" | "intelmpi" };
  requiredArchitecture: string;
  requiredTopology?: Record<string, string>;
  excludedNodeIds?: string[];
  tolerations?: Toleration[];
  preferredZone?: string;
  minimumNetworkBits?: number;
  cpuSet?: string;
  numaNodes?: string;
  timeoutSeconds?: number;
}

export interface Toleration {
  key: string;
  value?: string;
  effect: "NoSchedule" | "NoExecute";
}

export interface QueuePolicy {
  name: string;
  weight: number;
  maxConcurrent?: number;
  maxConcurrentPerProject?: number;
  resourceQuota: ResourceVector;
  projectResourceQuota: ResourceVector;
  backfill: boolean;
  allowPreemption: boolean;
  preemptible: boolean;
  preemptionGraceSeconds?: number;
  maxPreemptionsPerJob?: number;
  maxRecommendedScaleOut?: number;
  elasticityBudget: ResourceVector;
  qos?: "normal" | "high" | "urgent";
  maxWallTimeSeconds?: number;
  fairShareHalfLifeHours?: number;
  historicalUsageWeight?: number;
  reservationStart?: string;
  reservationEnd?: string;
  reservationNodeIds?: string[];
  reservationGroupIds?: string[];
  updatedAt: string;
}

export interface ElasticitySnapshot {
  generatedAt: string;
  recommendations: Array<{
    queue: string;
    queuedJobs: number;
    queuedResources: ResourceVector;
    capacityDeficit: ResourceVector;
    suggestedAdditionalNodes: number;
    limitedByBudget: boolean;
    reason: string;
  }>;
}

export interface DiscoveryScan {
  id: string;
  cidrs: string[];
  ports: number[];
  status: "running" | "completed" | "failed";
  scannedHosts: number;
  candidatesSeen: number;
  failure?: string;
  startedAt: string;
  finishedAt?: string;
}

export interface DiscoveryCandidate {
  id: string;
  address: string;
  agentPort: number;
  openPorts: number[];
  banners?: Record<string, string>;
  protocol?: string;
  agentVersion?: string;
  hostname?: string;
  architecture?: string;
  distribution?: string;
  kernelVersion?: string;
  cpuCount?: number;
  memoryBytes?: number;
  interfaces?: NetworkInterfaceSnapshot[];
  fingerprint?: string;
  pairingRequired?: boolean;
  enrolledNodeId?: string;
  status: "discovered" | "approved" | "rejected";
  firstSeenAt: string;
  lastSeenAt: string;
  lastScanId: string;
  approvedAt?: string;
  rejectedAt?: string;
}

export interface DiscoveryBootstrap {
  candidate: DiscoveryCandidate;
  enrollmentToken?: string;
  expiresAt: string;
  controllerUrl: string;
  installCommand?: string;
  nodeId?: string;
}

export interface ClusterJoinToken {
  token: {
    id: string;
    name: string;
    uses: number;
    maxUses: number;
    expiresAt: string;
  };
  secret: string;
}

export interface BuildWorker {
  id: string;
  architecture: string;
  toolchains: string[];
  toolchainCapabilities?: Array<{
    name: string;
    version?: string;
    targetArchitectures: string[];
    executables?: string[];
  }>;
  isolation: string;
  ready: boolean;
  supportsCancellation?: boolean;
}

export interface NativeBuild {
  id: string;
  ownerId?: string;
  ownerName?: string;
  status: "running" | "canceling" | "succeeded" | "failed" | "canceled";
  recipe: {
    name: string;
    application?: string;
    variant?: string;
    runtimeNodeIds?: string[];
		runtimeEntrypoint?: string;
		runtimeRequirements?: Record<string, string>;
    sourcePath: string;
    sourceSha256: string;
    architecture: string;
    builderArchitecture?: string;
    workerId?: string;
    crossCompile?: boolean;
    sourceSpaceId?: string;
    outputSpaceId?: string;
    publishPath?: string;
    command: string[];
    outputPath: string;
    toolchain: string;
    milliCpu: number;
    memoryBytes: number;
    timeout: number;
  };
  workerId: string;
  logTail?: string;
  failure?: string;
  artifact?: {
    uri: string;
    sha256: string;
    signature: string;
    publicKey: string;
    architecture: string;
    abi: string;
    toolchain: string;
    sbomUri: string;
    application?: string;
    variant?: string;
    entrypoint?: string;
		runtimeNodeIds?: string[];
		runtimeRequirements?: Record<string, string>;
    routingUpdatedAt?: string;
  };
  createdAt: string;
  startedAt: string;
  finishedAt?: string;
}

export interface BuildRetentionPolicy {
  maximumRecords: number;
  maximumAgeSeconds: number;
  preserveSuccessfulBuilds: boolean;
}

export interface BuildCleanupResult {
  deletedRecords: number;
  deletedArtifacts: number;
  reclaimedBytes: number;
}

export interface AccessPrincipal {
  userId?: string;
  subject: string;
  displayName?: string;
  role: string;
  permissions: string[];
  authMethod: string;
  groupIds?: string[];
}

export interface LocalUser {
  id: string;
  username: string;
  displayName?: string;
  role: string;
  groupIds?: string[];
  status: "pending" | "active" | "rejected" | "disabled";
  createdAt: string;
  approvedAt?: string;
}

export interface UserGroup {
  id: string;
  name: string;
  resourceQuota: ResourceVector;
  storageQuotaBytes: number;
  maxConcurrentJobs: number;
  maxJobTimeoutSeconds: number;
  disabled?: boolean;
  disableSuspendQueued?: boolean;
  disableEndSessions?: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface AccessSettings {
  defaultUserGroupId: string;
  updatedAt?: string;
}

export interface AccessRole {
  name: string;
  description: string;
  permissions: string[];
}

export interface APIToken {
  id: string;
  name: string;
  role: string;
  subject: string;
  createdAt: string;
  expiresAt: string;
  lastUsedAt?: string;
  revokedAt?: string;
}

export interface BackupManifest {
  id: string;
  schemaVersion: number;
  createdAt: string;
  files: Array<{ name: string; sha256: string; size: number }>;
}

export interface ClusterNode {
  id: string;
  name: string;
  status: string;
  desiredStatus?: string;
  pendingModeCommandId?: string;
  pendingManagementCommandId?: string;
  desiredManagementPanel?: boolean;
  managementPanelEnabled?: boolean;
  managementPanelStatus?: string;
  os: string;
  architecture: string;
  distribution?: string;
  kernelVersion?: string;
  runtimeFacts?: Record<string, string>;
  agentVersion?: string;
  capabilities?: string[];
  cpuModel?: string;
  cpuFeatures?: string[];
  cpuBenchmarkScore?: number;
  computeWeight?: number;
  computePriority?: number;
  labels?: Record<string, string>;
  roles?: Array<"management" | "compute" | "storage">;
  managementAddress?: string;
  sshUser?: string;
  group?: string;
  taints?: Array<{ key: string; value?: string; effect: "NoSchedule" | "NoExecute" }>;
  numaTopology?: Array<{ id: number; cpus: string; memoryBytes: number }>;
  deviceTopology?: Record<string, Record<string, number>>;
  certificateSerial?: string;
  certificateNotAfter?: string;
  pendingCertificateSerial?: string;
  pendingCertificateNotAfter?: string;
  pendingCertificateCommandId?: string;
  pendingCertificateRotationId?: string;
  certificateRevokedAt?: string;
  removedAt?: string;
  capacity: ResourceVector;
  allocatable: ResourceVector;
  allocated: ResourceVector;
  revision: number;
  createdAt: string;
  updatedAt: string;
  lastSeenAt: string;
}

export interface SharedSpace {
  id: string;
  name: string;
  sourceNodeId: string;
  sourcePath: string;
  mountPath: string;
  target: "all" | "group" | "nodes";
  targetGroup?: string;
  targetNodeIds?: string[];
  status: "applying" | "active" | "degraded" | "failed";
  results?: Array<{ nodeId: string; status: string; message?: string }>;
  usedBytes?: number;
  availableBytes?: number;
  totalBytes?: number;
  capacityObservedAt?: string;
  capacityError?: string;
  createdAt: string;
  updatedAt: string;
}

export interface SharedFile {
  path: string;
  name: string;
  directory: boolean;
  sizeBytes: number;
  modifiedAt: string;
  owner: string;
  ownedByUser: boolean;
}

export interface NetworkInterfaceSnapshot {
  name: string;
  macAddress?: string;
  addresses?: string[];
  speedBitsPerSecond?: number;
  mtu?: number;
}

export interface NodeInventorySnapshot {
  nodeId: string;
  hostname: string;
  distribution?: string;
  kernelVersion?: string;
  architecture: string;
  runtimeFacts?: Record<string, string>;
  logicalCpuCount: number;
  cpuModel?: string;
  cpuFeatures?: string[];
  cpuBenchmarkScore?: number;
  computeWeight?: number;
  memoryBytes: number;
  networkInterfaces?: NetworkInterfaceSnapshot[];
  devices?: Array<{ resourceName: string; vendor?: string; model?: string; deviceId?: string; attributes?: Record<string, string> }>;
  numaTopology?: Array<{ id: number; cpus: string; memoryBytes: number }>;
  observedAt: string;
  receivedAt: string;
}

export interface NodeResourceObservation {
  messageId: string;
  nodeId: string;
  usedMilliCpu: number;
  usedMemoryBytes: number;
  receivedBytes: number;
  transmittedBytes: number;
  usedDevices?: Record<string, number>;
  cpuTemperatureC?: number;
  cpuTemperatureAvailable: boolean;
  cpuTemperatureSource?: string;
  cpuTemperatureConfidence?: "high" | "medium" | "low";
  gpuTemperatureC?: number;
  gpuUtilizationPercent?: number;
  gpuUtilizationAvailable: boolean;
  gpuTemperatureAvailable: boolean;
  accelerators?: Array<{ id: string; vendor: string; utilizationPercent?: number; temperatureC?: number; memoryUsedBytes?: number; memoryTotalBytes?: number; utilizationAvailable: boolean; temperatureAvailable: boolean }>;
  interfaceCounters?: Record<string, {
    receivedBytes: number;
    transmittedBytes: number;
    receivedPackets: number;
    transmittedPackets: number;
    receiveErrors: number;
    transmitErrors: number;
    receiveDropped: number;
    transmitDropped: number;
    packetCountersSupported: boolean;
  }>;
  observedAt: string;
  receivedAt: string;
}

export interface TopologyVertex {
  id: string;
  kind: "host" | "switch" | "router" | "unknown";
  labels?: Record<string, string>;
}

export interface TopologyEdge {
  id: string;
  type: "neighbor" | "route" | "lldp" | "probe";
  sourceId: string;
  targetId: string;
  sourceInterface?: string;
  targetInterface?: string;
  sourceAddress?: string;
  targetAddress?: string;
  capacityBits?: number;
  observedBits?: number;
  observedPacketsPerSecond?: number;
  droppedPacketsPerSecond?: number;
  errorPacketsPerSecond?: number;
  dropRatio?: number;
  latencyMicros?: number;
  health?: "healthy" | "degraded" | "critical";
  observedAt: string;
}

export interface NetworkPathSelection {
  sourceId: string;
  targetId: string;
  primaryEdgeId: string;
  primaryReverseEdgeId?: string;
  fallbackEdgeIds?: string[];
  fallbackReverseEdgeIds?: string[];
  effectiveCapacityBits?: number;
  effectiveLatencyMicros?: number;
  health?: "healthy" | "degraded" | "critical";
  bidirectional: boolean;
  policy: "single-best-healthy";
}

export interface TopologyRoute {
  nodeId: string;
  destination: string;
  gateway?: string;
  interfaceName: string;
  metric?: number;
  table: number;
  family: string;
}

export interface TopologyNeighbor {
  nodeId: string;
  address: string;
  macAddress?: string;
  interfaceName: string;
  state: string;
  router?: boolean;
}

export interface TopologyLLDPNeighbor {
  nodeId: string;
  localInterface: string;
  chassisId: string;
  portId?: string;
  systemName?: string;
  managementAddresses?: string[];
}

export interface NetworkPathProbe {
  nodeId: string;
  targetNodeId: string;
  sourceInterface: string;
  sourceAddress: string;
  targetInterface: string;
  targetAddress: string;
  sourceSpeedBitsPerSecond?: number;
  targetSpeedBitsPerSecond?: number;
  latencyMicros?: number;
  reachable: boolean;
  failure?: string;
  observedAt: string;
}

export interface TopologySnapshot {
  version: number;
  generatedAt: string;
  reportCount: number;
  vertices: TopologyVertex[];
  edges: TopologyEdge[];
  pathSelections?: NetworkPathSelection[];
  routes?: TopologyRoute[];
  neighbors?: TopologyNeighbor[];
  lldpNeighbors?: TopologyLLDPNeighbor[];
  pathProbes?: NetworkPathProbe[];
}

export interface NetworkFlow {
	family: "ipv4" | "ipv6";
	protocol: "tcp" | "udp";
	sourceAddress: string;
	sourcePort: number;
	destinationAddress: string;
	destinationPort: number;
	direction: "ingress" | "egress";
	packets: number;
	bytes: number;
}

export interface NodeFlowObservation {
	messageId: string;
	nodeId: string;
	flows: NetworkFlow[];
	truncated: boolean;
	mapEntries: number;
	interval: number;
	observedAt: string;
	receivedAt: string;
}

export interface Alert {
  id: string;
  fingerprint: string;
  kind: "interface_health" | "job_storage" | "group_storage" | "node_storage";
  sourceNodeId: string;
  sourceInterface: string;
  status: "firing" | "acknowledged" | "resolved";
  severity: "info" | "warning" | "critical";
  observedHealth: "healthy" | "degraded" | "critical";
  summary: string;
  observedBits: number;
  droppedPacketsPerSecond: number;
  errorPacketsPerSecond: number;
  dropRatio: number;
  firstObservedAt: string;
  lastObservedAt: string;
  firingAt?: string;
  healthySince?: string;
  acknowledgedAt?: string;
  acknowledgedBy?: string;
  resolvedAt?: string;
  revision: number;
}

export interface UsageCost {
  cpuCostMicros: number;
  memoryCostMicros: number;
  gpuCostMicros: number;
  storageCostMicros: number;
  totalCostMicros: number;
}

export interface DailyUsageCost {
  date: string;
  ownerId: string;
  ownerName?: string;
  cpuUsageMicros: number;
  memoryByteSeconds: number;
  gpuDeviceSeconds: number;
  wallTimeMillis: number;
  workspaceByteSeconds: number;
  peakWorkspaceBytes: number;
  cost: UsageCost;
}

export interface UsageSummary {
  ownerId?: string;
  ownerName?: string;
  from: string;
  to: string;
  currency: string;
  cpuUsageMicros: number;
  memoryByteSeconds: number;
  gpuDeviceSeconds: number;
  wallTimeMillis: number;
  workspaceByteSeconds: number;
  peakWorkspaceBytes: number;
  cost: UsageCost;
  daily: DailyUsageCost[];
}

export interface BillingRate {
  id?: string;
  currency: string;
  cpuCoreHourMicros: number;
  memoryGiBHourMicros: number;
  gpuDeviceHourMicros: number;
  storageGiBDayMicros: number;
  effectiveAt?: string;
  createdAt?: string;
  createdBy?: string;
}

export interface AlertWebhookStatus {
  enabled: boolean;
  destination?: string;
}

export interface AlertDelivery {
  id: string;
  eventType: "alert.firing" | "alert.updated" | "alert.acknowledged" | "alert.resolved";
  alertId: string;
  alertRevision: number;
  alert: Alert;
  state: "pending" | "retrying" | "delivered" | "failed";
  attempts: number;
  responseStatus?: number;
  lastError?: string;
  createdAt: string;
  nextAttemptAt?: string;
  lastAttemptAt?: string;
  deliveredAt?: string;
}
