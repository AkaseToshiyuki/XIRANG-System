import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getNodeInventory, getNodeObservations, getNodes, getTopology } from "../api/client";
import type { ClusterNode, NetworkInterfaceSnapshot, NetworkPathProbe, NodeResourceObservation, TopologyNeighbor, TopologyRoute, TopologySnapshot } from "../api/contracts";
import { clockTime, formatBitRate, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

interface Position { x: number; y: number }
interface InterfaceRate { name: string; addresses: string[]; receivedBits?: number; transmittedBits?: number; speedBits?: number }
export interface NodeRate { receivedBits?: number; transmittedBits?: number; observedAt?: string; interfaces: InterfaceRate[] }
export interface ManagedLink {
  id: string;
  source: ClusterNode;
  target: ClusterNode;
  forwardBits?: number;
  reverseBits?: number;
  negotiatedBits?: number;
  latencyMicros?: number;
  primaryInterface?: string;
  primaryPath?: string;
  fallbackInterfaces: string[];
  fallbackPaths: string[];
  fallback: "none" | "available" | "active";
  observedAt?: string;
  health: "healthy" | "attention" | "offline";
}
type Selection = { kind: "node" | "link"; id: string } | null;
export function TopologyPage({ token }: { token: string }) {
  const { refreshMs, t } = usePreferences();
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [rates, setRates] = useState<Record<string, NodeRate>>({});
  const [topology, setTopology] = useState<TopologySnapshot | null>(null);
  const [selection, setSelection] = useState<Selection>(null);
  const [lastRefreshedAt, setLastRefreshedAt] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const strongestPrimary = useRef<Record<string, { capacity: number; path: string }>>({});

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      const [nodeItems, snapshot] = await Promise.all([
        getNodes(token, signal),
        getTopology(token, undefined, signal).catch((cause) => {
          if (cause instanceof Error && cause.message === "No topology snapshot has been reported yet") return null;
          throw cause;
        }),
      ]);
      const registered = nodeItems.filter((node) => node.status !== "rejected")
        .sort((left, right) => Date.parse(left.createdAt) - Date.parse(right.createdAt) || left.id.localeCompare(right.id));
      const measurements = await Promise.all(registered.map(async (node) => {
        try {
          const [observations, inventory] = await Promise.all([
            getNodeObservations(node.id, token, 2, signal),
            getNodeInventory(node.id, token, signal),
          ]);
          return [node.id, nodeRate(observations, inventory.networkInterfaces ?? [])] as const;
        } catch {
          return [node.id, { interfaces: [] } as NodeRate] as const;
        }
      }));
      if (signal?.aborted) return;
      setNodes(registered);
      setRates(Object.fromEntries(measurements));
      setTopology(snapshot);
      setLastRefreshedAt(new Date().toISOString());
      setError("");
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : t("无法读取节点拓扑", "Unable to load node topology"));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [token, t]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    const timer = window.setInterval(() => void load(controller.signal), refreshMs);
    return () => { controller.abort(); window.clearInterval(timer); };
  }, [load, refreshMs]);

  const links = useMemo(() => buildObservedLinks(nodes, rates, topology, strongestPrimary.current), [nodes, rates, topology]);
  useEffect(() => {
    for (const link of links) {
      if (link.health === "offline") continue;
      const current = strongestPrimary.current[link.id];
      if (!current || (link.negotiatedBits ?? 0) >= current.capacity) {
        strongestPrimary.current[link.id] = { capacity: link.negotiatedBits ?? 0, path: link.primaryPath ?? link.primaryInterface ?? "" };
      }
    }
  }, [links]);
  const positions = useMemo(() => layoutNodes(nodes), [nodes]);
  const selectedNode = selection?.kind === "node" ? nodes.find((node) => node.id === selection.id) ?? null : null;
  const selectedLink = selection?.kind === "link" ? links.find((link) => link.id === selection.id) ?? null : null;
  const readyNodes = nodes.filter((node) => node.status === "ready").length;
  const healthyLinks = links.filter((link) => link.health === "healthy").length;
  const currentRate = aggregateBidirectionalRate(links);

  return <section>
    <header className="page-header">
      <div><h1>{t("网络拓扑", "Network topology")}</h1></div>
      <div className={`live-indicator ${error ? "live-indicator--warning" : ""}`} title={error || undefined}><span />{lastRefreshedAt ? `${t("更新于", "Updated at")} ${clockTime(lastRefreshedAt)}` : t("等待更新", "Waiting for update")}</div>
    </header>
    {error && <p className="error-banner" role="alert">{error}</p>}
    {loading && nodes.length === 0 ? <TopologyAwaiting t={t} /> : nodes.length === 0 ? <TopologyEmpty t={t} /> : <>
      <div className="topology-metrics topology-metrics--managed">
        <TopologyMetric label={t("已加入节点", "Enrolled nodes")} value={nodes.length} />
        <TopologyMetric label={t("当前就绪", "Currently ready")} value={`${readyNodes} / ${nodes.length}`} tone={readyNodes < nodes.length ? "attention" : undefined} />
        <TopologyMetric label={t("节点连接", "Node links")} value={links.length} />
        <TopologyMetric label={t("健康连接", "Healthy links")} value={`${healthyLinks} / ${links.length}`} tone={healthyLinks < links.length ? "attention" : undefined} />
        <TopologyMetric label={t("双向交换总速率", "Aggregate bidirectional rate")} value={formatBitRate(currentRate)} />
      </div>
      <div className="topology-workspace">
        <div className="topology-stage">
          <div className="graph-toolbar graph-toolbar--managed"><span><i className="graph-key graph-key--healthy" />{t("连接正常", "Healthy")}</span><span><i className="graph-key graph-key--attention" />{t("节点受限", "Node restricted")}</span><span><i className="graph-key graph-key--offline" />{t("连接不可用", "Unavailable")}</span><small>{t("选择连接可查看两个方向对应的节点与速率", "Select a link to inspect both directions and rates")}</small></div>
          <svg className="topology-graph" viewBox="0 0 960 540" role="img" aria-label={t("XIRANG 节点网络拓扑", "XIRANG node network topology")}>
            {links.map((link) => {
              const source = positions[link.source.id], target = positions[link.target.id];
              const midpoint = { x: (source.x + target.x) / 2, y: (source.y + target.y) / 2 };
              return <g key={link.id} className={selectedLink?.id === link.id ? "graph-edge-group selected" : "graph-edge-group"} onClick={() => setSelection({ kind: "link", id: link.id })}>
                <line className="graph-edge-hitbox" x1={source.x} y1={source.y} x2={target.x} y2={target.y} />
                <line className={`graph-edge graph-edge--managed graph-edge--managed-${link.health}`} style={{ strokeWidth: edgeStrokeWidth((link.forwardBits ?? 0) + (link.reverseBits ?? 0)) }} x1={source.x} y1={source.y} x2={target.x} y2={target.y} />
                <text className="graph-link-rate" x={midpoint.x} y={midpoint.y - 15} textAnchor="middle">↑ {formatLinkRate(link, link.forwardBits, t)} · ↓ {formatLinkRate(link, link.reverseBits, t)}</text>
                <text className="graph-link-capacity" x={midpoint.x} y={midpoint.y + 3} textAnchor="middle">{negotiatedLabel(link, t, t("协商", "Negotiated"))}</text>
                {link.fallback !== "none" && <text className={`graph-link-fallback graph-link-fallback--${link.fallback}`} x={midpoint.x} y={midpoint.y + 20} textAnchor="middle">{link.fallback === "active" ? t("主链路失效", "Primary link failed") : fallbackAvailableLabel(link.fallbackInterfaces.length, t)}</text>}
              </g>;
            })}
            {nodes.map((node) => {
              const position = positions[node.id];
              const management = effectiveRoles(node).includes("management");
              return <g className={`graph-vertex graph-vertex--managed graph-vertex--state-${nodeVisualState(node)}${selectedNode?.id === node.id ? " selected" : ""}`} key={node.id} transform={`translate(${position.x} ${position.y})`} role="button" tabIndex={0} onClick={() => setSelection({ kind: "node", id: node.id })}>
                <circle className="graph-vertex-halo" r="39" /><circle r="27" /><text textAnchor="middle" y="3">{management ? "CTRL" : "NODE"}</text>
                <text className="graph-label" textAnchor="middle" y="48">{node.name}</text>
                <text className="graph-node-rate" textAnchor="middle" y="61">{roleText(node, t)}</text>
              </g>;
            })}
          </svg>
          <div className="graph-footnote"><span>{t(`${nodes.length} 个已加入节点`, `${nodes.length} enrolled nodes`)}</span><span>{lastRefreshedAt ? `${t("更新于", "Updated at")} ${clockTime(lastRefreshedAt)}` : t("等待更新", "Waiting for update")}</span></div>
        </div>
        <TopologyInspector node={selectedNode} link={selectedLink} rate={selectedNode ? rates[selectedNode.id] : undefined} t={t} />
      </div>
      <article className="topology-list managed-link-list">
        <header><div><h2>{t("双向速率与链路选择", "Bidirectional rates and path selection")}</h2></div><small>{t("最新采样", "Latest sample")}</small></header>
        {links.map((link) => <button type="button" key={link.id} onClick={() => setSelection({ kind: "link", id: link.id })}>
          <strong>{link.source.name} <b>↔</b> {link.target.name}</strong><span>{linkPathLabel(link, t)} · {negotiatedLabel(link, t, t("协商", "negotiated"))}</span>
          <small>{link.source.name} → {formatLinkRate(link, link.forwardBits, t)} · {link.target.name} → {formatLinkRate(link, link.reverseBits, t)} · {fallbackLabel(link, t)}{link.fallbackPaths.length ? t(` · 备用：${link.fallbackPaths.join("；")}`, ` · fallback: ${link.fallbackPaths.join("; ")}`) : ""}</small>
        </button>)}
      </article>
    </>}
  </section>;
}

