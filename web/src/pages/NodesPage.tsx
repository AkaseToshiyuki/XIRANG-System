import { Fragment, useCallback, useEffect, useMemo, useState } from "react";
import { getNodeInventory, getNodeObservations, getNodes, removeNode, revokeNodeCertificate, rotateNodeCertificate, setNodeMode, setNodeTaints, updateNodeConfiguration, updateNodeStatus, watchNodes } from "../api/client";
import type { ClusterNode, NodeInventorySnapshot, NodeResourceObservation } from "../api/contracts";
import { clockTime, formatByteRate, formatBytes, networkDirectionBytesPerSecond, percentage, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

interface LiveResources {
  latest: NodeResourceObservation;
  networkRates?: { received: number; transmitted: number };
}

export function NodesPage({ token, isAdmin }: { token: string; isAdmin: boolean }) {
  const { refreshMs, t } = usePreferences();
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [live, setLive] = useState(false);
  const [lastUpdatedAt, setLastUpdatedAt] = useState("");
  const [resources, setResources] = useState<Record<string, LiveResources>>({});
  const [inventories, setInventories] = useState<Record<string, NodeInventorySnapshot>>({});
  const [query, setQuery] = useState("");
  const [statusFilter, setStatusFilter] = useState("all");
  const [taintEditorID, setTaintEditorID] = useState("");
  const [taintDraft, setTaintDraft] = useState("");
  const [configurationEditorID, setConfigurationEditorID] = useState("");
  const [configurationDraft, setConfigurationDraft] = useState<{ roles: Array<"management" | "compute" | "storage">; managementAddress: string; sshUser: string; group: string; computePriority: number }>({ roles: ["compute"], managementAddress: "", sshUser: "", group: "", computePriority: 0 });

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      setNodes(await getNodes(token, signal));
      setLastUpdatedAt(new Date().toISOString());
      setError("");
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : "Unable to load nodes");
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  useEffect(() => {
    const controller = new AbortController();
    let retry: number | undefined;
    const connect = async () => {
      try {
        await watchNodes(token, (node) => {
          if (node.removedAt) {
            setNodes((current) => current.filter((item) => item.id !== node.id));
            return;
          }
          setNodes((current) => current.some((item) => item.id === node.id)
            ? current.map((item) => item.id === node.id ? node : item)
            : [...current, node]);
          setLastUpdatedAt(new Date().toISOString());
        }, () => setLive(true), controller.signal);
      } catch {
        // A failed stream and a clean EOF both follow the same reconnect path.
      }
      if (!controller.signal.aborted) {
        setLive(false);
        retry = window.setTimeout(connect, 1500);
      }
    };
    void connect();
    return () => {
      controller.abort();
      if (retry !== undefined) window.clearTimeout(retry);
      setLive(false);
    };
  }, [token]);

  const nodeIDs = nodes.map((node) => node.id).sort().join(",");
  useEffect(() => {
    if (!nodeIDs) {
      setResources({});
      setInventories({});
      return;
    }
    const controller = new AbortController();
    const refresh = async () => {
      const updates = await Promise.all(nodes.map(async (node) => {
        try {
          const [observations, inventory] = await Promise.all([
            getNodeObservations(node.id, token, 2, controller.signal),
            getNodeInventory(node.id, token, controller.signal).catch(() => undefined),
          ]);
          return { nodeId: node.id, inventory, resource: observations.length ? { latest: observations[0], networkRates: networkDirectionBytesPerSecond(observations) } : undefined };
        } catch {
          return undefined;
        }
      }));
      if (!controller.signal.aborted) {
        const next: Record<string, LiveResources> = {};
        const nextInventories: Record<string, NodeInventorySnapshot> = {};
        for (const entry of updates) {
          if (entry?.resource) next[entry.nodeId] = entry.resource;
          if (entry?.inventory) nextInventories[entry.nodeId] = entry.inventory;
        }
        setResources(next);
        setInventories(nextInventories);
      }
    };
    void refresh();
    const interval = window.setInterval(() => void refresh(), refreshMs);
    return () => {
      controller.abort();
      window.clearInterval(interval);
    };
  }, [token, nodeIDs, refreshMs]);

  const statuses = useMemo(() => [...new Set(nodes.map((node) => node.status))].sort(), [nodes]);
  const filteredNodes = useMemo(() => nodes.filter((node) => {
    const normalized = query.trim().toLowerCase();
    const matchesQuery = !normalized || [node.name, node.id, node.architecture, node.distribution, node.os].some((value) => value?.toLowerCase().includes(normalized));
    return matchesQuery && (statusFilter === "all" || node.status === statusFilter);
  }), [nodes, query, statusFilter]);

  async function updateNode(node: ClusterNode, operation: () => Promise<ClusterNode>, fallback: string) {
    setError("");
    try {
      const updated = await operation();
      setNodes((current) => current.map((item) => item.id === updated.id ? updated : item));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : fallback);
    }
  }

  function act(node: ClusterNode, action: "approve" | "reject") {
    return updateNode(node, () => updateNodeStatus(node.id, action, token), "Node action failed");
  }

  async function revoke(node: ClusterNode) {
    if (!window.confirm(`Revoke the certificate for ${node.name}? The Agent will immediately lose access.`)) return;
    await updateNode(node, () => revokeNodeCertificate(node.id, token), "Certificate revocation failed");
  }

  async function remove(node: ClusterNode) {
    const confirmed = window.confirm(t(
      `从集群中移除“${node.name}”？节点将从运行视图中消失，证书会被吊销，但任务和审计历史会保留。`,
      `Remove “${node.name}” from the cluster? The node disappears from operational views and its certificate is revoked, while job and audit history are retained.`,
    ));
    if (!confirmed) return;
    setError("");
    try {
      const removed = await removeNode(node.id, token);
      setNodes((current) => current.filter((item) => item.id !== removed.id));
      setResources((current) => Object.fromEntries(Object.entries(current).filter(([id]) => id !== removed.id)));
      setInventories((current) => Object.fromEntries(Object.entries(current).filter(([id]) => id !== removed.id)));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("节点移除失败", "Node removal failed"));
    }
  }

  function rotateCertificate(node: ClusterNode) {
    return updateNode(node, () => rotateNodeCertificate(node.id, token), "Certificate rotation failed");
  }

  function changeMode(node: ClusterNode, mode: "ready" | "cordoned" | "draining") {
    return updateNode(node, () => setNodeMode(node.id, mode, token), "Node mode command failed");
  }

  function openTaintEditor(node: ClusterNode) {
    setTaintEditorID(node.id);
    setTaintDraft((node.taints ?? []).map((taint) => `${taint.key}${taint.value ? `=${taint.value}` : ""}:${taint.effect}`).join("\n"));
  }

  async function saveTaints(node: ClusterNode) {
    try {
      const taints = taintDraft.split("\n").map((line) => line.trim()).filter(Boolean).map((line) => {
        const separator = line.lastIndexOf(":");
        if (separator < 1) throw new Error(t(`无效污点：${line}`, `Invalid taint: ${line}`));
        const effectText = line.slice(separator + 1);
        const effect: "NoSchedule" | "NoExecute" = effectText === "NoSchedule" || effectText === "NoExecute" ? effectText : (() => { throw new Error(t(`无效 effect：${effectText}`, `Invalid effect: ${effectText}`)); })();
        const [key, value] = line.slice(0, separator).split("=", 2);
        if (!key) throw new Error(t(`无效污点：${line}`, `Invalid taint: ${line}`));
        return { key, value: value || undefined, effect };
      });
      await updateNode(node, () => setNodeTaints(node.id, taints, token), "Node taint update failed");
      setTaintEditorID("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Node taint update failed");
    }
  }

  function openConfiguration(node: ClusterNode) {
    setConfigurationEditorID(node.id);
    setConfigurationDraft({
      roles: node.roles?.length ? node.roles : ["compute"],
      managementAddress: node.managementAddress ?? "",
      sshUser: node.sshUser ?? "",
      group: node.group ?? "",
      computePriority: node.computePriority ?? 0,
    });
  }

  async function saveConfiguration(node: ClusterNode) {
    if (configurationDraft.roles.length === 0) {
      setError(t("节点至少需要一个角色", "A node must have at least one role"));
      return;
    }
    await updateNode(node, () => updateNodeConfiguration(node.id, configurationDraft, token), t("节点配置更新失败", "Node configuration update failed"));
    setConfigurationEditorID("");
  }

  const readyCount = nodes.filter((node) => node.status === "ready").length;
  const pendingCount = nodes.filter((node) => node.status === "pending_approval").length;

  return <section>
    <header className="page-header">
      <div>
        <h1>{t("节点状态", "Node status")}</h1>
      </div>
      <div className={`live-indicator ${live ? "" : "live-indicator--warning"}`} title={live ? undefined : t("节点事件流重连中", "Node event stream reconnecting")}><span />{lastUpdatedAt ? `${t("更新于", "Updated at")} ${clockTime(lastUpdatedAt)}` : t("等待更新", "Waiting for update")}</div>
    </header>

    <div className="summary-strip" aria-label={t("节点摘要", "Node summary")}>
      <div><span>{t("全部节点", "All nodes")}</span><strong>{nodes.length}</strong></div>
      <div><span>{t("运行就绪", "Ready")}</span><strong>{readyCount}</strong></div>
      <div className={pendingCount ? "summary-strip--attention" : ""}><span>{t("等待审批", "Pending approval")}</span><strong>{pendingCount}</strong></div>
      <div><span>{t("遥测在线", "Telemetry online")}</span><strong>{Object.keys(resources).length}</strong></div>
    </div>

    <div className="nodes-toolbar">
      <label className="search-field"><span aria-hidden="true">⌕</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t("搜索名称、ID、架构或发行版", "Search name, ID, architecture or distribution")} aria-label={t("搜索节点", "Search nodes")} /></label>
      <label className="select-field"><span>{t("状态", "Status")}</span><select value={statusFilter} onChange={(event) => setStatusFilter(event.target.value)}><option value="all">{t("全部", "All")}</option>{statuses.map((status) => <option key={status} value={status}>{statusLabel(status, t)}</option>)}</select></label>
      <button className="secondary-button" type="button" onClick={() => void load()} disabled={loading}>{loading ? t("刷新中…", "Refreshing…") : t("刷新", "Refresh")}</button>
    </div>

    {error && <p className="error-banner" role="alert">{error}</p>}
    <div className="nodes-table" role="table" aria-label={t("集群节点", "Cluster nodes")} tabIndex={0}>
      <div className="nodes-table__header" role="row">
        <span role="columnheader">{t("节点", "Node")}</span><span role="columnheader">{t("宿主机", "Host")}</span><span role="columnheader">{t("容量与能力", "Capacity")}</span><span role="columnheader">{t("实时资源", "Live resources")}</span><span role="columnheader">{t("温度", "Temperature")}</span><span role="columnheader">{t("状态", "Status")}</span><span role="columnheader" aria-label={t("操作", "Actions")}>•••</span>
      </div>
      {filteredNodes.map((node) => {
        const resource = resources[node.id];
        const inventory = inventories[node.id];
        const cpu = resource ? percentage(resource.latest.usedMilliCpu, node.capacity.milliCpu) : 0;
        const memory = resource ? percentage(resource.latest.usedMemoryBytes, node.capacity.memoryBytes) : 0;
        const accelerators = resource?.latest.accelerators ?? [];
        const panelState = managementPanelState(node, t);
        return <Fragment key={node.id}><div className="nodes-table__row" role="row">
          <span role="cell" className="node-name-cell"><i className={`state-dot state-dot--${node.status}`} /><span><strong>{node.name}</strong><small title={node.id}>{shortID(node.id)}</small><span className="node-role-list">{effectiveRoles(node).map((role) => <i key={role}>{roleLabel(role, t)}</i>)}</span>{node.roles?.includes("management") && <small className={`management-panel-state${panelState.ready ? " management-panel-state--ready" : ""}`} title={`${t("此节点上报的面板状态，不代表整个集群控制服务的健康状态。", "Panel state reported by this node, not the health of the cluster control service.")} ${node.managementPanelStatus || ""}`.trim()}>{panelState.label}</small>}</span></span>
          <span role="cell"><strong>{node.architecture}</strong><small>{node.distribution || node.os} · {node.kernelVersion || "kernel unknown"}</small><small>{node.cpuModel || inventory?.cpuModel || t("CPU 型号待探测", "CPU model pending")}</small><span className="cpu-features" title={(node.cpuFeatures ?? inventory?.cpuFeatures ?? []).join(", ")}>{importantCPUFeatures(node.cpuFeatures ?? inventory?.cpuFeatures ?? []).map((feature) => <i key={feature}>{feature}</i>)}</span></span>
          <span role="cell"><strong>{node.capacity.milliCpu / 1000} CPU</strong><small>{formatBytes(node.capacity.memoryBytes)} RAM · {node.numaTopology?.length ?? 0} NUMA</small><small title={computeWeightDetail(inventory)}>{t("自动权重", "Auto weight")} {(node.computeWeight ?? inventory?.computeWeight ?? 0).toFixed(2)} · {t("优先级", "Priority")} {node.computePriority ?? 0}</small>{inventory?.devices?.filter((device) => device.vendor).map((device) => <span className="device-model" key={`${device.resourceName}-${device.deviceId}`} title={gpuDetail(device)}>{device.model || device.vendor?.toUpperCase()}</span>)}{Object.entries(node.capacity.devices ?? {}).length > 0 && <span className="device-list">{Object.entries(node.capacity.devices ?? {}).map(([name, quantity]) => <i key={name} title={deviceLocality(node, name)}>{shortResourceName(name)} × {quantity}</i>)}</span>}</span>
          <span role="cell" className="resource-cell">
            {resource ? <><div><small>CPU</small><i><b style={{ width: `${cpu}%` }} /></i><strong>{cpu.toFixed(0)}%</strong></div><div><small>RAM</small><i><b style={{ width: `${memory}%` }} /></i><strong>{memory.toFixed(0)}%</strong></div>{accelerators.map((accelerator, index) => <div className="gpu-resource-row" key={accelerator.id || `${accelerator.vendor}-${index}`} title={`${accelerator.vendor.toUpperCase()} · ${accelerator.id}`}><small>GPU{index}</small><i><b className="gpu-bar" style={{ width: `${accelerator.utilizationAvailable ? accelerator.utilizationPercent ?? 0 : 0}%` }} /></i><strong>{accelerator.utilizationAvailable ? `${(accelerator.utilizationPercent ?? 0).toFixed(0)}%` : "—"}</strong></div>)}{resource.networkRates ? <span className="network-direction-rates"><span>{t("上传", "Upload")} <b>↑</b> {formatByteRate(resource.networkRates.transmitted)}</span><span>{t("下载", "Download")} <b>↓</b> {formatByteRate(resource.networkRates.received)}</span></span> : <em>{t("正在计算网络速率", "Calculating network rate")}</em>}</> : <><strong>{t("等待采样", "Waiting for sample")}</strong><small>{t("等待 Agent 上报资源", "Waiting for Agent telemetry")}</small></>}
          </span>
          <span role="cell" className="temperature-cell">
            <span title={resource?.latest.cpuTemperatureSource ? `${resource.latest.cpuTemperatureSource} · ${resource.latest.cpuTemperatureConfidence || "unknown confidence"}` : undefined}><small>CPU</small><strong>{formatTemperature(resource?.latest.cpuTemperatureAvailable || (resource?.latest.cpuTemperatureC ?? 0) > 0 ? resource?.latest.cpuTemperatureC : undefined, t)}</strong></span>
            {accelerators.map((accelerator, index) => <span key={accelerator.id || `${accelerator.vendor}-${index}`} title={`${accelerator.vendor.toUpperCase()} · ${accelerator.id}`}><small>GPU{index}</small><strong>{formatTemperature(accelerator.temperatureAvailable ? accelerator.temperatureC : undefined, t)}</strong></span>)}
          </span>
          <span role="cell"><strong className={`node-state node-state--${node.status}`}>{statusLabel(node.status, t)}</strong><small>{node.desiredStatus ? `${t("正在切换", "Transitioning")} → ${statusLabel(node.desiredStatus, t)}` : relativeTime(node.lastSeenAt)}</small>{(node.taints?.length ?? 0) > 0 && <span className="taint-list">{node.taints?.map((taint) => <i key={`${taint.key}-${taint.effect}`} title={`${taint.key}${taint.value ? `=${taint.value}` : ""}:${taint.effect}`}>{taint.key} · {taint.effect}</i>)}</span>}</span>
          <span role="cell" className="node-actions">
            {isAdmin && <details className="action-menu"><summary aria-label={`${t("操作", "Actions")} ${node.name}`}>•••</summary><div>
              {node.status === "pending_approval" && <><button type="button" onClick={() => void act(node, "approve")}>{t("批准", "Approve")}</button><button type="button" className="danger" onClick={() => void act(node, "reject")}>{t("拒绝", "Reject")}</button></>}
              {!node.pendingModeCommandId && node.status === "ready" && <button type="button" onClick={() => void changeMode(node, "cordoned")}>{t("隔离", "Cordon")}</button>}
              {!node.pendingModeCommandId && node.status === "cordoned" && <><button type="button" onClick={() => void changeMode(node, "ready")}>{t("恢复", "Resume")}</button><button type="button" onClick={() => void changeMode(node, "draining")}>{t("排空", "Drain")}</button></>}
              {!node.pendingModeCommandId && node.status === "draining" && <button type="button" onClick={() => void changeMode(node, "cordoned")}>{t("停止排空", "Stop draining")}</button>}
              <button type="button" onClick={(event) => { const menu = event.currentTarget.closest("details"); if (menu) menu.open = false; openConfiguration(node); }}>{t("角色、连接与优先级", "Roles, access and priority")}</button>
              <button type="button" onClick={(event) => { const menu = event.currentTarget.closest("details"); if (menu) menu.open = false; openTaintEditor(node); }}>{t("调度约束", "Scheduling constraints")}</button>
              {node.certificateSerial && !node.certificateRevokedAt && node.status !== "pending_approval" && <><button type="button" disabled={Boolean(node.pendingCertificateCommandId)} onClick={() => void rotateCertificate(node)}>{node.pendingCertificateCommandId ? t("轮换中…", "Rotating…") : t("轮换证书", "Rotate certificate")}</button><button type="button" className="danger" onClick={() => void revoke(node)}>{t("吊销证书", "Revoke certificate")}</button></>}
              {(node.status === "offline" || node.status === "rejected") && <button type="button" className="danger" onClick={() => void remove(node)}>{t("移除节点", "Remove node")}</button>}
            </div></details>}
          </span>
        </div>{isAdmin && configurationEditorID === node.id && <div role="row"><div role="cell" aria-colspan={7}><div className="node-editor node-configuration-editor">
          <header><strong>{t("节点角色与管理连接", "Node roles and management access")}</strong><small>{t("启用管理角色会在该节点开放 8899 端口的 XIRANG 控制面板；移除管理角色会关闭由 Agent 托管的面板。只有计算节点会接收任务。", "The management role exposes the XIRANG panel on port 8899 of this node; removing it stops the Agent-managed panel. Only compute nodes receive jobs.")}</small></header>
          <div className="node-configuration-fields">
            <fieldset><legend>{t("节点角色", "Node roles")}</legend>{(["management", "compute", "storage"] as const).map((role) => <label key={role}><input type="checkbox" checked={configurationDraft.roles.includes(role)} disabled={Boolean(node.pendingManagementCommandId)} onChange={(event) => setConfigurationDraft((current) => ({ ...current, roles: event.target.checked ? [...current.roles, role] : current.roles.filter((item) => item !== role) }))} />{roleLabel(role, t)}</label>)}</fieldset>
            <label><span>{t("管理地址", "Management address")}</span><input value={configurationDraft.managementAddress} onChange={(event) => setConfigurationDraft({ ...configurationDraft, managementAddress: event.target.value })} placeholder={t("192.0.2.25 或 node.example", "192.0.2.25 or node.example")} /></label>
            <label><span>{t("SSH 用户", "SSH user")}</span><input value={configurationDraft.sshUser} onChange={(event) => setConfigurationDraft({ ...configurationDraft, sshUser: event.target.value })} placeholder="yang" /></label>
            <label><span>{t("节点分组", "Node group")}</span><input value={configurationDraft.group} onChange={(event) => setConfigurationDraft({ ...configurationDraft, group: event.target.value })} placeholder={t("gpu 或 storage-a", "gpu or storage-a")} /></label>
            <label><span>{t("计算优先级（-1000 至 1000）", "Compute priority (-1000 to 1000)")}</span><input type="number" min={-1000} max={1000} value={configurationDraft.computePriority} onChange={(event) => setConfigurationDraft({ ...configurationDraft, computePriority: Number(event.target.value) })} /></label>
          </div>
          <div className="taint-editor__actions"><button type="button" onClick={() => setConfigurationEditorID("")}>{t("取消", "Cancel")}</button><button type="button" disabled={Boolean(node.pendingManagementCommandId)} onClick={() => void saveConfiguration(node)}>{node.pendingManagementCommandId ? t("正在切换控制面板…", "Panel transitioning…") : t("保存配置", "Save configuration")}</button></div>
        </div></div></div>}{isAdmin && taintEditorID === node.id && <div role="row"><div role="cell" aria-colspan={7}><div className="node-editor taint-editor">
          <header><strong>{t("节点污点", "Node taints")}</strong><small>{t("每行填写 key[=value]:NoSchedule 或 key[=value]:NoExecute。NoExecute 当前阻止新任务；不会强制终止运行中任务。", "Enter one key[=value]:NoSchedule or key[=value]:NoExecute per line. NoExecute currently blocks new jobs and does not terminate running jobs.")}</small></header>
          <textarea value={taintDraft} onChange={(event) => setTaintDraft(event.target.value)} placeholder={"dedicated=gpu:NoSchedule\nmaintenance:NoExecute"} aria-label={t("节点污点", "Node taints")} />
          <div className="taint-editor__actions"><button type="button" onClick={() => setTaintEditorID("")}>{t("取消", "Cancel")}</button><button type="button" onClick={() => void saveTaints(node)}>{t("保存约束", "Save constraints")}</button></div>
        </div></div></div>}</Fragment>;
      })}
      {!loading && filteredNodes.length === 0 && <div role="row"><div role="cell" aria-colspan={7} className="table-empty"><strong>{nodes.length ? t("没有匹配的节点", "No matching nodes") : t("尚未纳管节点", "No enrolled nodes")}</strong><span>{nodes.length ? t("尝试调整搜索条件或状态筛选。", "Adjust the search terms or status filter.") : t("启动 Agent 后，新节点会进入等待审批状态。", "After an Agent starts, the new node enters pending approval.")}</span></div></div>}
      {loading && nodes.length === 0 && <div className="table-loading" aria-hidden="true"><span /><span /><span /></div>}
    </div>
  </section>;
}

