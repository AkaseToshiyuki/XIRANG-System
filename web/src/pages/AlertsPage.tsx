import { useEffect, useMemo, useState } from "react";
import { acknowledgeAlert, getAlertDeliveries, getAlerts, retryAlertDelivery, watchAlerts } from "../api/client";
import type { Alert, AlertDelivery, AlertWebhookStatus } from "../api/contracts";
import { clockTime, formatBitRate, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

type AlertView = "active" | "critical" | "history" | "all";

export function AlertsPage({ token }: { token: string }) {
  const { refreshMs, t } = usePreferences();
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [deliveries, setDeliveries] = useState<AlertDelivery[]>([]);
  const [webhook, setWebhook] = useState<AlertWebhookStatus>({ enabled: false });
  const [view, setView] = useState<AlertView>("active");
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState("");
  const [deliveryError, setDeliveryError] = useState("");
  const [lastUpdatedAt, setLastUpdatedAt] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    getAlerts(token, true, controller.signal).then((items) => {
      setAlerts(sortAlerts(items));
      setLoading(false);
      setError("");
      setLastUpdatedAt(new Date().toISOString());
    }).catch((cause) => {
      if (!controller.signal.aborted) {
        setLoading(false);
        setError(cause instanceof Error ? cause.message : "Unable to load alerts");
      }
    });
    return () => controller.abort();
  }, [token]);

  useEffect(() => {
    const controller = new AbortController();
    const refresh = async () => {
      try {
        const result = await getAlertDeliveries(token, 200, controller.signal);
        if (!controller.signal.aborted) {
          setDeliveries(result.items);
          setWebhook(result.webhook);
          setDeliveryError("");
        }
      } catch (cause) {
        if (!controller.signal.aborted) setDeliveryError(cause instanceof Error ? cause.message : "Unable to load webhook deliveries");
      }
    };
    void refresh();
    const interval = window.setInterval(() => void refresh(), refreshMs);
    return () => {
      controller.abort();
      window.clearInterval(interval);
    };
  }, [token, refreshMs]);

  useEffect(() => {
    const controller = new AbortController();
    let retry: number | undefined;
    const connect = async () => {
      try {
        await watchAlerts(token, (update) => {
          setAlerts((current) => sortAlerts(current.some((alert) => alert.id === update.id)
            ? current.map((alert) => alert.id === update.id ? update : alert)
            : [...current, update]));
          setError("");
          setLastUpdatedAt(new Date().toISOString());
        }, () => {
          setStreaming(true);
          setError("");
          setLastUpdatedAt(new Date().toISOString());
        }, controller.signal);
      } catch {
        // Clean EOF and transport failure both reconnect with bounded delay.
      }
      if (!controller.signal.aborted) {
        setStreaming(false);
        setError(t("告警事件流已断开，正在重连…", "Alert event stream disconnected; reconnecting…"));
        retry = window.setTimeout(connect, 1500);
      }
    };
    void connect();
    return () => {
      controller.abort();
      if (retry !== undefined) window.clearTimeout(retry);
      setStreaming(false);
    };
  }, [token, t]);

  const selected = alerts.find((alert) => alert.id === selectedID) ?? null;
  const latestDeliveries = useMemo(() => {
    const result = new Map<string, AlertDelivery>();
    for (const delivery of deliveries) if (!result.has(delivery.alertId)) result.set(delivery.alertId, delivery);
    return result;
  }, [deliveries]);
  const visible = useMemo(() => alerts.filter((alert) => {
    if (view === "active") return alert.status === "firing" || alert.status === "acknowledged";
    if (view === "critical") return alert.severity === "critical" && alert.status !== "resolved";
    if (view === "history") return alert.status === "resolved";
    return true;
  }), [alerts, view]);
  const firing = alerts.filter((alert) => alert.status === "firing").length;
  const critical = alerts.filter((alert) => alert.status !== "resolved" && alert.severity === "critical").length;
  const acknowledged = alerts.filter((alert) => alert.status === "acknowledged").length;
  const resolved = alerts.filter((alert) => alert.status === "resolved").length;

  async function acknowledge(alert: Alert) {
    setError("");
    try {
      const updated = await acknowledgeAlert(alert.id, token);
      setAlerts((current) => sortAlerts(current.map((item) => item.id === updated.id ? updated : item)));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Alert acknowledgement failed");
    }
  }

  async function retry(delivery: AlertDelivery) {
    setError("");
    try {
      const updated = await retryAlertDelivery(delivery.id, token);
      setDeliveries((current) => current.map((item) => item.id === updated.id ? updated : item));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Webhook delivery retry failed");
    }
  }

  return <section>
    <header className="page-header">
      <div>
        <span className="eyebrow">{t("处理集群运行异常", "Handle cluster conditions")}</span>
        <h1>{t("告警中心", "Alerts")}</h1>
        <p>{t("检查分级告警、确认处理状态，并在条件恢复后查看关闭记录。", "Inspect severity, acknowledge active conditions, and review recovery records.")}</p>
      </div>
      <div className={`live-indicator ${streaming ? "" : "live-indicator--warning"}`} title={streaming ? undefined : t("告警流重连中", "Alert stream reconnecting")}><span />{lastUpdatedAt ? `${t("更新于", "Updated at")} ${clockTime(lastUpdatedAt)}` : t("等待更新", "Waiting for update")}</div>
    </header>

    <div className="alert-summary" aria-label={t("告警摘要", "Alert summary")}>
      <div className={firing ? "alert-summary--hot" : ""}><span>{t("等待处理", "Requires action")}</span><strong>{firing}</strong><small>{t("选择告警后确认", "Select an alert to acknowledge")}</small></div>
      <div className={critical ? "alert-summary--critical" : ""}><span>{t("严重告警", "Critical alerts")}</span><strong>{critical}</strong><small>{t("请优先检查", "Inspect first")}</small></div>
      <div><span>{t("已确认", "Acknowledged")}</span><strong>{acknowledged}</strong><small>{t("等待健康恢复", "Awaiting recovery")}</small></div>
      <div><span>{t("已恢复历史", "Recovery history")}</span><strong>{resolved}</strong><small>{t("可切换历史查看", "Available in history")}</small></div>
    </div>

    <div className="alert-tabs" role="tablist" aria-label="Alert views">
      {(["active", "critical", "history", "all"] as AlertView[]).map((item) => <button role="tab" aria-selected={view === item} className={view === item ? "active" : ""} key={item} type="button" onClick={() => setView(item)}>{viewLabel(item, t)}<span>{viewCount(item, alerts)}</span></button>)}
    </div>
    <div className={`webhook-status-line ${webhook.enabled ? "" : "webhook-status-line--disabled"}`}>
      <span><i /> WEBHOOK {webhook.enabled ? "ACTIVE" : "NOT CONFIGURED"}</span>
      <strong>{webhook.enabled ? webhook.destination : t("使用控制节点启动参数启用签名通知", "Enable signed notifications in the controller startup configuration")}</strong>
      {deliveries.some((delivery) => delivery.state === "failed") && <em>{t(`${deliveries.filter((delivery) => delivery.state === "failed").length} 条投递需要处理`, `${deliveries.filter((delivery) => delivery.state === "failed").length} deliveries require action`)}</em>}
    </div>
    {(error || deliveryError) && <p className="error-banner" role="alert">{error || deliveryError}</p>}

    <div className="alerts-workspace">
      <div className="alert-feed" aria-live="polite">
        {visible.map((alert) => <article className={`alert-row alert-row--${alert.severity}${selectedID === alert.id ? " selected" : ""}`} key={alert.id} onClick={() => setSelectedID(alert.id)}>
          <div className="alert-severity-icon"><span>{alert.severity === "critical" ? "!" : alert.severity === "warning" ? "△" : "i"}</span></div>
          <div className="alert-main">
            <div className="alert-title-line"><strong>{alertTitle(alert, t)}</strong><span className={`alert-status alert-status--${alert.status}`}>{statusLabel(alert.status, t)}</span></div>
            <p>{alert.summary}</p>
            <div className="alert-context">{alert.sourceNodeId && <span>NODE <strong>{shortID(alert.sourceNodeId)}</strong></span>}<span>TYPE <strong>{kindLabel(alert.kind, t)}</strong></span>{alert.kind === "interface_health" && <><span>RATE <strong>{formatBitRate(alert.observedBits ?? 0)}</strong></span><span>DROP <strong>{formatRatio(alert.dropRatio)}</strong></span></>}{latestDeliveries.get(alert.id) && <span>WEBHOOK <strong className={`delivery-text--${latestDeliveries.get(alert.id)?.state}`}>{deliveryLabel(latestDeliveries.get(alert.id)!.state, t)}</strong></span>}</div>
          </div>
          <div className="alert-timing"><strong>{relativeTime(alert.lastObservedAt)}</strong><small>{t("持续", "Duration")} {duration(alert.firstObservedAt, alert.resolvedAt || alert.lastObservedAt, t)}</small></div>
          <div className="alert-row-action">{alert.status === "firing" ? <button type="button" onClick={(event) => { event.stopPropagation(); void acknowledge(alert); }}>{t("确认", "Acknowledge")}</button> : <span>{t("查看", "View")} →</span>}</div>
        </article>)}
        {!loading && visible.length === 0 && <div className="alerts-empty"><span>✓</span><strong>{view === "history" ? t("尚无恢复历史", "No recovery history yet") : t("当前没有匹配的告警", "No matching alerts")}</strong><p>{t("当前筛选范围内没有需要处理的集群条件。", "No cluster condition requires action in the current filter.")}</p></div>}
        {loading && <div className="table-loading"><span /><span /><span /></div>}
      </div>
      <AlertInspector alert={selected} delivery={selected ? latestDeliveries.get(selected.id) ?? null : null} webhook={webhook} onRetry={retry} t={t} />
    </div>
  </section>;
}

