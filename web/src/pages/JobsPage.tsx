import { FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { cancelJob, createJobTemplate, deleteJobTemplate, getJobs, getJobTemplates, submitJobScript, updateJobTemplate, watchJobLogs } from "../api/client";
import type { Job, JobStatus, JobTemplate } from "../api/contracts";
import { formatBytes, formatCoreCount, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";
import { emptyJobScriptDraft, parseJobScript, serializeJobScript, type JobScriptDraft } from "../lib/jobScript";

const starterScriptZH = `XIRANG 1
# 每行使用 key = value；arg 与 env.NAME 可以重复
project = default
queue = default
priority = 0
attempts = 1
# array = 0-15
# depends = job_xxx:afterok
# nodes = 2
# tasks_per_node = 1
# launcher = native
artifact = https://artifacts.example/worker
sha256 = <64 位 SHA-256>
architecture = amd64
entrypoint = worker
cpu = 1
memory = 1G
timeout = 1h
# device.nvidia.com/gpu = 1
# workdir = work
# env.OMP_NUM_THREADS = 1
# arg = --input
# arg = /shared/project/input.dat`;

const starterScriptEN = `XIRANG 1
# One key = value per line; arg and env.NAME may be repeated
project = default
queue = default
priority = 0
attempts = 1
# array = 0-15
# depends = job_xxx:afterok
# nodes = 2
# tasks_per_node = 1
# launcher = native
artifact = https://artifacts.example/worker
sha256 = <64-character SHA-256>
architecture = amd64
entrypoint = worker
cpu = 1
memory = 1G
timeout = 1h
# device.nvidia.com/gpu = 1
# workdir = work
# env.OMP_NUM_THREADS = 1
# arg = --input
# arg = /shared/project/input.dat`;

const emptyTemplate = { id: "", name: "", description: "", script: starterScriptEN, revision: 0 };

export function JobsPage({ token, isAdmin }: { token: string; isAdmin: boolean }) {
  const { language, refreshMs, t } = usePreferences();
  const [jobs, setJobs] = useState<Job[]>([]);
  const [templates, setTemplates] = useState<JobTemplate[]>([]);
  const [script, setScript] = useState(() => language === "zh" ? starterScriptZH : starterScriptEN);
  const [scriptDraft, setScriptDraft] = useState<JobScriptDraft>(emptyJobScriptDraft);
  const [rawScriptMode, setRawScriptMode] = useState(true);
  const [showSubmit, setShowSubmit] = useState(false);
  const [showTemplateManager, setShowTemplateManager] = useState(false);
  const [selectedTemplateID, setSelectedTemplateID] = useState("");
  const [templateDraft, setTemplateDraft] = useState(emptyTemplate);
  const [templatesLoading, setTemplatesLoading] = useState(true);
  const [savingTemplate, setSavingTemplate] = useState(false);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [cancelingID, setCancelingID] = useState("");
  const [error, setError] = useState("");
  const [filter, setFilter] = useState<"all" | "active" | "finished">("all");
  const [logJobID, setLogJobID] = useState("");
  const [logStreaming, setLogStreaming] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      setJobs(await getJobs(token, 250, signal));
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : t("无法读取作业", "Unable to load jobs"));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [token, t]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    const interval = window.setInterval(() => void load(controller.signal), refreshMs);
    return () => { controller.abort(); window.clearInterval(interval); };
  }, [load, refreshMs]);

  const loadTemplates = useCallback(async (signal?: AbortSignal) => {
    try {
      const items = await getJobTemplates(token, signal);
      setTemplates(items);
      setSelectedTemplateID((current) => items.some((item) => item.id === current) ? current : (items[0]?.id ?? ""));
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : t("无法读取任务模板", "Unable to load job templates"));
    } finally {
      if (!signal?.aborted) setTemplatesLoading(false);
    }
  }, [token, t]);

  useEffect(() => {
    const controller = new AbortController();
    void loadTemplates(controller.signal);
    return () => controller.abort();
  }, [loadTemplates]);

  const visible = useMemo(() => jobs.filter((job) => filter === "all" || (filter === "active" ? !isTerminal(job.status) : isTerminal(job.status))), [jobs, filter]);
  const counts = useMemo(() => ({
    queued: jobs.filter((job) => job.status === "queued").length,
    running: jobs.filter((job) => ["running", "suspended", "assigned", "canceling", "orphaned"].includes(job.status)).length,
    succeeded: jobs.filter((job) => job.status === "succeeded").length,
    failed: jobs.filter((job) => job.status === "failed").length,
  }), [jobs]);
  const logJob = jobs.find((job) => job.id === logJobID);
  const selectedTemplate = templates.find((template) => template.id === selectedTemplateID);

  useEffect(() => {
    if (!logJobID) return;
    const controller = new AbortController();
    let retry: number | undefined;
    const connect = async () => {
      try {
        await watchJobLogs(logJobID, token, (updated) => {
          setJobs((current) => current.map((item) => item.id === updated.id ? updated : item));
        }, () => setLogStreaming(true), controller.signal);
      } catch { /* bounded reconnect */ }
      if (!controller.signal.aborted) {
        setLogStreaming(false);
        retry = window.setTimeout(connect, 1000);
      }
    };
    void connect();
    return () => { controller.abort(); if (retry !== undefined) window.clearTimeout(retry); setLogStreaming(false); };
  }, [logJobID, token]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const created = await submitJobScript(script, token);
      setJobs((current) => [...created, ...current]);
      setShowSubmit(false);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("作业脚本提交失败", "Job script submission failed"));
    } finally {
      setSubmitting(false);
    }
  }

  async function loadFile(file?: File) {
    if (!file) return;
    if (file.size > 1 << 20) {
      setError(t("任务脚本不能超过 1 MiB", "Job scripts cannot exceed 1 MiB"));
      return;
    }
    setScript(await file.text());
    setRawScriptMode(true);
    setShowSubmit(true);
  }

  async function cancel(job: Job) {
    if (!window.confirm(t(`取消作业 ${job.id}？运行中的进程会先收到 SIGTERM。`, `Cancel job ${job.id}? Running processes receive SIGTERM first.`))) return;
    setCancelingID(job.id);
    setError("");
    try {
      const updated = await cancelJob(job.id, token);
      setJobs((current) => current.map((item) => item.id === updated.id ? updated : item));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("取消作业失败", "Unable to cancel job"));
    } finally {
      setCancelingID("");
    }
  }

  function useTemplate() {
    if (!selectedTemplate) return;
    try {
      const draft = parseJobScript(selectedTemplate.script);
      setScriptDraft(draft); setScript(serializeJobScript(draft)); setRawScriptMode(false);
      setError("");
    } catch (cause) {
      setScript(selectedTemplate.script); setRawScriptMode(true);
      setError(cause instanceof Error ? cause.message : t("模板无法结构化解析", "Template cannot be opened in structured mode"));
    }
    setShowSubmit(true);
  }

  function updateScriptField(key: keyof JobScriptDraft, value: string) {
    setScriptDraft((current) => { const next = { ...current, [key]: value }; setScript(serializeJobScript(next)); return next; });
  }

  function toggleRawScript(checked: boolean) {
    if (!checked) {
      try { const draft = parseJobScript(script); setScriptDraft(draft); setScript(serializeJobScript(draft)); }
      catch (cause) { setError(cause instanceof Error ? cause.message : t("脚本无法结构化解析", "Script cannot be parsed into fields")); return; }
    }
    setRawScriptMode(checked); setError("");
  }

  function editTemplate(template?: JobTemplate) {
    setTemplateDraft(template ? {
      id: template.id, name: template.name, description: template.description ?? "",
      script: template.script, revision: template.revision,
    } : { ...emptyTemplate, script });
    setShowTemplateManager(true);
    setError("");
  }

  async function saveTemplate(event: FormEvent) {
    event.preventDefault();
    setSavingTemplate(true);
    setError("");
    try {
      const saved = templateDraft.id
        ? await updateJobTemplate(templateDraft, token)
        : await createJobTemplate(templateDraft, token);
      setTemplates((current) => [...current.filter((item) => item.id !== saved.id), saved].sort((left, right) => left.name.localeCompare(right.name)));
      setSelectedTemplateID(saved.id);
      setTemplateDraft({
        id: saved.id, name: saved.name, description: saved.description ?? "",
        script: saved.script, revision: saved.revision,
      });
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : t("无法保存任务模板", "Unable to save job template");
      if (message.includes("revision conflict")) {
        await loadTemplates();
        setError(t("模板已被其他管理员更新，列表已刷新，请重新选择后编辑。", "Another administrator updated this template. The list was refreshed; select it again before editing."));
      } else {
        setError(message);
      }
    } finally {
      setSavingTemplate(false);
    }
  }

  async function removeTemplate(template: Pick<JobTemplate, "id" | "name" | "revision">) {
    if (!window.confirm(t(`删除任务模板“${template.name}”？用户将不能再选择它。`, `Delete job template “${template.name}”? Users will no longer be able to select it.`))) return;
    setError("");
    try {
      await deleteJobTemplate(template.id, template.revision, token);
      setTemplates((current) => current.filter((item) => item.id !== template.id));
      setSelectedTemplateID((current) => current === template.id ? "" : current);
      if (templateDraft.id === template.id) setTemplateDraft(emptyTemplate);
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : t("无法删除任务模板", "Unable to delete job template");
      if (message.includes("revision conflict")) {
        await loadTemplates();
        setError(t("模板已被其他管理员更新，列表已刷新，请重新选择后删除。", "Another administrator updated this template. The list was refreshed; select it again before deleting."));
      } else {
        setError(message);
      }
    }
  }

  return <section>
    <header className="page-header jobs-header">
      <div><span className="eyebrow">{t("提交与管理作业", "Submit and manage jobs")}</span><h1>{t("作业调度", "Job scheduling")}</h1><p>{t("上传或编辑 XIRANG 任务脚本，提交后可查看状态、日志并取消自己的作业。", "Open or edit a XIRANG job script, then inspect status and logs or cancel your own jobs.")}</p></div>
      <div className="jobs-header-actions">
        <input ref={fileInput} type="file" accept=".xirang,text/plain" hidden onChange={(event) => void loadFile(event.target.files?.[0])} />
        {isAdmin && <button className="secondary-button" type="button" onClick={() => showTemplateManager ? setShowTemplateManager(false) : editTemplate()}>{t("模板管理", "Manage templates")}</button>}
        <button className="secondary-button" type="button" onClick={() => fileInput.current?.click()}>{t("打开脚本", "Open script")}</button>
        <button className="primary-button jobs-new" type="button" onClick={() => setShowSubmit((value) => !value)}>{showSubmit ? t("关闭编辑器", "Close editor") : t("提交任务脚本", "Submit job script")}<span>{showSubmit ? "×" : "+"}</span></button>
      </div>
    </header>

    <section className="job-template-picker" aria-labelledby="job-template-title">
      <div><span className="eyebrow">{t("可用模板", "Available templates")}</span><h2 id="job-template-title">{t("从管理员模板开始", "Start from an administrator template")}</h2><p>{selectedTemplate?.description || (templatesLoading ? t("正在读取模板…", "Loading templates…") : templates.length ? t("选择模板后载入编辑器，提交前仍可修改所有字段。", "Load a template into the editor; every field remains editable before submission.") : t("管理员尚未发布任务模板，仍可打开或直接编辑脚本。", "No template has been published; you can still open or edit a script directly."))}</p></div>
      <label><span>{t("任务模板", "Job template")}</span><select value={selectedTemplateID} disabled={!templates.length} onChange={(event) => setSelectedTemplateID(event.target.value)}><option value="">{templates.length ? t("请选择", "Select") : t("暂无模板", "No templates")}</option>{templates.map((template) => <option value={template.id} key={template.id}>{template.name}</option>)}</select></label>
      <button className="primary-button" type="button" disabled={!selectedTemplate} onClick={useTemplate}>{t("载入并编辑", "Load and edit")}<span>→</span></button>
    </section>

    {isAdmin && showTemplateManager && <section className="job-template-manager">
      <aside>
        <header><div><span className="eyebrow">{t("管理员操作", "Administrator actions")}</span><h2>{t("模板列表", "Template list")}</h2></div><button type="button" onClick={() => editTemplate()}>{t("新建", "New")}</button></header>
        <div>{templates.map((template) => <button className={template.id === templateDraft.id ? "active" : ""} type="button" key={template.id} onClick={() => editTemplate(template)}><strong>{template.name}</strong><span>{t(`修订 ${template.revision}`, `Revision ${template.revision}`)} · {template.updatedBy}</span></button>)}{!templates.length && <p>{t("保存第一个模板后，所有已登录用户都可以在此页面选择它。", "After the first template is saved, every signed-in user can select it here.")}</p>}</div>
      </aside>
      <form onSubmit={saveTemplate}>
        <header><div><span className="eyebrow">{templateDraft.id ? t(`修订 ${templateDraft.revision}`, `Revision ${templateDraft.revision}`) : t("新模板", "New template")}</span><h2>{templateDraft.id ? t("编辑任务模板", "Edit job template") : t("发布任务模板", "Publish job template")}</h2></div>{templateDraft.id && <button className="danger-button" type="button" onClick={() => void removeTemplate(templates.find((item) => item.id === templateDraft.id) ?? templateDraft)}>{t("删除模板", "Delete template")}</button>}</header>
        <div className="job-template-fields">
          <label><span>{t("模板名称", "Template name")}</span><input required maxLength={128} value={templateDraft.name} onChange={(event) => setTemplateDraft({ ...templateDraft, name: event.target.value })} /></label>
          <label><span>{t("使用说明", "Usage notes")}</span><input maxLength={1024} value={templateDraft.description} onChange={(event) => setTemplateDraft({ ...templateDraft, description: event.target.value })} placeholder={t("告诉用户需要修改哪些路径或参数", "Tell users which paths or parameters to change")} /></label>
        </div>
        <label className="job-template-script"><span>{t("XIRANG 1 脚本", "XIRANG 1 script")}</span><textarea required maxLength={65536} spellCheck={false} value={templateDraft.script} onChange={(event) => setTemplateDraft({ ...templateDraft, script: event.target.value })} /></label>
        <footer><span>{t("服务端会完整解析脚本；模板对所有用户可见，请勿写入密码或令牌。", "The server parses the full script. Templates are visible to all users; never include passwords or tokens.")}</span><button className="primary-button" disabled={savingTemplate} type="submit">{savingTemplate ? t("正在校验…", "Validating…") : templateDraft.id ? t("保存修订", "Save revision") : t("校验并发布", "Validate and publish")}<span>→</span></button></footer>
      </form>
    </section>}

    <div className="job-summary">
      <div><span>{t("队列等待", "Queued")}</span><strong>{counts.queued}</strong><small>{t("等待可用节点", "Waiting for available nodes")}</small></div>
      <div className="job-summary--active"><span>{t("已分配 / 运行", "Assigned / running")}</span><strong>{counts.running}</strong><small>{t("可查看实时日志", "Live logs available")}</small></div>
      <div className="job-summary--success"><span>{t("执行成功", "Succeeded")}</span><strong>{counts.succeeded}</strong><small>{t("任务已正常结束", "Jobs completed normally")}</small></div>
      <div className={counts.failed ? "job-summary--failed" : ""}><span>{t("执行失败", "Failed")}</span><strong>{counts.failed}</strong><small>{t("打开日志检查原因", "Open logs to inspect the cause")}</small></div>
    </div>

    {showSubmit && <form className="job-submit-panel job-script-panel" onSubmit={submit}>
      <header><div><span className="eyebrow">XIRANG 1</span><h2>{t("任务脚本", "Job script")}</h2></div><p>{t("声明编译产物、架构、资源、环境变量与参数；服务端会逐行校验后再加入队列。", "Declare the compiled output, architecture, resources, environment and arguments. The server validates every line before queueing.")}</p></header>
      {rawScriptMode ? <textarea className="job-script-editor" spellCheck={false} value={script} onChange={(event) => setScript(event.target.value)} aria-label={t("XIRANG 任务脚本", "XIRANG job script")} /> : <div className="job-structured-editor">
        <fieldset><legend>{t("调度参数", "Scheduling")}</legend><JobField label={t("项目", "Project")} value={scriptDraft.project} set={(value) => updateScriptField("project", value)} /><JobField label={t("队列", "Queue")} value={scriptDraft.queue} set={(value) => updateScriptField("queue", value)} /><JobField label={t("优先级", "Priority")} value={scriptDraft.priority} set={(value) => updateScriptField("priority", value)} type="number" /><JobField label={t("最大尝试次数", "Maximum attempts")} value={scriptDraft.attempts} set={(value) => updateScriptField("attempts", value)} type="number" /><JobField label={t("任务数组（可选）", "Job array (optional)")} value={scriptDraft.array} set={(value) => updateScriptField("array", value)} /><JobArea label={t("依赖任务（每行一个）", "Dependencies (one per line)")} value={scriptDraft.dependencies} set={(value) => updateScriptField("dependencies", value)} /></fieldset>
        <fieldset><legend>{t("编译产物", "Compiled output")}</legend><JobField label={t("应用名（自动选择节点构建变体）", "Application (selects the node build variant)")} value={scriptDraft.application} set={(value) => updateScriptField("application", value)} />{!scriptDraft.application && <><JobField label="URI" value={scriptDraft.artifact} set={(value) => updateScriptField("artifact", value)} /><JobField label="SHA-256" value={scriptDraft.sha256} set={(value) => updateScriptField("sha256", value)} /><label><span>{t("架构", "Architecture")}</span><select value={scriptDraft.architecture} onChange={(event) => updateScriptField("architecture", event.target.value)}><option value="amd64">amd64</option><option value="arm64">arm64</option><option value="riscv64">riscv64</option><option value="ppc64le">ppc64le</option></select></label><JobField label="ABI" value={scriptDraft.abi} set={(value) => updateScriptField("abi", value)} /><JobField label={t("入口程序", "Entrypoint")} value={scriptDraft.entrypoint} set={(value) => updateScriptField("entrypoint", value)} /></>}</fieldset>
        <fieldset><legend>{t("资源与并行", "Resources and parallelism")}</legend><JobField label={t("逻辑 CPU 核心数", "Logical CPU cores")} value={scriptDraft.cpu} set={(value) => updateScriptField("cpu", value)} type="number" /><JobField label={t("内存（如 4G）", "Memory (for example 4G)")} value={scriptDraft.memory} set={(value) => updateScriptField("memory", value)} /><JobField label={t("超时", "Timeout")} value={scriptDraft.timeout} set={(value) => updateScriptField("timeout", value)} /><JobField label={t("节点数", "Nodes")} value={scriptDraft.nodes} set={(value) => updateScriptField("nodes", value)} type="number" /><JobField label={t("每节点任务数", "Tasks per node")} value={scriptDraft.tasksPerNode} set={(value) => updateScriptField("tasksPerNode", value)} type="number" /><label><span>{t("启动器", "Launcher")}</span><select value={scriptDraft.launcher} onChange={(event) => updateScriptField("launcher", event.target.value)}><option value="native">native</option><option value="openmpi">Open MPI</option><option value="mpich">MPICH</option><option value="intelmpi">Intel MPI</option></select></label><JobArea label={t("设备（名称=数量）", "Devices (name=quantity)")} value={scriptDraft.devices} set={(value) => updateScriptField("devices", value)} /></fieldset>
        <fieldset><legend>{t("运行参数", "Runtime")}</legend><JobField label={t("工作目录", "Working directory")} value={scriptDraft.workdir} set={(value) => updateScriptField("workdir", value)} /><JobArea label={t("环境变量（名称=值）", "Environment (name=value)")} value={scriptDraft.environment} set={(value) => updateScriptField("environment", value)} /><JobArea label={t("命令参数（每行一个）", "Arguments (one per line)")} value={scriptDraft.arguments} set={(value) => updateScriptField("arguments", value)} /></fieldset>
        <fieldset><legend>{t("批量清单", "Batch manifest")}</legend><JobField label={t("共享空间 ID", "Shared space ID")} value={scriptDraft.batchSpace} set={(value) => updateScriptField("batchSpace", value)} /><JobField label={t("清单文件相对路径", "Manifest relative path")} value={scriptDraft.batchFile} set={(value) => updateScriptField("batchFile", value)} /><JobField label={t("每项前置参数（如 -s）", "Per-item argument (for example -s)")} value={scriptDraft.batchArg} set={(value) => updateScriptField("batchArg", value)} /><JobField label={t("并行节点上限", "Maximum parallel nodes")} value={scriptDraft.batchNodes} set={(value) => updateScriptField("batchNodes", value)} type="number" /></fieldset>
      </div>}
      <label className="raw-script-toggle"><input type="checkbox" checked={rawScriptMode} onChange={(event) => toggleRawScript(event.target.checked)} /><span>{t("显示并直接编辑原始模板脚本", "Show and directly edit the raw template script")}</span></label>
      <div className="job-submit-actions"><span>{t("共享输入和输出请使用管理员配置的集群共享路径。", "Use administrator-configured cluster shared paths for shared input and output.")}</span><button className="primary-button" disabled={submitting} type="submit">{submitting ? t("正在校验并提交…", "Validating and submitting…") : t("校验并加入队列", "Validate and queue")}<span>→</span></button></div>
    </form>}

    {error && <p className="error-banner" role="alert">{error}</p>}
    <div className="job-toolbar"><div>{(["all", "active", "finished"] as const).map((item) => <button className={filter === item ? "active" : ""} type="button" key={item} onClick={() => setFilter(item)}>{t(({ all: "全部", active: "活动中", finished: "已完成" })[item], ({ all: "All", active: "Active", finished: "Finished" })[item])}</button>)}</div><span>{t(`每 ${refreshMs / 1000} 秒刷新 · ${jobs.length} 个作业`, `Refresh every ${refreshMs / 1000}s · ${jobs.length} jobs`)}</span></div>
    {logJob && <article className="job-log-panel">
      <header><div><span className="eyebrow">{t("任务输出", "Job output")}</span><h2>{t(`${shortID(logJob.id)} 的日志`, `Logs for ${shortID(logJob.id)}`)}</h2></div><div><span className={logStreaming ? "job-log-live" : ""}>{logStreaming ? t("实时流已连接", "Live stream connected") : t("正在重新连接", "Reconnecting")}</span>{logJob.logsTruncated && <span>{t("每个输出流保留最后 64 KiB", "Last 64 KiB retained per stream")}</span>}<button type="button" onClick={() => setLogJobID("")}>{t("关闭", "Close")}</button></div></header>
      <div>{logJob.reason && <LogStream title="system" content={logJob.reason} empty={t("该流没有输出", "No output in this stream")} />}<LogStream title="stdout" content={logJob.stdoutTail} empty={t("该流没有输出", "No output in this stream")} /><LogStream title="stderr" content={logJob.stderrTail} empty={t("该流没有输出", "No output in this stream")} />{logJob.allocation && <section className="allocation-log-members"><h3>{t("分配成员", "Allocation members")}</h3><div>{logJob.allocation.members.map((member) => <article key={member.assignmentId}><header><strong>{shortID(member.nodeId)}</strong><span>rank {rankRange(member.rankStart, member.rankCount)} · {member.state || t("准备中", "Preparing")}</span></header>{member.reason && <p>{member.reason}</p>}{member.stdoutTail && <pre>{member.stdoutTail}</pre>}{member.stderrTail && <pre>{member.stderrTail}</pre>}</article>)}</div></section>}</div>
    </article>}
    <div className="jobs-table">
      <div className="jobs-row jobs-row--header"><span>{t("作业 / 项目", "Job / project")}</span><span>{t("用户", "User")}</span><span>{t("状态", "Status")}</span><span>{t("队列 / 优先级", "Queue / priority")}</span><span>{t("目标资源", "Target resources")}</span><span>{t("节点 / 尝试", "Node / attempt")}</span><span>{t("更新时间", "Updated")}</span></div>
      {visible.map((job) => <div className="jobs-row" key={job.id}>
        <span><strong title={job.id}>{shortID(job.id)}</strong><small>{job.projectId} · {job.task.application || job.task.artifact.entrypoint}{job.arrayId ? ` · [${job.arrayIndex}]` : ""}</small></span>
        <span><strong title={job.ownerName || job.ownerId}>{job.ownerName || shortID(job.ownerId)}</strong><small title={job.ownerId}>{job.ownerId ? shortID(job.ownerId) : "—"}</small></span>
        <span><b className={`job-status job-status--${job.status}`}><i />{statusLabel(job.status, t)}</b>{job.blockReason ? <small title={blockReasonDetails(job)}>{blockReasonLabel(job.blockReason.code, t)}</small> : job.reason && <small title={job.reason}>{job.reason}</small>}</span>
        <span><strong>{job.queue}</strong><small>{t("优先级", "Priority")} {job.priority >= 0 ? `+${job.priority}` : job.priority}</small></span>
        <span><strong>{formatCoreCount(job.task.resources.milliCpu)} {t("逻辑核心", "logical cores")}{requestedGPUs(job) > 0 ? ` · GPU ${requestedGPUs(job)}` : ""}</strong><small>{job.allocation ? t("每节点 · ", "per node · ") : ""}{formatBytes(job.task.resources.memoryBytes)} · {job.task.placement?.requiredArchitecture || job.task.artifact.architecture || t("多构建变体", "multi-variant")}</small></span>
        <span><strong title={job.allocation?.members.map((member) => member.nodeId).join("\n") || job.nodeId}>{job.allocation ? t(`${job.allocation.members.length} 节点 · ${job.allocation.worldSize} ranks`, `${job.allocation.members.length} nodes · ${job.allocation.worldSize} ranks`) : job.nodeId ? shortID(job.nodeId) : t("等待节点", "Waiting for node")}</strong><small>{job.allocation ? `${job.allocation.launcher} · ${job.allocation.state}` : t(`第 ${job.attempt} / ${job.maxAttempts} 次`, `Attempt ${job.attempt} / ${job.maxAttempts}`)}</small></span>
        <span className="job-time"><strong>{relativeTime(job.updatedAt)}</strong><small>{new Date(job.updatedAt).toLocaleString()}</small>{hasLogViewer(job) && <button className="job-log-button" type="button" onClick={() => setLogJobID(job.id)}>{t("查看日志", "View logs")}</button>}{canCancel(job.status) && <button className="danger-button" type="button" disabled={cancelingID === job.id} onClick={() => void cancel(job)}>{cancelingID === job.id ? t("取消中…", "Canceling…") : t("取消作业", "Cancel job")}</button>}</span>
      </div>)}
      {!loading && visible.length === 0 && <div className="table-empty"><strong>{jobs.length ? t("当前筛选没有作业", "No jobs match this filter") : t("调度队列为空", "Scheduling queue is empty")}</strong><span>{t("使用 XIRANG 任务脚本提交新作业。", "Submit a new job with a XIRANG job script.")}</span></div>}
      {loading && jobs.length === 0 && <div className="table-loading"><span /><span /><span /></div>}
    </div>
  </section>;
}