function TopologyInspector({ node, link, rate, t }: { node: ClusterNode | null; link: ManagedLink | null; rate?: NodeRate; t: (zh: string, en: string) => string }) {
  if (link) return <aside className="topology-inspector"><span className="eyebrow">{t("连接详情", "Link details")}</span><h2>{link.source.name} ↔ {link.target.name}</h2><div className={`health-badge health-badge--managed-${link.health}`}><i />{linkHealthLabel(link.health, t)}</div><dl>
    <div><dt>{link.source.name} → {link.target.name}</dt><dd>{formatLinkRate(link, link.forwardBits, t)}</dd></div><div><dt>{link.target.name} → {link.source.name}</dt><dd>{formatLinkRate(link, link.reverseBits, t)}</dd></div>
    <div><dt>{t("主接口", "Primary interface")}</dt><dd>{link.health === "offline" ? unavailableLabel(t) : link.primaryInterface ?? t("等待识别", "Pending detection")}</dd></div><div><dt>{t("协商速率", "Negotiated rate")}</dt><dd>{formatLinkRate(link, link.negotiatedBits, t)}</dd></div>
    <div><dt>{t("Ping 延迟", "Ping latency")}</dt><dd>{link.health === "offline" ? unavailableLabel(t) : formatLatency(link.latencyMicros, t)}</dd></div>
    <div><dt>Fallback</dt><dd>{fallbackLabel(link, t)}</dd></div><div><dt>{t("备用接口", "Fallback interfaces")}</dt><dd>{link.fallbackInterfaces.join(t("、", ", ")) || t("无", "None")}</dd></div>
    <div><dt>{t("最新采样", "Latest sample")}</dt><dd>{link.observedAt ? relativeTime(link.observedAt) : t("等待遥测", "Waiting for telemetry")}</dd></div>
  </dl><p>{t("链路由两端内核路由或邻居证据验证；双向速率来自两端接口连续采样，协商速率来自网卡报告值。", "Links are verified by routing or neighbor evidence at both endpoints. Bidirectional rates come from consecutive interface samples and negotiated rates from NIC reports.")}</p></aside>;
  if (node) return <aside className="topology-inspector"><span className="eyebrow">{t("节点详情", "Node details")}</span><h2>{node.name}</h2><div className={`vertex-kind vertex-kind--managed-${nodeVisualState(node)}`}>{node.status}</div><dl>
    <div><dt>{t("角色", "Roles")}</dt><dd>{roleText(node, t)}</dd></div><div><dt>{t("架构", "Architecture")}</dt><dd>{node.architecture}</dd></div><div><dt>{t("接收速率", "Receive rate")}</dt><dd>{nodeAccessUnavailable(node) ? unavailableLabel(t) : formatRate(rate?.receivedBits, t)}</dd></div><div><dt>{t("发送速率", "Transmit rate")}</dt><dd>{nodeAccessUnavailable(node) ? unavailableLabel(t) : formatRate(rate?.transmittedBits, t)}</dd></div><div><dt>{t("最后在线", "Last seen")}</dt><dd>{relativeTime(node.lastSeenAt)}</dd></div>
  </dl></aside>;
  return <aside className="topology-inspector topology-inspector--empty"><div className="inspector-symbol">⌖</div><h2>{t("检查节点或连接", "Inspect a node or link")}</h2><p>{t("点击图中的节点或连接查看当前状态。", "Select a node or link in the graph to inspect its current state.")}</p></aside>;
}