function AlertInspector({ alert, delivery, webhook, onRetry, t }: { alert: Alert | null; delivery: AlertDelivery | null; webhook: AlertWebhookStatus; onRetry: (delivery: AlertDelivery) => void; t: (zh: string, en: string) => string }) {
  if (!alert) return <aside className="alert-inspector alert-inspector--empty"><div className="inspector-symbol">◉</div><h2>{t("告警详情", "Alert details")}</h2><p>{t("选择一条告警查看指标、生命周期和处理状态。", "Select an alert to inspect metrics, lifecycle and handling status.")}</p><div className="policy-note"><span>{t("抑制策略", "Suppression policy")}</span><strong>{t("持续异常才触发", "Requires a sustained condition")}</strong><small>{t("触发和恢复窗口由控制节点启动参数配置。", "Firing and recovery windows are configured by controller startup parameters.")}</small></div></aside>;
  return <aside className="alert-inspector">
    <span className="eyebrow">{t("检查告警详情", "Inspect alert details")}</span><h2>{alertTitle(alert, t)}</h2>
    <div className={`alert-detail-severity alert-detail-severity--${alert.severity}`}><i />{severityLabel(alert.severity, t)} · {statusLabel(alert.status, t)}</div>
    <p>{alert.summary}</p>
    <dl>
      {alert.sourceNodeId && <div><dt>Node</dt><dd>{alert.sourceNodeId}</dd></div>}
      <div><dt>Condition type</dt><dd>{kindLabel(alert.kind, t)}</dd></div>
      {alert.kind === "interface_health" && <><div><dt>Interface rate</dt><dd>{formatBitRate(alert.observedBits ?? 0)}</dd></div>
      <div><dt>Dropped packets</dt><dd>{formatPacketRate(alert.droppedPacketsPerSecond)}</dd></div>
      <div><dt>Error packets</dt><dd>{formatPacketRate(alert.errorPacketsPerSecond)}</dd></div>
      <div><dt>Drop ratio</dt><dd>{formatRatio(alert.dropRatio)}</dd></div></>}
      <div><dt>First observed</dt><dd>{new Date(alert.firstObservedAt).toLocaleString()}</dd></div>
      {alert.acknowledgedAt && <div><dt>Acknowledged</dt><dd>{new Date(alert.acknowledgedAt).toLocaleString()} · {alert.acknowledgedBy}</dd></div>}
      {alert.resolvedAt && <div><dt>Resolved</dt><dd>{new Date(alert.resolvedAt).toLocaleString()}</dd></div>}
    </dl>
    <div className={`delivery-card ${delivery ? `delivery-card--${delivery.state}` : ""}`}>
      <span>Webhook delivery</span>
      {!webhook.enabled ? <><strong>{t("未配置", "Not configured")}</strong><small>{t("控制节点不会为新转换创建投递事件。", "The controller will not create deliveries for new transitions.")}</small></> : !delivery ? <><strong>{t("等待下一次状态转换", "Waiting for the next transition")}</strong><small>{t("目标", "Target")}：{webhook.destination}</small></> : <>
        <strong>{deliveryLabel(delivery.state, t)}</strong>
        <small>{delivery.eventType} · {t(`尝试 ${delivery.attempts} 次`, `${delivery.attempts} attempts`)}{delivery.responseStatus ? ` · HTTP ${delivery.responseStatus}` : ""}</small>
        {delivery.lastError && <p>{delivery.lastError}</p>}
        {delivery.nextAttemptAt && <small>{t("下次尝试", "Next attempt")}：{new Date(delivery.nextAttemptAt).toLocaleString()}</small>}
        {delivery.state === "failed" && <button type="button" onClick={() => onRetry(delivery)}>{t("立即重试", "Retry now")}</button>}
      </>}
    </div>
    <small className="alert-id">{alert.id} · revision {alert.revision}</small>
  </aside>;
}

