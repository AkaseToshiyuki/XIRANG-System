import { useEffect, useMemo, useState } from "react";
import { getNodeInventory, getNodeObservations, getNodes, getTopology } from "../api/client";
import type { ClusterNode, NodeResourceObservation, TopologySnapshot } from "../api/contracts";
import { MetricCard } from "../components/MetricCard";
import { formatBitRate, formatBytes, percentage, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";
import { aggregateBidirectionalRate, buildObservedLinks, nodeRate, type NodeRate } from "./TopologyPage";

interface NodeSample {
  node: ClusterNode;
  observations: NodeResourceObservation[];
}

interface OverviewPageProps {
  token: string;
  onNavigate: (page: "Topology" | "Nodes") => void;
}

export function OverviewPage({ token, onNavigate }: OverviewPageProps) {
  const { refreshMs, t } = usePreferences();
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [samples, setSamples] = useState<NodeSample[]>([]);
  const [rates, setRates] = useState<Record<string, NodeRate>>({});
  const [topology, setTopology] = useState<TopologySnapshot | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    const refresh = async () => {
      try {
        const [nodeItems, topologySnapshot] = await Promise.all([
          getNodes(token, controller.signal),
          getTopology(token, undefined, controller.signal).catch((cause) => {
            if (cause instanceof Error && cause.message === "No topology snapshot has been reported yet") return null;
            throw cause;
          }),
        ]);
        const currentNodes = nodeItems.filter((node) => node.status !== "rejected");
        const observationResults = await Promise.all(currentNodes.map(async (node) => {
            try {
              const [observations, inventory] = await Promise.all([
                getNodeObservations(node.id, token, 2, controller.signal),
                getNodeInventory(node.id, token, controller.signal),
              ]);
              return { sample: { node, observations }, rate: nodeRate(observations, inventory.networkInterfaces ?? []) };
            } catch {
              return { sample: { node, observations: [] }, rate: { interfaces: [] } as NodeRate };
            }
          }));
        if (!controller.signal.aborted) {
          setNodes(currentNodes);
          setSamples(observationResults.map((result) => result.sample));
          setRates(Object.fromEntries(observationResults.map((result) => [result.sample.node.id, result.rate])));
          setTopology(topologySnapshot);
          setUpdatedAt(new Date());
          setError("");
        }
      } catch (cause) {
        if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Unable to load cluster overview");
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    };
    void refresh();
    const interval = window.setInterval(() => void refresh(), refreshMs);
    return () => {
      controller.abort();
      window.clearInterval(interval);
    };
  }, [token, refreshMs]);

  const summary = useMemo(() => {
    let usedCPU = 0;
    let totalCPU = 0;
    let usedMemory = 0;
    let totalMemory = 0;
    let measuredCPU = 0;
    let measuredMemory = 0;
    let measuredNodes = 0;
    for (const sample of samples) {
      const latest = sample.observations[0];
      totalCPU += sample.node.capacity.milliCpu;
      totalMemory += sample.node.capacity.memoryBytes;
      if (!latest) continue;
      measuredNodes++;
      measuredCPU += sample.node.capacity.milliCpu;
      measuredMemory += sample.node.capacity.memoryBytes;
      usedCPU += latest.usedMilliCpu;
      usedMemory += latest.usedMemoryBytes;
    }
    const links = buildObservedLinks(nodes, rates, topology, {});
    return {
      usedCPU, totalCPU, measuredCPU, usedMemory, totalMemory, measuredMemory, measuredNodes,
      networkBits: aggregateBidirectionalRate(links),
      networkLinks: links.length, links,
    };
  }, [nodes, rates, samples, topology]);

  const loadedNodes = useMemo(() => samples.map((sample) => {
    const latest = sample.observations[0];
    return {
      ...sample,
      cpu: latest ? percentage(latest.usedMilliCpu, sample.node.capacity.milliCpu) : 0,
      memory: latest ? percentage(latest.usedMemoryBytes, sample.node.capacity.memoryBytes) : 0,
    };
  }).sort((left, right) => Math.max(right.cpu, right.memory) - Math.max(left.cpu, left.memory)).slice(0, 5), [samples]);

  const ready = nodes.filter((node) => node.status === "ready").length;
  const pending = nodes.filter((node) => node.status === "pending_approval").length;

  return <section>
    <header className="page-header">
      <div>
        <h1>{t("集群概览", "Cluster overview")}</h1>
      </div>
      <div className={`live-indicator ${error ? "live-indicator--warning" : ""}`}><span />{error ? t("数据刷新异常", "Refresh failed") : updatedAt ? `${t("更新于", "Updated at")} ${updatedAt.toLocaleTimeString()}` : t("正在连接", "Connecting")}</div>
    </header>

    {error && <p className="error-banner" role="alert">{error}</p>}

    <section className="metrics-grid" aria-label={t("集群指标", "Cluster metrics")}>
      <MetricCard label={t("纳管节点", "Managed nodes")} value={loading ? "—" : `${ready} / ${nodes.length}`} detail={pending ? t(`${pending} 个等待审批`, `${pending} waiting for approval`) : t("就绪 / 总数", "Ready / total")} />
      <MetricCard label={t("CPU 利用率", "CPU utilization")} value={summary.measuredNodes ? `${percentage(summary.usedCPU, summary.measuredCPU).toFixed(0)}%` : "—"} detail={t(`可见 ${(summary.totalCPU / 1000).toFixed(0)} 个逻辑核心`, `${(summary.totalCPU / 1000).toFixed(0)} logical cores visible`)} tone="violet" progress={percentage(summary.usedCPU, summary.measuredCPU)} />
      <MetricCard label={t("内存利用率", "Memory utilization")} value={summary.measuredNodes ? `${percentage(summary.usedMemory, summary.measuredMemory).toFixed(0)}%` : "—"} detail={t(`已用 ${formatBytes(summary.usedMemory)} · 可见 ${formatBytes(summary.totalMemory)}`, `${formatBytes(summary.usedMemory)} used · ${formatBytes(summary.totalMemory)} visible`)} tone="amber" progress={percentage(summary.usedMemory, summary.measuredMemory)} />
      <MetricCard label={t("网络活动", "Network activity")} value={summary.measuredNodes > 0 ? formatBitRate(summary.networkBits) : "—"} detail={t(`${summary.networkLinks} 条连接 · 双向速率`, `${summary.networkLinks} ${summary.networkLinks === 1 ? "link" : "links"} · bidirectional`)} />
    </section>

    <section className="content-grid">
      <article className="panel overview-topology">
        <div className="panel-heading">
          <div><h2>{t("节点连接", "Node links")}</h2></div>
          <button className="text-button" type="button" onClick={() => onNavigate("Topology")}>{t("查看完整拓扑", "View full topology")} <span aria-hidden="true">→</span></button>
        </div>
        <div className="overview-links">
          {summary.links.slice(0, 5).map((link) => <div className="overview-link" key={link.id}>
            <div><strong>{link.source.name} <span aria-hidden="true">↔</span> {link.target.name}</strong><small>{link.primaryInterface || t("接口不可用", "Interface unavailable")}</small></div>
            <span>{link.forwardBits !== undefined && link.reverseBits !== undefined ? formatBitRate(link.forwardBits + link.reverseBits) : "—"}</span>
          </div>)}
          {!summary.links.length && <div className="compact-empty">{loading ? t("正在读取连接…", "Loading links…") : t("暂无节点连接数据", "No node links available")}</div>}
        </div>
      </article>

      <article className="panel load-panel">
        <div className="panel-heading">
          <div><h2>{t("负载最高节点", "Highest-load nodes")}</h2></div>
          <button className="icon-button" type="button" aria-label={t("管理节点", "Manage nodes")} onClick={() => onNavigate("Nodes")}>↗</button>
        </div>
        <div className="node-list">
          {loadedNodes.map(({ node, observations, cpu, memory }) => <div className="node-row" key={node.id}>
            <div className="node-identity"><span className={`state-dot state-dot--${node.status}`} /><div><strong>{node.name}</strong><span>{node.architecture} · {relativeTime(node.lastSeenAt)}</span></div></div>
            <div className="dual-load">
              <div><span>CPU</span><i><b style={{ width: `${cpu}%` }} /></i><strong>{observations[0] ? `${cpu.toFixed(0)}%` : "—"}</strong></div>
              <div><span>RAM</span><i><b style={{ width: `${memory}%` }} /></i><strong>{observations[0] ? `${memory.toFixed(0)}%` : "—"}</strong></div>
            </div>
          </div>)}
          {!loading && loadedNodes.length === 0 && <div className="compact-empty">{t("还没有节点遥测数据", "No node telemetry is available yet")}</div>}
        </div>
      </article>
    </section>
  </section>;
}
