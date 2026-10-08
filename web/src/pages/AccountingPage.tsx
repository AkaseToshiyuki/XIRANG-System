import { FormEvent, useCallback, useEffect, useMemo, useState } from "react";
import type { CSSProperties } from "react";
import { createBillingRate, getBillingRates, getUsageSummary, getUserUsageSummaries } from "../api/client";
import type { BillingRate, DailyUsageCost, UsageSummary } from "../api/contracts";
import { formatBytes } from "../lib/format";
import { usePreferences } from "../app/preferences";

const DAY = 86_400_000;

export function AccountingPage({ token, isAdmin }: { token: string; isAdmin: boolean }) {
  const { refreshMs, t } = usePreferences();
  const today = useMemo(() => new Date().toISOString().slice(0, 10), []);
  const [from, setFrom] = useState(() => new Date(Date.now() - 29 * DAY).toISOString().slice(0, 10));
  const [to, setTo] = useState(today);
  const [ownerID, setOwnerID] = useState("");
  const [summary, setSummary] = useState<UsageSummary | null>(null);
  const [users, setUsers] = useState<UsageSummary[]>([]);
  const [rates, setRates] = useState<BillingRate[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [showRates, setShowRates] = useState(false);
  const [rateDraft, setRateDraft] = useState({ currency: "CNY", cpu: "0", memory: "0", gpu: "0", storage: "0", effectiveAt: "" });

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const [current, rateItems, userItems] = await Promise.all([
        getUsageSummary(token, from, to, ownerID || undefined, signal),
        getBillingRates(token, signal),
        isAdmin ? getUserUsageSummaries(token, from, to, signal) : Promise.resolve([]),
      ]);
      setSummary(current); setRates(rateItems); setUsers(userItems); setError("");
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : "Unable to load accounting data");
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [from, isAdmin, ownerID, to, token]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    const interval = window.setInterval(() => void load(controller.signal), refreshMs);
    return () => { controller.abort(); window.clearInterval(interval); };
  }, [load, refreshMs]);

  async function saveRate(event: FormEvent) {
    event.preventDefault();
    try {
      await createBillingRate(token, {
        currency: rateDraft.currency.toUpperCase(),
        cpuCoreHourMicros: priceMicros(rateDraft.cpu),
        memoryGiBHourMicros: priceMicros(rateDraft.memory),
        gpuDeviceHourMicros: priceMicros(rateDraft.gpu),
        storageGiBDayMicros: priceMicros(rateDraft.storage),
        effectiveAt: rateDraft.effectiveAt ? new Date(rateDraft.effectiveAt).toISOString() : undefined,
      });
      setShowRates(false);
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to create billing rate");
    }
  }

  const daily = fillDaily(summary?.daily ?? [], from, to);
  const selectedLabel = summary?.ownerName || summary?.ownerId || t("我的账户", "My account");

  return <section>
    <header className="page-header accounting-header">
      <div><h1>{t("用量核算", "Usage accounting")}</h1></div>
      <div className="accounting-actions">
        <label><span>{t("开始", "From")}</span><input type="date" value={from} max={to} onChange={(event) => setFrom(event.target.value)} /></label>
        <label><span>{t("结束", "To")}</span><input type="date" value={to} min={from} max={today} onChange={(event) => setTo(event.target.value)} /></label>
        {isAdmin && <button className="secondary-button" type="button" onClick={() => setShowRates((value) => !value)}>{t("计费标准", "Billing rates")}</button>}
      </div>
    </header>

    {error && <p className="error-banner" role="alert">{error}</p>}
    {isAdmin && <section className="accounting-user-strip">
      <label><span>{t("查看用户", "View user")}</span><select value={ownerID} onChange={(event) => setOwnerID(event.target.value)}><option value="">{t("我的管理员账户", "My administrator account")}</option>{users.map((user) => <option key={user.ownerId} value={user.ownerId}>{user.ownerName || user.ownerId}</option>)}</select></label>
      <div>{users.slice(0, 8).map((user) => <button className={ownerID === user.ownerId ? "active" : ""} type="button" key={user.ownerId} onClick={() => setOwnerID(user.ownerId ?? "")}><strong>{user.ownerName || user.ownerId}</strong><span>{formatMoney(user.cost.totalCostMicros, user.currency)}</span></button>)}</div>
    </section>}

    {showRates && isAdmin && <section className="billing-rate-panel">
      <div><h2>{t("发布新计费标准", "Publish billing rates")}</h2><p>{t("新标准按生效时间创建，已有标准保持不变。", "New rates take effect at the selected time; existing rate versions remain unchanged.")}</p></div>
      <form onSubmit={saveRate}>
        <label><span>{t("币种", "Currency")}</span><input required minLength={3} maxLength={3} value={rateDraft.currency} onChange={(event) => setRateDraft({ ...rateDraft, currency: event.target.value })} /></label>
        <RateInput label={t("CPU 核心时", "CPU core-hour")} value={rateDraft.cpu} onChange={(cpu) => setRateDraft({ ...rateDraft, cpu })} />
        <RateInput label={t("内存 GiB 时", "Memory GiB-hour")} value={rateDraft.memory} onChange={(memory) => setRateDraft({ ...rateDraft, memory })} />
        <RateInput label={t("GPU 卡时", "GPU device-hour")} value={rateDraft.gpu} onChange={(gpu) => setRateDraft({ ...rateDraft, gpu })} />
        <RateInput label={t("存储 GiB 日", "Storage GiB-day")} value={rateDraft.storage} onChange={(storage) => setRateDraft({ ...rateDraft, storage })} />
        <label><span>{t("生效时间（留空立即生效）", "Effective time (blank for now)")}</span><input type="datetime-local" value={rateDraft.effectiveAt} onChange={(event) => setRateDraft({ ...rateDraft, effectiveAt: event.target.value })} /></label>
        <button className="primary-button" type="submit">{t("发布标准", "Publish rates")}<span aria-hidden="true">→</span></button>
      </form>
      <div className="billing-rate-history">{rates.slice().reverse().map((rate) => <span key={rate.id}><strong>{rate.currency}</strong><small>{rate.effectiveAt ? new Date(rate.effectiveAt).toLocaleString() : "—"}</small><b>{formatMoney(rate.cpuCoreHourMicros, rate.currency)} / CPUh · {formatMoney(rate.gpuDeviceHourMicros, rate.currency)} / GPUh</b></span>)}</div>
    </section>}

    <div className="accounting-summary">
      <div><span>{t("核算账户", "Account")}</span><strong>{selectedLabel}</strong><small>{from} — {to}</small></div>
      <div><span>{t("CPU 核心时", "CPU core-hours")}</span><strong>{formatNumber((summary?.cpuUsageMicros ?? 0) / 3_600_000_000)}</strong><small>{t("cgroup 实际 CPU 时间", "Measured cgroup CPU time")}</small></div>
      <div><span>{t("内存 GiB 时", "Memory GiB-hours")}</span><strong>{formatNumber((summary?.memoryByteSeconds ?? 0) / (2 ** 30 * 3600))}</strong><small>{t("采样积分", "Sampled integral")}</small></div>
      <div><span>{t("GPU 卡时", "GPU device-hours")}</span><strong>{formatNumber((summary?.gpuDeviceSeconds ?? 0) / 3600)}</strong><small>{t("已分配设备运行时间", "Allocated device runtime")}</small></div>
      <div><span>{t("峰值工作区", "Peak workspace")}</span><strong>{formatBytes(summary?.peakWorkspaceBytes ?? 0)}</strong><small>{t("实际文件占用", "Measured file usage")}</small></div>
      <div className="accounting-total"><span>{t("对应费用", "Cost")}</span><strong>{formatMoney(summary?.cost.totalCostMicros ?? 0, summary?.currency ?? "CNY")}</strong><small>{t("按生效计费版本计算", "Calculated with effective rate versions")}</small></div>
    </div>

    <div className="accounting-grid">
      <article className="usage-chart-card"><header><div><h2>{t("每日费用", "Daily cost")}</h2></div><strong>{daily.length} {t("天", "days")}</strong></header><UsageLine daily={daily} currency={summary?.currency ?? "CNY"} /></article>
      <article className="usage-heat-card"><header><div><h2>{t("每日资源活动", "Daily resource activity")}</h2></div></header><UsageHeatmap daily={daily} currency={summary?.currency ?? "CNY"} /></article>
    </div>

    {isAdmin && <section className="accounting-user-table"><header><div><h2>{t("用户用量与费用", "User usage and cost")}</h2></div></header><div className="accounting-user-row accounting-user-row--head"><span>{t("用户", "User")}</span><span>CPU h</span><span>GPU h</span><span>{t("峰值空间", "Peak storage")}</span><span>{t("费用", "Cost")}</span></div>{users.map((user) => <button type="button" className="accounting-user-row" key={user.ownerId} onClick={() => setOwnerID(user.ownerId ?? "")}><span><strong>{user.ownerName || user.ownerId}</strong><small>{user.ownerId}</small></span><span>{formatNumber(user.cpuUsageMicros / 3_600_000_000)}</span><span>{formatNumber(user.gpuDeviceSeconds / 3600)}</span><span>{formatBytes(user.peakWorkspaceBytes)}</span><span><strong>{formatMoney(user.cost.totalCostMicros, user.currency)}</strong></span></button>)}</section>}
    {loading && !summary && <div className="table-loading"><span /><span /><span /></div>}
  </section>;
}