function effectiveRoles(node: ClusterNode): Array<"management" | "compute" | "storage"> {
  return node.roles?.length ? node.roles : ["compute"];
}

function managementPanelState(node: ClusterNode, t: (zh: string, en: string) => string): { label: string; ready: boolean } {
  if (node.status === "offline" || node.status === "rejected" || node.status === "pending_approval") return { label: t("面板状态未知", "Panel state unknown"), ready: false };
  if (node.pendingManagementCommandId) return { label: t("控制面板切换中", "Panel transitioning"), ready: false };
  if (node.managementPanelStatus?.startsWith("error:")) return { label: t("面板操作失败", "Panel operation failed"), ready: false };
  if (node.managementPanelEnabled) return { label: node.managementPanelStatus === "controller" ? t("独立控制服务已识别", "Independent controller detected") : t("控制面板已启用", "Panel enabled"), ready: true };
  if (node.managementPanelStatus === "disabled") return { label: t("节点面板未启用", "Node panel disabled"), ready: false };
  if (node.managementPanelStatus === "externally managed") return { label: t("面板由外部服务管理", "Panel managed externally"), ready: false };
  return { label: t("面板状态未上报", "Panel state not reported"), ready: false };
}

function computeWeightDetail(inventory?: NodeInventorySnapshot): string {
  if (!inventory) return "CPU benchmark + Σ(NVIDIA SM compute score ÷ 1000)";
  const gpuScore = (inventory.devices ?? []).reduce((total, device) => total + Number(device.attributes?.smComputeScore ?? 0) / 1000, 0);
  return `CPU ${inventory.cpuBenchmarkScore?.toFixed(2) ?? "0.00"} + GPU ${gpuScore.toFixed(2)} = ${(inventory.computeWeight ?? 0).toFixed(2)}`;
}