export function nodeRate(observations: NodeResourceObservation[], inventory: NetworkInterfaceSnapshot[]): NodeRate {
  const current = observations[0], previous = observations[1];
  if (!current || !previous) return { observedAt: current?.observedAt, interfaces: inventory.map((item) => ({ name: item.name, addresses: item.addresses ?? [], speedBits: item.speedBitsPerSecond })) };
  const seconds = (Date.parse(current.observedAt) - Date.parse(previous.observedAt)) / 1000;
  const byName = new Map(inventory.map((item) => [item.name, item]));
  const names = new Set([...Object.keys(current.interfaceCounters ?? {}), ...inventory.map((item) => item.name)]);
  const interfaces = [...names].map((name) => {
    const now = current.interfaceCounters?.[name], before = previous.interfaceCounters?.[name], info = byName.get(name);
    const valid = seconds > 0 && now && before && now.receivedBytes >= before.receivedBytes && now.transmittedBytes >= before.transmittedBytes;
    return { name, addresses: info?.addresses ?? [], speedBits: info?.speedBitsPerSecond, receivedBits: valid ? (now.receivedBytes - before.receivedBytes) * 8 / seconds : undefined, transmittedBits: valid ? (now.transmittedBytes - before.transmittedBytes) * 8 / seconds : undefined };
  }).filter((item) => item.name !== "lo");
  return {
    receivedBits: interfaces.reduce((sum, item) => sum + (item.receivedBits ?? 0), 0),
    transmittedBits: interfaces.reduce((sum, item) => sum + (item.transmittedBits ?? 0), 0),
    observedAt: current.observedAt, interfaces,
  };
}