function RateInput({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return <label><span>{label}</span><input type="number" min={0} step="0.000001" required value={value} onChange={(event) => onChange(event.target.value)} /></label>;
}

function UsageLine({ daily, currency }: { daily: DailyUsageCost[]; currency: string }) {
  const values = daily.map((item) => item.cost.totalCostMicros || item.wallTimeMillis);
  const maximum = Math.max(1, ...values);
  const points = values.map((value, index) => `${24 + index * (672 / Math.max(1, values.length - 1))},${184 - value / maximum * 148}`).join(" ");
  return <div className="usage-line"><svg viewBox="0 0 720 220" role="img" aria-label="Daily usage cost line chart"><path d="M24 36H696M24 110H696M24 184H696" /><polyline points={points} /><circle cx={values.length ? 24 + (values.length - 1) * (672 / Math.max(1, values.length - 1)) : 24} cy={values.length ? 184 - values[values.length - 1] / maximum * 148 : 184} r="4" /></svg><footer><span>{daily[0]?.date ?? "—"}</span><strong>{formatMoney(daily.at(-1)?.cost.totalCostMicros ?? 0, currency)}</strong><span>{daily.at(-1)?.date ?? "—"}</span></footer></div>;
}

function UsageHeatmap({ daily, currency }: { daily: DailyUsageCost[]; currency: string }) {
  const intensity = daily.map((item) => item.cost.totalCostMicros || item.wallTimeMillis);
  const maximum = Math.max(1, ...intensity);
  return <div className="usage-heatmap">{daily.map((item, index) => <span key={item.date} style={{ "--heat": String(.08 + .92 * intensity[index] / maximum) } as CSSProperties} title={`${item.date} · ${formatMoney(item.cost.totalCostMicros, currency)} · CPU ${formatNumber(item.cpuUsageMicros / 3_600_000_000)} h`}><i /></span>)}</div>;
}

function fillDaily(source: DailyUsageCost[], from: string, to: string): DailyUsageCost[] {
  const byDate = new Map(source.map((item) => [item.date, item]));
  const result: DailyUsageCost[] = [];
  for (let cursor = Date.parse(`${from}T00:00:00Z`), end = Date.parse(`${to}T00:00:00Z`); cursor <= end && result.length <= 367; cursor += DAY) {
    const date = new Date(cursor).toISOString().slice(0, 10);
    result.push(byDate.get(date) ?? { date, ownerId: "", cpuUsageMicros: 0, memoryByteSeconds: 0, gpuDeviceSeconds: 0, wallTimeMillis: 0, workspaceByteSeconds: 0, peakWorkspaceBytes: 0, cost: { cpuCostMicros: 0, memoryCostMicros: 0, gpuCostMicros: 0, storageCostMicros: 0, totalCostMicros: 0 } });
  }
  return result;
}

function priceMicros(value: string): number { const parsed = Number(value); if (!Number.isFinite(parsed) || parsed < 0) throw new Error("Invalid billing price"); return Math.round(parsed * 1_000_000); }
function formatMoney(micros: number, currency: string): string { return `${currency || "CNY"} ${(micros / 1_000_000).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 6 })}`; }
function formatNumber(value: number): string { return value.toLocaleString(undefined, { maximumFractionDigits: value < 10 ? 3 : 1 }); }