function LogStream({ title, content, empty }: { title: string; content?: string; empty: string }) {
  return <section><h3>{title}</h3><pre>{content || empty}</pre></section>;
}

function JobField({ label, value, set, type = "text" }: { label: string; value: string; set: (value: string) => void; type?: string }) { return <label><span>{label}</span><input type={type} value={value} onChange={(event) => set(event.target.value)} /></label>; }
function JobArea({ label, value, set }: { label: string; value: string; set: (value: string) => void }) { return <label><span>{label}</span><textarea value={value} onChange={(event) => set(event.target.value)} /></label>; }

function isTerminal(status: JobStatus) { return status === "succeeded" || status === "failed" || status === "canceled"; }
function canCancel(status: JobStatus) { return status === "queued" || status === "assigned" || status === "running" || status === "suspended" || status === "orphaned"; }
function hasLogViewer(job: Job) { return Boolean(job.reason || job.stdoutTail || job.stderrTail || job.nodeId || isTerminal(job.status)); }
function statusLabel(status: JobStatus, t: (zh: string, en: string) => string) { return t(({ pending: "准备中", queued: "排队", assigned: "已分配", running: "运行中", suspended: "已挂起", canceling: "取消中", orphaned: "等待节点对账", succeeded: "成功", failed: "失败", canceled: "已取消" })[status], ({ pending: "Preparing", queued: "Queued", assigned: "Assigned", running: "Running", suspended: "Suspended", canceling: "Canceling", orphaned: "Awaiting reconciliation", succeeded: "Succeeded", failed: "Failed", canceled: "Canceled" })[status]); }
function shortID(value: string) { return value.length > 22 ? `${value.slice(0, 19)}…` : value; }
function requestedGPUs(job: Job): number {
  return Object.entries(job.task.resources.devices ?? {}).reduce((total, [name, quantity]) => {
    const resource = name.toLowerCase();
    const isGPU = resource.split("/").pop() === "gpu" || resource === "xirang.io/drm-render";
    const count = Number(quantity);
    return isGPU && Number.isFinite(count) && count > 0 ? total + count : total;
  }, 0);
}
function rankRange(start: number, count: number) { return count <= 1 ? `${start}` : `${start}-${start + count - 1}`; }
function blockReasonLabel(code: string, t: (zh: string, en: string) => string) {
  switch (code) {
    case "dependency_wait": return t("等待依赖任务", "Waiting for dependencies");
    case "queue_head_blocked": return t("等待队列前序任务", "Waiting behind queue head");
    case "queue_concurrency_limit": return t("队列并发额度已满", "Queue concurrency limit reached");
    case "project_concurrency_limit": return t("项目并发额度已满", "Project concurrency limit reached");
    case "queue_resource_quota": return t("队列资源额度不足", "Queue resource quota exhausted");
    case "project_resource_quota": return t("项目资源额度不足", "Project resource quota exhausted");
    case "wall_time_policy": return t("运行时长不符合队列策略", "Wall time exceeds queue policy");
    case "preemption_wait": return t("等待被抢占任务退出", "Waiting for preempted jobs");
    case "no_nodes": return t("集群中没有节点", "No enrolled nodes");
    case "no_ready_compute_node": return t("没有就绪的计算节点", "No ready compute node");
    case "architecture_unavailable": return t("所需架构不可用", "Required architecture unavailable");
    case "mpi_adapter_unavailable": return t("MPI 适配器节点不足", "Insufficient certified MPI nodes");
    case "device_capacity_unavailable": return t("GPU 或设备容量不足", "Accelerator capacity unavailable");
    case "placement_constraints": return t("节点约束不匹配", "Placement constraints do not match");
    case "resource_capacity": return t("CPU、内存或设备容量不足", "Insufficient CPU, memory, or device capacity");
    case "topology_unavailable": return t("拓扑数据不可用", "Topology data unavailable");
    case "topology_capacity": return t("网络拓扑或带宽不满足", "Network topology or bandwidth unavailable");
    case "gang_capacity": return t("多节点资源无法同时满足", "Gang capacity unavailable");
    default: return t("等待调度条件", "Waiting for scheduling conditions");
  }
}
function blockReasonDetails(job: Job) {
  if (!job.blockReason) return job.reason || "";
  const details = Object.entries(job.blockReason.details ?? {}).map(([key, value]) => `${key}=${value}`).join(" · ");
  return details ? `${job.blockReason.message}\n${details}` : job.blockReason.message;
}