export function buildObservedLinks(nodes: ClusterNode[], rates: Record<string, NodeRate>, topology: TopologySnapshot | null, previousPrimary: Record<string, { capacity: number; path: string }>): ManagedLink[] {
  if (nodes.length < 2 || !topology) return [];
  const links: ManagedLink[] = [];
  for (let sourceIndex = 0; sourceIndex < nodes.length; sourceIndex += 1) {
    for (let targetIndex = sourceIndex + 1; targetIndex < nodes.length; targetIndex += 1) {
      const source = nodes[sourceIndex], target = nodes[targetIndex];
      const paths = discoverPaths(
        source, target,
        rates[source.id]?.interfaces ?? [], rates[target.id]?.interfaces ?? [],
        topology.routes ?? [], topology.neighbors ?? [], topology.pathProbes ?? [],
      );
      const selection = findPathSelection(topology, source.id, target.id);
      const selectedEdge = selection ? topology.edges.find((edge) => edge.id === selection.primaryEdgeId) : undefined;
      const selectedIndex = selectedEdge ? paths.findIndex((path) => pathMatchesEdge(path, selectedEdge, source.id)) : 0;
      const selected = paths[selectedIndex >= 0 ? selectedIndex : 0];
      if (!selected) continue;
      const fallbacks = paths.filter((_, index) => index !== (selectedIndex >= 0 ? selectedIndex : 0));
      const linkID = `${source.id}:${target.id}`;
      const health = linkHealth(source, target);
      const accessible = health !== "offline";
      const primaryFailed = topology.edges.some((edge) => edgeConnects(edge, source.id, target.id) && edge.health === "critical" &&
        (edge.capacityBits ?? 0) > (selected.capacityBits ?? 0)) || (previousPrimary[linkID]?.capacity ?? 0) > (selected.capacityBits ?? 0);
      links.push({
        id: linkID, source, target,
        forwardBits: accessible ? selected.forwardBits : undefined, reverseBits: accessible ? selected.reverseBits : undefined,
        negotiatedBits: accessible ? selected.capacityBits : undefined, latencyMicros: accessible ? selected.latencyMicros : undefined,
        primaryInterface: accessible ? interfacePathLabel(selected) : undefined,
        primaryPath: accessible ? pathLabel(selected) : undefined,
        fallbackInterfaces: accessible ? fallbacks.map(interfacePathLabel) : [],
        fallbackPaths: accessible ? fallbacks.map((item) => `${pathLabel(item)} (${formatRate(item.capacityBits)} · ${formatLatency(item.latencyMicros)})`) : [],
        fallback: accessible ? primaryFailed ? "active" : fallbacks.length ? "available" : "none" : "none",
        observedAt: newestTimestamp(rates[source.id]?.observedAt, rates[target.id]?.observedAt),
        health,
      });
    }
  }
  return links;
}

