import { FormEvent, useEffect, useState } from "react";
import { getAlertDeliveries, getAlertPolicy, updateAlertPolicy } from "../api/client";
import type { AlertPolicy, AlertWebhookStatus } from "../api/contracts";
import { usePreferences } from "../app/preferences";

export function AlertPolicyForm({ token }: { token: string }) {
  const { t } = usePreferences();
  const [draft, setDraft] = useState<AlertPolicy | null>(null);
  const [webhook, setWebhook] = useState<AlertWebhookStatus>({ enabled: false });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  async function reload() {
    setBusy(true); setError(""); setNotice("");
    try {
      const [policy, deliveries] = await Promise.all([getAlertPolicy(token), getAlertDeliveries(token, 1)]);
      setDraft(policy); setWebhook(deliveries.webhook);
    } catch (cause) { setError(cause instanceof Error ? cause.message : t("无法读取告警策略", "Unable to load alert policy")); }
    finally { setBusy(false); }
  }
  useEffect(() => { void reload(); }, [token]);

  async function save(event: FormEvent) {
    event.preventDefault();
    if (!draft) return;
    setBusy(true); setError(""); setNotice("");
    try {
      setDraft(await updateAlertPolicy(draft, token));
      setNotice(t("告警策略已保存。", "Alert policy saved."));
    } catch (cause) { setError(cause instanceof Error ? cause.message : t("告警策略保存失败", "Unable to save alert policy")); }
    finally { setBusy(false); }
  }

  const kinds: Array<[AlertPolicy["kinds"][number], string]> = [
    ["interface_health", t("网络接口", "Network interface")], ["job_storage", t("作业存储", "Job storage")],
    ["group_storage", t("用户组配额", "Group quota")], ["node_storage", t("节点空间", "Node storage")],
  ];
  return <form className="panel alert-policy-form" aria-label={t("告警策略", "Alert policy")} onSubmit={save}>
    <h2>{t("告警与通知策略", "Alert and notification policy")}</h2>
    {draft && <fieldset disabled={busy} className="policy-fields">
      <legend className="sr-only">{t("策略参数", "Policy parameters")}</legend>
      <div className="policy-grid">
        <label className="job-field"><span>{t("触发持续时间（秒）", "Trigger duration (seconds)")}</span><input type="number" min="0.001" max="86400" step="any" required value={draft.triggerAfterSeconds} onChange={(e) => setDraft({ ...draft, triggerAfterSeconds: Number(e.target.value) })} /></label>
        <label className="job-field"><span>{t("恢复持续时间（秒）", "Recovery duration (seconds)")}</span><input type="number" min="0.001" max="86400" step="any" required value={draft.resolveAfterSeconds} onChange={(e) => setDraft({ ...draft, resolveAfterSeconds: Number(e.target.value) })} /></label>
        <label className="job-field"><span>{t("通知最低级别", "Minimum notification severity")}</span><select value={draft.minimumSeverity} onChange={(e) => setDraft({ ...draft, minimumSeverity: e.target.value as AlertPolicy["minimumSeverity"] })}><option value="info">{t("信息及以上", "Info and above")}</option><option value="warning">{t("警告及以上", "Warning and above")}</option><option value="critical">{t("仅严重", "Critical only")}</option></select></label>
        <label className="job-field"><span>{t("送达目标（秒）", "Delivery objective (seconds)")}</span><input type="number" min="1" max="86400" required value={draft.deliveryTargetSeconds} onChange={(e) => setDraft({ ...draft, deliveryTargetSeconds: Number(e.target.value) })} /></label>
      </div>
      <label className="policy-choice"><input type="checkbox" checked={draft.routeEnabled} onChange={(e) => setDraft({ ...draft, routeEnabled: e.target.checked })} />{t("向已配置的 Webhook 路由通知", "Route notifications to the configured webhook")}</label>
      <p className="branding-help">{webhook.enabled ? t(`当前目的地：${webhook.destination}`, `Current destination: ${webhook.destination}`) : t("尚未配置通知目的地。", "No notification destination configured.")}</p>
      <fieldset className="policy-kinds"><legend>{t("允许通知的类型", "Routed condition types")}</legend><div>{kinds.map(([kind, label]) => <label className="policy-choice" key={kind}><input type="checkbox" checked={draft.kinds.includes(kind)} onChange={(e) => setDraft({ ...draft, kinds: e.target.checked ? [...draft.kinds, kind] : draft.kinds.filter((value) => value !== kind) })} />{label}</label>)}</div></fieldset>
      <label className="job-field"><span>{t("静默至（本地时间，最长 30 天）", "Silence until (local time, up to 30 days)")}</span><input type="datetime-local" value={localDateTime(draft.silenceUntil)} onChange={(e) => setDraft({ ...draft, silenceUntil: e.target.value ? new Date(e.target.value).toISOString() : undefined })} /></label>
    </fieldset>}
    {error && <p className="form-error" role="alert">{error}</p>}
    {notice && <p className="form-notice" role="status">{notice}</p>}
    <div className="branding-actions"><button className="primary-button" type="submit" disabled={busy || !draft}>{t("保存告警策略", "Save alert policy")}</button><button className="text-button" type="button" disabled={busy} onClick={() => void reload()}>{t("重新载入告警策略", "Reload alert policy")}</button></div>
  </form>;
}

function localDateTime(value?: string): string {
  if (!value) return "";
  const date = new Date(value);
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}
