import { FormEvent, useEffect, useState } from "react";
import { getElasticity, getQueuePolicies, putQueuePolicy } from "../api/client";
import type { ElasticitySnapshot, QueuePolicy } from "../api/contracts";
import { formatBytes, formatCoreCount, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

const initial = {
  name: "default", weight: "1", maxConcurrent: "0", maxProject: "0", cpu: "0", memoryGiB: "0",
  projectCpu: "0", projectMemoryGiB: "0", backfill: true, allowPreemption: false, preemptible: false,
  preemptionGrace: "10", maxPreemptions: "3", maxScaleOut: "0", budgetCPU: "0", budgetMemoryGiB: "0",
  qos: "normal" as "normal" | "high" | "urgent", fairShareHalfLife: "168", historicalWeight: "1",
  reservationStart: "", reservationEnd: "", reservationNodes: "", reservationGroups: "",
};

export function QueuesPage({ token }: { token: string }) {
  const { t } = usePreferences();
  const [policies, setPolicies] = useState<QueuePolicy[]>([]);
  const [elasticity, setElasticity] = useState<ElasticitySnapshot | null>(null);
  const [form, setForm] = useState(initial);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    Promise.all([getQueuePolicies(token, controller.signal), getElasticity(token, controller.signal)])
      .then(([nextPolicies, nextElasticity]) => { setPolicies(nextPolicies); setElasticity(nextElasticity); })
      .catch((cause) => !controller.signal.aborted && setError(cause instanceof Error ? cause.message : "Unable to load queue controls"));
    return () => controller.abort();
  }, [token]);

  async function save(event: FormEvent) {
    event.preventDefault(); setSaving(true); setError("");
    try {
      const updated = await putQueuePolicy(form.name, {
        weight: Number(form.weight), maxConcurrent: Number(form.maxConcurrent), maxConcurrentPerProject: Number(form.maxProject), backfill: form.backfill,
        resourceQuota: { milliCpu: Number(form.cpu) * 1000, memoryBytes: Number(form.memoryGiB) * 1024 ** 3, ephemeralStorageBytes: 0 },
        projectResourceQuota: { milliCpu: Number(form.projectCpu) * 1000, memoryBytes: Number(form.projectMemoryGiB) * 1024 ** 3, ephemeralStorageBytes: 0 },
        allowPreemption: form.allowPreemption, preemptible: form.preemptible,
        preemptionGraceSeconds: Number(form.preemptionGrace), maxPreemptionsPerJob: Number(form.maxPreemptions),
        maxRecommendedScaleOut: Number(form.maxScaleOut),
        elasticityBudget: { milliCpu: Number(form.budgetCPU) * 1000, memoryBytes: Number(form.budgetMemoryGiB) * 1024 ** 3, ephemeralStorageBytes: 0 },
        qos: form.qos,
        fairShareHalfLifeHours: Number(form.fairShareHalfLife), historicalUsageWeight: Number(form.historicalWeight),
        reservationStart: form.reservationStart ? new Date(form.reservationStart).toISOString() : undefined,
        reservationEnd: form.reservationEnd ? new Date(form.reservationEnd).toISOString() : undefined,
        reservationNodeIds: splitList(form.reservationNodes), reservationGroupIds: splitList(form.reservationGroups),
      }, token);
      setPolicies((current) => [...current.filter((item) => item.name !== updated.name), updated].sort((a, b) => a.name.localeCompare(b.name)));
      setElasticity(await getElasticity(token));
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Queue update failed"); }
    finally { setSaving(false); }
  }

  function edit(policy: QueuePolicy) {
    setForm({
      name: policy.name, weight: String(policy.weight), maxConcurrent: String(policy.maxConcurrent ?? 0),
      maxProject: String(policy.maxConcurrentPerProject ?? 0), cpu: formatCoreCount(policy.resourceQuota.milliCpu),
      memoryGiB: String(policy.resourceQuota.memoryBytes / 1024 ** 3), projectCpu: formatCoreCount(policy.projectResourceQuota.milliCpu),
      projectMemoryGiB: String(policy.projectResourceQuota.memoryBytes / 1024 ** 3), backfill: policy.backfill,
      allowPreemption: policy.allowPreemption, preemptible: policy.preemptible,
      preemptionGrace: String(policy.preemptionGraceSeconds ?? 10), maxPreemptions: String(policy.maxPreemptionsPerJob ?? 3),
      maxScaleOut: String(policy.maxRecommendedScaleOut ?? 0), budgetCPU: formatCoreCount(policy.elasticityBudget?.milliCpu ?? 0),
      budgetMemoryGiB: String((policy.elasticityBudget?.memoryBytes ?? 0) / 1024 ** 3),
      qos: policy.qos ?? "normal",
      fairShareHalfLife: String(policy.fairShareHalfLifeHours ?? 168), historicalWeight: String(policy.historicalUsageWeight ?? 1),
      reservationStart: localDateTime(policy.reservationStart), reservationEnd: localDateTime(policy.reservationEnd),
      reservationNodes: (policy.reservationNodeIds ?? []).join(", "), reservationGroups: (policy.reservationGroupIds ?? []).join(", "),
    });
  }

  return <section>
    <header className="page-header"><div><span className="eyebrow">{t("调整任务排队规则", "Tune scheduling rules")}</span><h1>{t("队列策略", "Queue policies")}</h1><p>{t("设置并发上限、资源配额、回填和抢占条件。", "Configure concurrency, quotas, backfill and preemption.")}</p></div></header>
    {error && <p className="error-banner" role="alert">{error}</p>}
    {elasticity && <div className="elasticity-strip">
      <div><span className="eyebrow">{t("检查容量缺口", "Inspect capacity shortfalls")}</span><strong>{t("弹性容量建议", "Elastic capacity recommendations")}</strong><small>{t("建议仅供管理员扩容参考", "Recommendations are advisory for administrator scale-out")} · {relativeTime(elasticity.generatedAt)}</small></div>
      {elasticity.recommendations.length === 0
        ? <p>{t("当前排队任务不需要增加容量。", "Queued jobs do not currently require additional capacity.")}</p>
        : elasticity.recommendations.map((item) => <article key={item.queue}>
          <span>{item.queue}</span><strong>+{item.suggestedAdditionalNodes}</strong><small>{t(`建议节点 · ${item.queuedJobs} 个等待任务${item.limitedByBudget ? " · 已受预算限制" : ""}`, `recommended nodes · ${item.queuedJobs} queued jobs${item.limitedByBudget ? " · budget limited" : ""}`)}</small>
        </article>)}
    </div>}
    <div className="queue-layout">
      <div className="queue-cards">
        {policies.map((policy) => <button type="button" className="queue-card" key={policy.name} onClick={() => edit(policy)}>
          <header><div><span>QUEUE</span><h2>{policy.name}</h2></div><strong>×{policy.weight}</strong></header>
          <dl><div><dt>{t("并发上限", "Concurrency limit")}</dt><dd>{policy.maxConcurrent || t("不限", "Unlimited")}</dd></div><div><dt>{t("项目并发", "Project concurrency")}</dt><dd>{policy.maxConcurrentPerProject || t("不限", "Unlimited")}</dd></div><div><dt>{t("CPU 配额", "CPU quota")}</dt><dd>{policy.resourceQuota.milliCpu ? t(`${formatCoreCount(policy.resourceQuota.milliCpu)} 核心`, `${formatCoreCount(policy.resourceQuota.milliCpu)} cores`) : t("不限", "Unlimited")}</dd></div><div><dt>{t("内存配额", "Memory quota")}</dt><dd>{policy.resourceQuota.memoryBytes ? formatBytes(policy.resourceQuota.memoryBytes) : t("不限", "Unlimited")}</dd></div></dl>
          <footer><span className={policy.allowPreemption ? "enabled" : ""}>{(policy.qos ?? "normal").toUpperCase()} · {policy.allowPreemption ? "PREEMPTION" : policy.backfill ? "BACKFILL" : "STRICT"}</span><small>{relativeTime(policy.updatedAt)}</small></footer>
        </button>)}
        {policies.length === 0 && <div className="queue-empty"><strong>{t("当前使用默认策略", "Default policy in use")}</strong><span>{t("权重 1 · 无配额上限 · 开启回填", "Weight 1 · unlimited quotas · backfill enabled")}</span></div>}
      </div>
      <form className="queue-editor" onSubmit={save}>
        <span className="eyebrow">{t("编辑队列限制", "Edit queue limits")}</span><h2>{t("配置队列", "Configure queue")}</h2><p>{t("数值 0 表示不限制；权重越高，在相同占用下越优先。", "A value of 0 is unlimited. Higher weight has priority at equal usage.")}</p>
        <label><span>{t("队列名称", "Queue name")}</span><input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required /></label>
        <div><QueueNumber label={t("权重", "Weight")} value={form.weight} set={(value) => setForm({ ...form, weight: value })} min="1" /><QueueNumber label={t("队列并发", "Queue concurrency")} value={form.maxConcurrent} set={(value) => setForm({ ...form, maxConcurrent: value })} /></div>
        <div><QueueNumber label={t("项目并发", "Project concurrency")} value={form.maxProject} set={(value) => setForm({ ...form, maxProject: value })} /><QueueNumber label={t("队列逻辑 CPU 核心数", "Queue logical CPU cores")} value={form.cpu} set={(value) => setForm({ ...form, cpu: value })} /></div>
        <div><QueueNumber label={t("队列内存 (G)", "Queue memory (G)")} value={form.memoryGiB} set={(value) => setForm({ ...form, memoryGiB: value })} step="0.1" /><QueueNumber label={t("项目逻辑 CPU 核心数", "Project logical CPU cores")} value={form.projectCpu} set={(value) => setForm({ ...form, projectCpu: value })} /></div>
        <QueueNumber label={t("项目内存 (G)", "Project memory (G)")} value={form.projectMemoryGiB} set={(value) => setForm({ ...form, projectMemoryGiB: value })} step="0.1" />
        <span className="editor-divider">{t("服务等级与历史公平份额", "Service level and historical fair share")}</span>
        <label><span>{t("QoS 等级", "QoS level")}</span><select value={form.qos} onChange={(event) => setForm({ ...form, qos: event.target.value as typeof form.qos })}><option value="normal">Normal</option><option value="high">High</option><option value="urgent">Urgent</option></select></label>
        <QueueNumber label={t("公平份额半衰期 (小时)", "Fair-share half-life (hours)")} value={form.fairShareHalfLife} set={(value) => setForm({ ...form, fairShareHalfLife: value })} />
        <QueueNumber label={t("历史用量权重 (0 关闭)", "Historical usage weight (0 disables)")} value={form.historicalWeight} set={(value) => setForm({ ...form, historicalWeight: value })} />
        <label className="queue-toggle"><input type="checkbox" checked={form.backfill} onChange={(event) => setForm({ ...form, backfill: event.target.checked })} /><span><strong>{t("允许基础回填", "Allow basic backfill")}</strong><small>{t("高优先级作业暂时无法放置时，使用空闲容量运行较小作业。", "Run smaller jobs on idle capacity while a high-priority job cannot be placed.")}</small></span></label>
        <label className="queue-toggle"><input type="checkbox" checked={form.allowPreemption} onChange={(event) => setForm({ ...form, allowPreemption: event.target.checked })} /><span><strong>{t("允许发起抢占", "Allow preemption")}</strong><small>{t("仅抢占显式允许且优先级更低的队列任务；资源在 Agent 确认退出前不会释放。", "Preempt only explicitly preemptible lower-priority jobs; resources remain reserved until the Agent confirms exit.")}</small></span></label>
        <label className="queue-toggle"><input type="checkbox" checked={form.preemptible} onChange={(event) => setForm({ ...form, preemptible: event.target.checked })} /><span><strong>{t("本队列任务可被抢占", "Jobs in this queue are preemptible")}</strong><small>{t("被抢占任务会安全重新入队，并保留原有失败重试额度。", "Preempted jobs are safely requeued without consuming failure retries.")}</small></span></label>
        <div><QueueNumber label={t("退出宽限期 (秒)", "Exit grace period (seconds)")} value={form.preemptionGrace} set={(value) => setForm({ ...form, preemptionGrace: value })} /><QueueNumber label={t("单任务最多抢占次数", "Maximum preemptions per job")} value={form.maxPreemptions} set={(value) => setForm({ ...form, maxPreemptions: value })} /></div>
        <span className="editor-divider">{t("弹性建议预算", "Elasticity recommendation budget")}</span>
        <div><QueueNumber label={t("建议节点上限", "Recommended node limit")} value={form.maxScaleOut} set={(value) => setForm({ ...form, maxScaleOut: value })} /><QueueNumber label={t("预算逻辑 CPU 核心数", "Budget logical CPU cores")} value={form.budgetCPU} set={(value) => setForm({ ...form, budgetCPU: value })} /></div>
        <QueueNumber label={t("预算内存 (G)", "Budget memory (G)")} value={form.budgetMemoryGiB} set={(value) => setForm({ ...form, budgetMemoryGiB: value })} step="0.1" />
        <span className="editor-divider">{t("预约窗口", "Reservation window")}</span>
        <div><label><span>{t("开始时间", "Start time")}</span><input type="datetime-local" value={form.reservationStart} onChange={(event) => setForm({ ...form, reservationStart: event.target.value })} /></label><label><span>{t("结束时间", "End time")}</span><input type="datetime-local" value={form.reservationEnd} onChange={(event) => setForm({ ...form, reservationEnd: event.target.value })} /></label></div>
        <label><span>{t("预约节点 ID（逗号分隔）", "Reserved node IDs (comma separated)")}</span><input value={form.reservationNodes} onChange={(event) => setForm({ ...form, reservationNodes: event.target.value })} placeholder="node_xxx, node_yyy" /></label>
        <label><span>{t("允许的用户组 ID（逗号分隔）", "Allowed group IDs (comma separated)")}</span><input value={form.reservationGroups} onChange={(event) => setForm({ ...form, reservationGroups: event.target.value })} placeholder="group_xxx" /></label>
        <button className="primary-button" disabled={saving} type="submit">{saving ? t("正在保存…", "Saving…") : t("保存队列策略", "Save queue policy")}<span>→</span></button>
      </form>
    </div>
  </section>;
}

function QueueNumber({ label, value, set, min = "0", step = "1" }: { label: string; value: string; set: (value: string) => void; min?: string; step?: string }) {
  return <label><span>{label}</span><input type="number" min={min} step={step} value={value} onChange={(event) => set(event.target.value)} required /></label>;
}

function splitList(value: string): string[] { return value.split(",").map((item) => item.trim()).filter(Boolean); }
function localDateTime(value?: string): string { if (!value) return ""; const date = new Date(value); return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16); }