export function aggregateBidirectionalRate(links: ManagedLink[]) {
  return links.reduce((total, link) => total + (link.forwardBits ?? 0) + (link.reverseBits ?? 0), 0);
}

interface DiscoveredPath {
  id: string;
  source: InterfaceRate;
  target: InterfaceRate;
  capacityBits?: number;
  forwardBits?: number;
  reverseBits?: number;
  activity: number;
  latencyMicros?: number;
  sourceAddress?: string;
  targetAddress?: string;
}

function discoverPaths(
  sourceNode: ClusterNode,
  targetNode: ClusterNode,
  sourceInterfaces: InterfaceRate[],
  targetInterfaces: InterfaceRate[],
  routes: TopologyRoute[],
  neighbors: TopologyNeighbor[],
  probes: NetworkPathProbe[],
): DiscoveredPath[] {
  const measured = measuredPaths(sourceNode, targetNode, sourceInterfaces, targetInterfaces, probes);
  if (measured.length > 0) return sortPaths(measured);
  const paths: DiscoveredPath[] = [];
  for (const source of sourceInterfaces) for (const target of targetInterfaces) {
    if (source.name === "lo" || target.name === "lo") continue;
    const directNeighbors = hasNeighborEvidence(sourceNode.id, source, target, neighbors) &&
      hasNeighborEvidence(targetNode.id, target, source, neighbors);
    const bidirectionalRoutes = hasRouteEvidence(sourceNode.id, source, target, routes) &&
      hasRouteEvidence(targetNode.id, target, source, routes);
    if (!directNeighbors && !bidirectionalRoutes) continue;
    paths.push(makePath(source, target));
  }
  return sortPaths(paths);
}

