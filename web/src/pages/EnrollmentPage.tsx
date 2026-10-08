import { FormEvent, useCallback, useEffect, useState } from "react";
import { approveDiscoveryCandidate, getClusterJoinToken, getDiscoveryState, rejectDiscoveryCandidate, startDiscoveryScan } from "../api/client";
import type { ClusterJoinToken, DiscoveryBootstrap, DiscoveryCandidate, DiscoveryScan } from "../api/contracts";
import { relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

export function EnrollmentPage({ token }: { token: string }) {
  const { t } = usePreferences();
  const [scans, setScans] = useState<DiscoveryScan[]>([]);
  const [candidates, setCandidates] = useState<DiscoveryCandidate[]>([]);
  const [address, setAddress] = useState("");
  const [bootstrap, setBootstrap] = useState<DiscoveryBootstrap | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [joinToken, setJoinToken] = useState<ClusterJoinToken | null>(null);
  const [pairingCodes, setPairingCodes] = useState<Record<string, string>>({});

  const refresh = useCallback(async (signal?: AbortSignal) => {
    const state = await getDiscoveryState(token, signal);
    setScans(state.scans); setCandidates(state.candidates);
  }, [token]);

  useEffect(() => {
    const controller = new AbortController();
    refresh(controller.signal).catch((cause) => !controller.signal.aborted && setError(cause instanceof Error ? cause.message : "Unable to load enrollment state"));
    return () => controller.abort();
  }, [refresh]);

  async function scan(event: FormEvent) {
    event.preventDefault(); setBusy("scan"); setError("");
    try {
      await startDiscoveryScan([address.trim()], token);
      setAddress(""); await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Discovery scan failed"); }
    finally { setBusy(""); }
  }

  async function decide(candidate: DiscoveryCandidate, approve: boolean) {
    setBusy(candidate.id); setError("");
    try {
      if (approve) setBootstrap(await approveDiscoveryCandidate(candidate.id, pairingCodes[candidate.id] ?? "", token));
      else await rejectDiscoveryCandidate(candidate.id, token);
      await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Candidate update failed"); }
    finally { setBusy(""); }
  }

  const pending = candidates.filter((candidate) => candidate.status === "discovered");
  return <section>
    <header className="page-header"><div><h1>{t("节点发现与接入", "Node discovery and enrollment")}</h1></div></header>
    {error && <p className="error-banner" role="alert">{error}</p>}
    <div className="bootstrap-panel"><header><div><h2>{t("工作节点加入令牌", "Worker join token")}</h2></div><div className="inline-actions"><button type="button" onClick={() => getClusterJoinToken(token).then(setJoinToken).catch((cause) => setError(String(cause)))}>{t("查看", "View")}</button><button className="danger-button" type="button" onClick={() => window.confirm(t("重置后旧令牌立即失效，确认继续？", "The old token is invalidated immediately. Continue?")) && getClusterJoinToken(token, true).then(setJoinToken).catch((cause) => setError(String(cause)))}>{t("重置", "Reset")}</button></div></header>{joinToken ? <><code>{joinToken.secret}</code><p>{t(`有效至 ${new Date(joinToken.token.expiresAt).toLocaleString()} · 已接入 ${joinToken.token.uses} 个节点。只通过可信通道分发。`, `Valid until ${new Date(joinToken.token.expiresAt).toLocaleString()} · used by ${joinToken.token.uses} nodes. Distribute only over trusted channels.`)}</p></> : <p>{t("令牌由控制节点首次启动时生成，只有管理员能够查看或重置。", "The token is generated on first controller startup and only administrators can view or reset it.")}</p>}</div>
    <div className="enrollment-hero">
      <form onSubmit={scan}>
        <h2>{t("扫描 XIRANG Agent", "Scan a XIRANG Agent")}</h2>
        <p>{t("只需输入一个 IPv4 或 IPv6 地址。主控将连接该地址的 Agent 引导端口并读取待审批状态。", "Enter one IPv4 or IPv6 address. The controller connects to the Agent bootstrap port and reads its approval state.")}</p>
        <div><input value={address} onChange={(event) => setAddress(event.target.value)} placeholder="192.0.2.25" required /><button className="primary-button" disabled={busy === "scan"}>{busy === "scan" ? t("扫描中…", "Scanning…") : t("扫描 Agent", "Scan Agent")}<span aria-hidden="true">→</span></button></div>
      </form>
      <div className="enrollment-stats"><article><span>{t("待审批", "Pending approval")}</span><strong>{pending.length}</strong><small>{t("等待管理员决定", "Awaiting administrator action")}</small></article><article><span>{t("历史扫描", "Scan history")}</span><strong>{scans.length}</strong><small>{scans[0] ? relativeTime(scans[0].startedAt) : t("尚未扫描", "Not scanned yet")}</small></article><article><span>{t("已接入候选", "Approved candidates")}</span><strong>{candidates.filter((item) => item.status === "approved").length}</strong><small>{t("审批后自动注册", "Registers automatically after approval")}</small></article></div>
    </div>
    {bootstrap && <div className="bootstrap-panel">
      <header><div><span className="eyebrow">{t("节点已自动接入", "Node enrolled automatically")}</span><h2>{bootstrap.candidate.hostname || bootstrap.candidate.address}</h2></div><button type="button" onClick={() => setBootstrap(null)}>{t("关闭", "Close")}</button></header>
      <p>{t("主控已使用扫描时记录的 TLS 指纹确认 Agent，并通过一次性凭据完成注册。节点 ID：", "The controller verified the Agent with its recorded TLS fingerprint and enrolled it using a one-time credential. Node ID: ")}<code>{bootstrap.nodeId || bootstrap.candidate.enrolledNodeId}</code></p>
    </div>}
    <div className="candidate-grid">
      {candidates.map((candidate) => <article className={`candidate-card candidate-card--${candidate.status}`} key={candidate.id}>
        <header><span className="status-dot" /><div><small>{candidate.status.toUpperCase()}</small><h2>{candidate.hostname || candidate.address}</h2></div><time>{relativeTime(candidate.lastSeenAt)}</time></header>
        <div className="candidate-ports"><span>{candidate.address}:{candidate.agentPort}</span><span>{candidate.architecture || t("架构待识别", "Architecture pending")}</span><span>{candidate.cpuCount || 0} CPU</span></div>
        <p>{candidate.distribution || "Linux"} · Agent {candidate.agentVersion || t("未知版本", "unknown version")} · {t(`${candidate.interfaces?.length || 0} 个网络接口`, `${candidate.interfaces?.length || 0} network interfaces`)}</p>
        {candidate.fingerprint && <code title={t("扫描时记录的 Agent TLS 证书 SHA-256 指纹", "Agent TLS certificate SHA-256 fingerprint recorded during discovery")}>{candidate.fingerprint}</code>}
        {candidate.status === "discovered" && <><label className="pairing-code-field"><span>{t("节点端一次性配对码", "One-time code shown on the node")}</span><input value={pairingCodes[candidate.id] ?? ""} onChange={(event) => setPairingCodes((current) => ({ ...current, [candidate.id]: event.target.value.toUpperCase() }))} placeholder="ABCD-EFGH-JKLM-NPQR" autoComplete="off" /></label><footer><button className="danger-button" type="button" onClick={() => decide(candidate, false)} disabled={busy === candidate.id}>{t("拒绝", "Reject")}</button><button className="primary-button" type="button" onClick={() => decide(candidate, true)} disabled={busy === candidate.id || (candidate.pairingRequired !== false && !(pairingCodes[candidate.id] ?? "").trim())}>{t("允许接入", "Approve enrollment")}</button></footer></>}
      </article>)}
      {candidates.length === 0 && <div className="queue-empty"><strong>{t("没有候选节点", "No candidate nodes")}</strong><span>{t("在子节点启动 Agent 后输入其 IP 地址", "Start the Agent on a node, then enter its IP address")}</span></div>}
    </div>
    <div className="scan-history">{scans.slice(0, 8).map((scan) => <div key={scan.id}><code>{scan.cidrs.join(", ")}</code><span>{scan.scannedHosts} hosts</span><strong>{scan.candidatesSeen} candidates</strong><small>{scan.status} · {relativeTime(scan.startedAt)}</small></div>)}</div>
  </section>;
}