function roleLabel(role: "management" | "compute" | "storage", t: (zh: string, en: string) => string) {
  return t(({ management: "管理", compute: "计算", storage: "存储" })[role], ({ management: "Management", compute: "Compute", storage: "Storage" })[role]);
}

function statusLabel(status: string, t: (zh: string, en: string) => string): string {
  return t(({ ready: "就绪", pending_approval: "待审批", rejected: "已拒绝", cordoned: "已隔离", draining: "排空中" } as Record<string, string>)[status] ?? status, ({ ready: "Ready", pending_approval: "Pending approval", rejected: "Rejected", cordoned: "Cordoned", draining: "Draining" } as Record<string, string>)[status] ?? status);
}

function shortID(value: string): string {
  return value.length > 20 ? `${value.slice(0, 17)}…` : value;
}

function shortResourceName(value: string): string {
  return value.replace("xirang.io/", "").replace("nvidia.com/", "NVIDIA ");
}

function deviceLocality(node: ClusterNode, resource: string): string {
  const topology = node.deviceTopology?.[resource];
  if (!topology) return "NUMA locality unavailable";
  return Object.entries(topology).map(([numa, quantity]) => `NUMA ${numa}: ${quantity}`).join(" · ");
}

function formatTemperature(value: number | undefined, t: (zh: string, en: string) => string): string {
  return value && value > 0 ? `${value.toFixed(0)}°C` : t("未暴露", "Unavailable");
}

function gpuDetail(device: NonNullable<NodeInventorySnapshot["devices"]>[number]): string {
  const attributes = device.attributes ?? {};
  return [
    device.model,
    attributes.computeCapability ? `SM ${attributes.computeCapability}` : "",
    attributes.smCount ? `${attributes.smCount} SM` : "",
    attributes.smComputeScore ? `${attributes.smComputeScore} CUDA cores` : "",
  ].filter(Boolean).join(" · ");
}

function importantCPUFeatures(features: string[]): string[] {
  const preferred = ["avx512f", "avx2", "avx", "fma", "sse4_2", "aes", "sha_ni", "sve", "sve2", "asimd", "neon"];
  const result = preferred.filter((feature) => features.includes(feature));
  return result.length ? result.slice(0, 5) : features.slice(0, 4);
}