function measuredPaths(
  sourceNode: ClusterNode,
  targetNode: ClusterNode,
  sourceInterfaces: InterfaceRate[],
  targetInterfaces: InterfaceRate[],
  probes: NetworkPathProbe[],
) {
  const paths = new Map<string, DiscoveredPath>();
  for (const probe of probes) {
    if (!probe.reachable) continue;
    const forward = probe.nodeId === sourceNode.id && probe.targetNodeId === targetNode.id;
    const reverse = probe.nodeId === targetNode.id && probe.targetNodeId === sourceNode.id;
    if (!forward && !reverse) continue;
    const sourceName = forward ? probe.sourceInterface : probe.targetInterface;
    const targetName = forward ? probe.targetInterface : probe.sourceInterface;
    const source = sourceInterfaces.find((item) => item.name === sourceName);
    const target = targetInterfaces.find((item) => item.name === targetName);
    if (!source || !target) continue;
    const path = makePath(source, target);
    if ((probe.sourceSpeedBitsPerSecond ?? 0) > 0 && (probe.targetSpeedBitsPerSecond ?? 0) > 0) {
      path.capacityBits = Math.min(probe.sourceSpeedBitsPerSecond!, probe.targetSpeedBitsPerSecond!);
    } else {
      path.capacityBits = undefined;
    }
    path.latencyMicros = probe.latencyMicros;
    path.sourceAddress = forward ? probe.sourceAddress : probe.targetAddress;
    path.targetAddress = forward ? probe.targetAddress : probe.sourceAddress;
    path.id = `${source.name}:${path.sourceAddress}:${target.name}:${path.targetAddress}`;
    // Retain the fastest observed address pair as evidence for an interface
    // pair. IPv6 privacy/stable addresses on one NIC are not independent
    // fallback links and must not inflate the visible topology.
    const key = `${source.name}\x00${target.name}`;
    const existing = paths.get(key);
    if (!existing || betterMeasuredPath(path, existing)) {
      paths.set(key, path);
    }
  }
  return [...paths.values()];
}

function betterMeasuredPath(left: DiscoveredPath, right: DiscoveredPath) {
  return (left.capacityBits ?? 0) > (right.capacityBits ?? 0) ||
    (left.capacityBits ?? 0) === (right.capacityBits ?? 0) &&
    (left.latencyMicros ?? Number.MAX_SAFE_INTEGER) < (right.latencyMicros ?? Number.MAX_SAFE_INTEGER);
}

function sortPaths(paths: DiscoveredPath[]) {
  return paths.sort((left, right) =>
    (right.capacityBits ?? 0) - (left.capacityBits ?? 0) ||
    (left.latencyMicros ?? Number.MAX_SAFE_INTEGER) - (right.latencyMicros ?? Number.MAX_SAFE_INTEGER) ||
    right.activity - left.activity);
}

function makePath(source: InterfaceRate, target: InterfaceRate): DiscoveredPath {
  const forwardBits = pairedRate(source.transmittedBits, target.receivedBits);
  const reverseBits = pairedRate(source.receivedBits, target.transmittedBits);
  return {
    id: `${source.name}:${target.name}`, source, target,
    capacityBits: source.speedBits && target.speedBits ? Math.min(source.speedBits, target.speedBits) : undefined,
    forwardBits, reverseBits, activity: (forwardBits ?? 0) + (reverseBits ?? 0),
  };
}

function pairedRate(left?: number, right?: number) {
  if (left === undefined) return right;
  if (right === undefined) return left;
  return Math.min(left, right);
}

function hasNeighborEvidence(nodeId: string, local: InterfaceRate, remote: InterfaceRate, neighbors: TopologyNeighbor[]) {
  const remoteAddresses = new Set(remote.addresses.map(addressOnly));
  return neighbors.some((neighbor) =>
    neighbor.nodeId === nodeId && neighbor.interfaceName === local.name &&
    usableNeighborState(neighbor.state) && remoteAddresses.has(addressOnly(neighbor.address)));
}

function hasRouteEvidence(nodeId: string, local: InterfaceRate, remote: InterfaceRate, routes: TopologyRoute[]) {
  return routes.some((route) =>
    route.nodeId === nodeId && route.interfaceName === local.name &&
    route.destination !== "0.0.0.0/0" && route.destination !== "::/0" &&
    remote.addresses.some((address) => routeContainsAddress(route.destination, addressOnly(address))));
}

function usableNeighborState(state: string) {
  return !["failed", "incomplete", "noarp"].includes(state.toLowerCase());
}