function sortAlerts(alerts: Alert[]): Alert[] {
  return [...alerts].sort((left, right) => alertPriority(left) - alertPriority(right) || Date.parse(right.lastObservedAt) - Date.parse(left.lastObservedAt));
}
function alertPriority(alert: Alert): number { return alert.status === "firing" ? alert.severity === "critical" ? 0 : 1 : alert.status === "acknowledged" ? 2 : 3; }
function viewLabel(view: AlertView, t: (zh: string, en: string) => string): string { return t(({ active: "活动告警", critical: "严重", history: "恢复历史", all: "全部" } as Record<AlertView, string>)[view], ({ active: "Active", critical: "Critical", history: "History", all: "All" } as Record<AlertView, string>)[view]); }
function viewCount(view: AlertView, alerts: Alert[]): number { return alerts.filter((alert) => view === "active" ? alert.status !== "resolved" : view === "critical" ? alert.severity === "critical" && alert.status !== "resolved" : view === "history" ? alert.status === "resolved" : true).length; }
function statusLabel(status: Alert["status"], t: (zh: string, en: string) => string): string { return t(({ firing: "待处理", acknowledged: "已确认", resolved: "已恢复" })[status], ({ firing: "Firing", acknowledged: "Acknowledged", resolved: "Resolved" })[status]); }
function severityLabel(severity: Alert["severity"], t: (zh: string, en: string) => string): string { return t(severity === "critical" ? "严重" : severity === "warning" ? "警告" : "信息", severity === "critical" ? "Critical" : severity === "warning" ? "Warning" : "Information"); }
function kindLabel(kind: Alert["kind"], t: (zh: string, en: string) => string): string { return t(({ interface_health: "网络接口", job_storage: "作业存储", group_storage: "用户组配额", node_storage: "节点空间" })[kind], ({ interface_health: "Network interface", job_storage: "Job storage", group_storage: "Group quota", node_storage: "Node storage" })[kind]); }
function alertTitle(alert: Alert, t: (zh: string, en: string) => string): string { return alert.sourceInterface || kindLabel(alert.kind, t); }
function formatRatio(value?: number): string { if (value === undefined) return "—"; return `${(value * 100).toFixed(value < .001 ? 3 : 2)}%`; }
function formatPacketRate(value?: number): string { if (value === undefined) return "—"; return `${value.toFixed(value >= 10 ? 0 : value >= 1 ? 1 : 2)} pkt/s`; }
function duration(start: string, end: string, t: (zh: string, en: string) => string): string { const seconds = Math.max(0, (Date.parse(end) - Date.parse(start)) / 1000); return seconds < 60 ? t(`${seconds.toFixed(0)} 秒`, `${seconds.toFixed(0)} seconds`) : seconds < 3600 ? t(`${Math.floor(seconds / 60)} 分钟`, `${Math.floor(seconds / 60)} minutes`) : t(`${Math.floor(seconds / 3600)} 小时`, `${Math.floor(seconds / 3600)} hours`); }
function shortID(value: string): string { return value.length > 20 ? `${value.slice(0, 17)}…` : value; }
function deliveryLabel(state: AlertDelivery["state"], t: (zh: string, en: string) => string): string { return t(({ pending: "等待投递", retrying: "自动重试中", delivered: "已送达", failed: "投递失败" })[state], ({ pending: "Pending delivery", retrying: "Retrying", delivered: "Delivered", failed: "Delivery failed" })[state]); }
