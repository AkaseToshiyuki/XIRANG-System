import { FormEvent, useEffect, useState } from "react";
import { ApiRequestError, addSystemNote, getBMCAction, getNodeHealth, getNodes, getSystemEvents, requestBMCAction } from "../api/client";
import type { BMCAction, ClusterNode, NodeHealthSnapshot, SystemEvent } from "../api/contracts";
import { usePreferences } from "../app/preferences";

export function HealthPage({ token }: { token: string }) {
  const { t, refreshSeconds } = usePreferences();
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [nodeID, setNodeID] = useState("");
  const [snapshot, setSnapshot] = useState<NodeHealthSnapshot | null>(null);
  const [events, setEvents] = useState<SystemEvent[]>([]);
  const [kind, setKind] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [note, setNote] = useState("");
  const [related, setRelated] = useState("");
  const [saving, setSaving] = useState(false);
  const [action, setAction] = useState<BMCAction | null>(null);
  const [reload, setReload] = useState(0);
  const kinds = ["error", "corrected", "recovery", "repair", "info"];
  const kindLabel = (value: string) => value === "error" ? t("错误", "Error") : value === "corrected" ? t("硬件已纠正", "Hardware corrected") : value === "recovery" ? t("恢复观测", "Recovery observed") : value === "repair" ? t("人工修复记录", "Operator repair record") : t("信息", "Information");
  const sourceStatus = (value: string) => value === "available" ? t("可用", "Available") : value === "partial" ? t("警告", "Warning") : value === "disabled" ? t("已禁用", "Disabled") : t("不可用", "Unavailable");
  const pendingAction = action?.status === "queued" && Date.now() - Date.parse(action.createdAt) < 60000;
  const stale = !snapshot || Date.now() - Date.parse(snapshot.receivedAt) > 120000;

  useEffect(() => {
    const controller = new AbortController();
    getNodes(token, controller.signal).then((items) => { setNodes(items); setNodeID((current) => current || items[0]?.id || ""); }).catch((cause) => { if (!controller.signal.aborted) setError(String(cause)); });
    return () => controller.abort();
  }, [token]);

  useEffect(() => { setSnapshot(null); setEvents([]); setAction(null); setNotice(""); setNote(""); setRelated(""); }, [nodeID]);

  useEffect(() => {
    if (!nodeID) return;
    const controller = new AbortController();
    let loading = false;
    async function load() {
      if (loading) return;
      loading = true;
      try {
        const [health, entries] = await Promise.all([
          getNodeHealth(nodeID, token, controller.signal).catch((cause) => { if (cause instanceof ApiRequestError && cause.status === 404) return null; throw cause; }),
          getSystemEvents(nodeID, kind, token, controller.signal),
        ]);
        if (!controller.signal.aborted) { setSnapshot(health); setEvents(entries); setError(""); }
      } catch (cause) { if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : t("无法读取系统健康状态", "Unable to load system health")); }
      finally { loading = false; }
    }
    void load();
    const timer = window.setInterval(() => void load(), refreshSeconds * 1000);
    return () => { controller.abort(); window.clearInterval(timer); };
  }, [nodeID, kind, token, refreshSeconds, reload]);

  useEffect(() => {
    if (!action || action.status !== "queued") return;
    const controller = new AbortController();
    const timer = window.setInterval(() => {
      getBMCAction(nodeID, action.id, token, controller.signal).then((value) => {
        if (controller.signal.aborted) return;
        setAction(value);
        if (value.status !== "queued" || Date.now() - Date.parse(value.createdAt) >= 60000) window.clearInterval(timer);
      }).catch((cause) => { if (!controller.signal.aborted) { setError(String(cause)); window.clearInterval(timer); } });
    }, 2000);
    return () => { controller.abort(); window.clearInterval(timer); };
  }, [action?.id, action?.status, nodeID, token]);

  async function identify(enabled: boolean) {
    setSaving(true); setError(""); setNotice("");
    try { setAction(await requestBMCAction(nodeID, enabled ? "identify-on" : "identify-off", token)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("定位灯操作失败", "Locator request failed")); }
    finally { setSaving(false); }
  }
  async function saveNote(event: FormEvent) {
    event.preventDefault(); setSaving(true); setError("");
    try { await addSystemNote(nodeID, note, related, token); setNote(""); setRelated(""); setNotice(t("修复记录已保存；原始事件保留。", "Repair record saved; original events are retained.")); setReload((value) => value + 1); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("保存记录失败", "Unable to save record")); }
    finally { setSaving(false); }
  }

  return <section>
    <header className="page-header"><div><span className="eyebrow">{t("管理员", "Administrator")}</span><h1>{t("系统健康", "System health")}</h1><p>{t("查看主机错误、纠错与恢复事件，以及 BMC 传感器和维护记录。", "Inspect host errors, corrections and recovery events, BMC sensors, and maintenance records.")}</p></div></header>
    <div className="health-toolbar"><label className="job-field"><span>{t("节点", "Node")}</span><select aria-label={t("健康监测节点", "Health monitoring node")} value={nodeID} disabled={saving || pendingAction} onChange={(event) => setNodeID(event.target.value)}>{nodes.map((node) => <option value={node.id} key={node.id}>{node.name || node.id}</option>)}</select></label><button className="text-button" type="button" onClick={() => setReload((value) => value + 1)}>{t("刷新视图", "Refresh view")}</button></div>
    {error && <p className="form-error" role="alert">{error}</p>}{notice && <p className="form-notice" role="status">{notice}</p>}
    {!nodeID ? <p className="branding-help">{t("暂无已接入节点。", "No enrolled nodes.")}</p> : <>
      <section className="panel health-panel"><div className="panel-heading"><h2>{t("采集状态", "Collection status")}</h2><span>{snapshot ? (stale ? t("观测已过期", "Observation stale") : t("最近观测", "Recent observation")) : t("等待 Agent 上报", "Waiting for Agent report")}</span></div>
        {snapshot && <p className="branding-help">{t("采样时间", "Sampled")}: {new Date(snapshot.observedAt).toLocaleString()} · {t("控制器接收", "Received")}: {new Date(snapshot.receivedAt).toLocaleString()}</p>}
        <div className="health-source-list">{snapshot?.sources.map((source) => <div key={source.name}><strong>{source.name}</strong><span>{sourceStatus(source.status)}</span>{sourceDetail(source, t) && <p>{sourceDetail(source, t)}</p>}</div>)}</div>
        {!snapshot && <p className="branding-help">{t("暂无健康数据。", "No health data available.")}</p>}
      </section>
      <section className="panel health-panel"><div className="panel-heading"><h2>{t("BMC 传感器", "BMC sensors")}</h2><span>{t("机箱电源", "Chassis power")}: {snapshot?.chassisPower === "on" ? t("开启", "On") : snapshot?.chassisPower === "off" ? t("关闭", "Off") : t("未知", "Unknown")}</span></div>
        <div className="branding-actions"><button className="text-button" type="button" disabled={stale || !snapshot?.identifySupported || saving || pendingAction} onClick={() => void identify(true)}>{t("定位灯亮起 120 秒", "Locate for 120 seconds")}</button><button className="text-button" type="button" disabled={stale || !snapshot?.identifySupported || saving || pendingAction} onClick={() => void identify(false)}>{t("关闭定位灯", "Turn locator off")}</button></div>
        {!snapshot?.identifySupported && <p className="branding-help">{t("定位灯控制未由主机管理员启用。", "Locator control has not been enabled by the host administrator.")}</p>}
        {action && <p role="status">{action.status === "succeeded" ? t("BMC 已确认请求。", "BMC acknowledged the request.") : action.status === "failed" ? t("BMC 操作失败。", "BMC action failed.") : pendingAction ? t("等待 Agent 回执…", "Waiting for Agent acknowledgement…") : t("未收到回执，执行结果未知。", "No acknowledgement received; outcome unknown.")}</p>}
        <div className="table-scroll"><table><thead><tr><th>{t("传感器", "Sensor")}</th><th>{t("读数", "Reading")}</th><th>{t("状态", "Status")}</th><th>{t("临界下限", "Lower critical")}</th><th>{t("临界上限", "Upper critical")}</th></tr></thead><tbody>{snapshot?.sensors.map((sensor) => <tr key={sensor.id}><td>{sensor.name}</td><td>{sensor.available ? `${sensor.value} ${sensor.unit}` : t("不可用", "Unavailable")}</td><td>{sensor.status}</td><td>{sensor.lowerCritical ?? "—"}</td><td>{sensor.upperCritical ?? "—"}</td></tr>)}</tbody></table></div>
        {!snapshot?.sensors.length && <p className="branding-help">{t("暂无可读取的 BMC 传感器。", "No BMC sensor readings available.")}</p>}
      </section>
      <section className="panel health-panel"><div className="panel-heading"><h2>{t("错误与维护记录", "Errors and maintenance records")}</h2><label className="job-field"><span className="sr-only">{t("事件类型", "Event kind")}</span><select value={kind} onChange={(event) => setKind(event.target.value)}><option value="">{t("全部类型", "All kinds")}</option>{kinds.map((item) => <option key={item} value={item}>{kindLabel(item)}</option>)}</select></label></div>
        <div className="health-event-list">{events.map((event) => <article key={event.id}><div className="health-event-meta"><strong>{kindLabel(event.kind)}</strong><span>{event.source} · {event.component}</span><time>{new Date(event.occurredAt || event.receivedAt).toLocaleString()}{!event.occurredAt && ` (${t("接收时间", "received")})`}</time></div><p>{event.message}</p>{event.actor && <small>{t("记录人", "Recorded by")}: {event.actor}</small>}<button className="text-button" type="button" onClick={() => setRelated(event.id)}>{t("关联修复记录", "Link a repair record")}</button></article>)}</div>
        {!events.length && <p className="branding-help">{t("暂无匹配的记录。", "No matching records.")}</p>}
        <form className="health-note" onSubmit={saveNote}><label className="job-field"><span>{t("人工修复记录", "Operator repair record")}</span><textarea value={note} maxLength={1000} required disabled={saving} onChange={(event) => setNote(event.target.value)} placeholder={t("记录检查结论、采取的操作与验证结果", "Record findings, actions taken, and verification results")} /></label>{related && <p className="branding-help">{t("已关联所选事件", "Linked to the selected event")} <button className="text-button" type="button" onClick={() => setRelated("")}>{t("取消关联", "Unlink")}</button></p>}<button className="primary-button" type="submit" disabled={saving || !note.trim()}>{t("保存维护记录", "Save maintenance record")}</button></form>
      </section>
    </>}
  </section>;
}

function sourceDetail(source: NodeHealthSnapshot["sources"][number], t: (zh: string, en: string) => string): string {
  if (source.name === "event-outbox") {
    const counts = source.message?.match(/pending=(\d+); evicted unacknowledged records=(\d+); journal coverage warnings=(\d+)/);
    if (counts) return t(`待发送 ${counts[1]} · 已淘汰 ${counts[2]} · 采集警告 ${counts[3]}`, `Pending ${counts[1]} · Evicted ${counts[2]} · Collection warnings ${counts[3]}`);
  }
  if (source.status === "available" || source.status === "disabled") return "";
  return source.message?.split(/[;\n]/)[0] ?? "";
}