function routeContainsAddress(destination: string, address: string) {
  const [network, prefixText] = destination.split("/");
  if (!network || prefixText === undefined) return addressOnly(destination) === address;
  const prefix = Number(prefixText);
  const networkV4 = ipv4Number(network), addressV4 = ipv4Number(address);
  if (networkV4 !== undefined && addressV4 !== undefined && prefix > 0 && prefix <= 32) {
    const mask = prefix === 32 ? 0xffffffff : (0xffffffff << (32 - prefix)) >>> 0;
    return (networkV4 & mask) === (addressV4 & mask);
  }
  return prefix === 128 && normalizeAddress(network) === normalizeAddress(address);
}

function addressOnly(value: string) { return normalizeAddress(value.split("/")[0]); }
function normalizeAddress(value: string) { return value.trim().toLowerCase(); }
function newestTimestamp(left?: string, right?: string) {
  if (!left) return right;
  if (!right) return left;
  return Date.parse(left) >= Date.parse(right) ? left : right;
}

// "1 fallback links available" was the plural falling out of a template string.
// The Chinese measure word does not inflect, so only the English side branches.
function fallbackAvailableLabel(count: number, t: (zh: string, en: string) => string) {
  return t(`${count} 条备用链路可用`, count === 1 ? "1 fallback link available" : `${count} fallback links available`);
}

function linkRateSampled(link: ManagedLink, rate: number | undefined) {
  return link.health !== "offline" && rate !== undefined;
}

// formatLinkRate returns either a rate or a status phrase, so "Negotiated" was
// running straight into "Waiting for sample" as though it were one sentence.
// A separator keeps the field name and its state readable as two things.
function negotiatedLabel(link: ManagedLink, t: (zh: string, en: string) => string, heading: string) {
  const value = formatLinkRate(link, link.negotiatedBits, t);
  return linkRateSampled(link, link.negotiatedBits) ? `${heading} ${value}` : `${heading} · ${value}`;
}

function ipv4Number(value: string) {
  const parts = value.split(".").map(Number);
  if (parts.length !== 4 || parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) return undefined;
  return (((parts[0] << 24) >>> 0) + (parts[1] << 16) + (parts[2] << 8) + parts[3]) >>> 0;
}

function effectiveRoles(node: ClusterNode) { return node.roles?.length ? node.roles : ["compute"]; }
function roleText(node: ClusterNode, t: (zh: string, en: string) => string) { return effectiveRoles(node).map((role) => t(({ management: "管理", compute: "计算", storage: "存储" }[role] ?? role), ({ management: "Management", compute: "Compute", storage: "Storage" }[role] ?? role))).join(" · "); }
function fallbackLabel(link: ManagedLink, t: (zh: string, en: string) => string) { return link.health === "offline" ? t("节点不可访问，备用链路状态未知", "Node unavailable; fallback status unknown") : link.fallback === "active" ? t("主链路失效，备用链路已接管", "Primary link failed; fallback is active") : link.fallback === "available" ? fallbackAvailableLabel(link.fallbackInterfaces.length, t) : t("无备用链路", "No fallback link"); }
function findPathSelection(topology: TopologySnapshot, sourceID: string, targetID: string) {
  return (topology.pathSelections ?? []).find((selection) =>
    selection.sourceId === sourceID && selection.targetId === targetID ||
    selection.sourceId === targetID && selection.targetId === sourceID);
}
function edgeConnects(edge: TopologySnapshot["edges"][number], sourceID: string, targetID: string) {
  return edge.sourceId === sourceID && edge.targetId === targetID || edge.sourceId === targetID && edge.targetId === sourceID;
}
function pathMatchesEdge(path: DiscoveredPath, edge: TopologySnapshot["edges"][number], sourceID: string) {
  const forward = edge.sourceId === sourceID;
  const sourceInterface = forward ? edge.sourceInterface : edge.targetInterface;
  const targetInterface = forward ? edge.targetInterface : edge.sourceInterface;
  return (!sourceInterface || path.source.name === sourceInterface) && (!targetInterface || path.target.name === targetInterface);
}
function nodeAccessUnavailable(node: ClusterNode) { return ["offline", "unreachable", "removed", "rejected"].includes(node.status); }
function linkHealth(source: ClusterNode, target: ClusterNode): ManagedLink["health"] { return nodeAccessUnavailable(source) || nodeAccessUnavailable(target) ? "offline" : source.status === "ready" && target.status === "ready" ? "healthy" : "attention"; }
function nodeVisualState(node: ClusterNode): "ready" | "attention" | "offline" { return nodeAccessUnavailable(node) ? "offline" : node.status === "ready" ? "ready" : "attention"; }
function linkHealthLabel(health: ManagedLink["health"], t: (zh: string, en: string) => string) { return t(health === "healthy" ? "连接正常" : health === "attention" ? "节点受限" : "连接不可用", health === "healthy" ? "Healthy" : health === "attention" ? "Node restricted" : "Unavailable"); }
function layoutNodes(nodes: ClusterNode[]): Record<string, Position> {
  const result: Record<string, Position> = {};
  if (!nodes.length) return result;
  if (nodes.length === 1) {
    result[nodes[0].id] = { x: 480, y: 270 };
    return result;
  }
  nodes.forEach((node, index) => {
    const angle = index / nodes.length * Math.PI * 2 - Math.PI / 2;
    result[node.id] = { x: 480 + Math.cos(angle) * 285, y: 270 + Math.sin(angle) * 185 };
  });
  return result;
}
function edgeStrokeWidth(rate: number) { return !rate ? 2 : Math.min(7, 2 + Math.log10(rate + 1) * .55); }
function formatRate(rate?: number, t?: (zh: string, en: string) => string) { return rate === undefined ? (t ? t("等待采样", "Waiting for sample") : "—") : formatBitRate(rate); }
function unavailableLabel(t: (zh: string, en: string) => string) { return t("不可用", "Unavailable"); }
function formatLinkRate(link: ManagedLink, rate: number | undefined, t: (zh: string, en: string) => string) { return link.health === "offline" ? unavailableLabel(t) : formatRate(rate, t); }
function linkPathLabel(link: ManagedLink, t: (zh: string, en: string) => string) { return link.health === "offline" ? t("节点不可访问", "Node unavailable") : link.primaryPath ?? link.primaryInterface ?? t("接口待识别", "Interface pending"); }
function pathLabel(path: DiscoveredPath) {
  const source = path.sourceAddress ? `${path.source.name} ${path.sourceAddress}` : path.source.name;
  const target = path.targetAddress ? `${path.target.name} ${path.targetAddress}` : path.target.name;
  return `${source} ↔ ${target}`;
}
function interfacePathLabel(path: DiscoveredPath) { return `${path.source.name} ↔ ${path.target.name}`; }
function formatLatency(micros?: number, t?: (zh: string, en: string) => string) {
  if (!micros || micros <= 0) return t ? t("等待探测", "Waiting for probe") : "—";
  return micros < 1000 ? `${micros} μs` : `${(micros / 1000).toFixed(micros < 10_000 ? 2 : 1)} ms`;
}
function TopologyAwaiting({ t }: { t: (zh: string, en: string) => string }) { return <div className="topology-awaiting"><span className="pulse-ring" /><strong>{t("正在载入节点", "Loading nodes")}</strong><small>{t("读取节点状态、接口速度与最新网络计数", "Reading node state, interface speed and current network counters")}</small></div>; }
function TopologyEmpty({ t }: { t: (zh: string, en: string) => string }) { return <div className="topology-awaiting"><span className="pulse-ring" /><strong>{t("尚无已加入节点", "No enrolled nodes")}</strong><small>{t("节点完成注册后会出现在此处", "Nodes appear here after enrollment")}</small></div>; }
function TopologyMetric({ label, value, tone }: { label: string; value: number | string; tone?: "attention" }) { return <div className={tone ? `topology-metric--${tone}` : ""}><span>{label}</span><strong>{value}</strong></div>; }
