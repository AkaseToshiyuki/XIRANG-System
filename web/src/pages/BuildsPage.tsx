import { FormEvent, useCallback, useEffect, useState } from "react";
import { cancelBuild, cleanupBuilds, deleteBuild, getBuildState, getNodes, submitBuild, updateBuildRetention, updateBuildRuntimeTargets } from "../api/client";
import type { BuildRetentionPolicy, BuildWorker, ClusterNode, NativeBuild } from "../api/contracts";
import { formatBytes, formatCoreCount, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";
import { SharedPathPicker } from "../components/SharedPathPicker";

const initial = { name: "", application: "", variant: "", runtimeNodeIds: [] as string[], runtimeEntrypoint: "bin/app", runtimeRequirements: "{}", sourceSpaceId: "", sourcePath: "", architecture: "", builderArchitecture: "", workerId: "", crossCompile: false, command: "", artifactPath: "bin/app", outputSpaceId: "", outputPath: "builds/app", toolchain: "", cpu: "2", memoryGiB: "2", timeout: "1800" };

export function BuildsPage({ token }: { token: string }) {
  const { refreshMs, t } = usePreferences();
  const [builds, setBuilds] = useState<NativeBuild[]>([]);
  const [workers, setWorkers] = useState<BuildWorker[]>([]);
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [form, setForm] = useState(initial);
  const [showForm, setShowForm] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionID, setActionID] = useState("");
  const [retention, setRetention] = useState<BuildRetentionPolicy>({ maximumRecords: 500, maximumAgeSeconds: 30 * 86400, preserveSuccessfulBuilds: false });
  const [pathPicker, setPathPicker] = useState<"source" | "output" | "">("");
  const [targetDraft, setTargetDraft] = useState<Record<string, string[]>>({});

  const refresh = useCallback(async (signal?: AbortSignal) => {
    const [state, clusterNodes] = await Promise.all([getBuildState(token, signal), getNodes(token, signal)]);
    setBuilds(state.builds); setWorkers(state.workers); setRetention(state.retention);
    setNodes(clusterNodes);
    setTargetDraft((current) => Object.fromEntries(state.builds.map((build) => [build.id, current[build.id] ?? build.artifact?.runtimeNodeIds ?? []])));
    if (state.workers[0]) setForm((current) => {
      const availableToolchains = [...new Set(state.workers.flatMap((worker) => worker.toolchains))];
      const toolchain = availableToolchains.includes(current.toolchain) ? current.toolchain : (availableToolchains[0] ?? "");
      const architecture = current.architecture || state.workers[0].architecture;
      return { ...current, architecture, builderArchitecture: current.builderArchitecture || state.workers[0].architecture, workerId: current.workerId || state.workers[0].id, toolchain, command: current.command || defaultBuildCommand(toolchain, architecture, state.workers[0]) };
    });
  }, [token]);

  useEffect(() => {
    const controller = new AbortController();
    refresh(controller.signal).catch((cause) => !controller.signal.aborted && setError(cause instanceof Error ? cause.message : t("无法读取构建状态", "Unable to load builds")));
    const timer = window.setInterval(() => refresh().catch(() => undefined), refreshMs);
    return () => { controller.abort(); window.clearInterval(timer); };
  }, [refresh, refreshMs, t]);

  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try {
      if (form.application && (!form.variant || !form.outputSpaceId || form.runtimeNodeIds.length === 0)) throw new Error(t("应用构建必须填写变体、选择共享输出空间并映射至少一个运行节点", "Application builds require a variant, a shared output space, and at least one runtime node"));
      await submitBuild({
        name: form.name, sourceSpaceId: form.sourceSpaceId, sourcePath: form.sourcePath,
        application: form.application, variant: form.variant, runtimeNodeIds: form.runtimeNodeIds, runtimeEntrypoint: form.runtimeEntrypoint, runtimeRequirements: parseRuntimeRequirements(form.runtimeRequirements),
        architecture: form.architecture, builderArchitecture: form.crossCompile ? form.builderArchitecture : form.architecture,
        workerId: form.workerId, crossCompile: form.crossCompile, steps: parseBuildSteps(form.command), outputPath: form.artifactPath, outputSpaceId: form.outputSpaceId, publishPath: form.outputPath,
        toolchain: form.toolchain, milliCpu: Number(form.cpu) * 1000, memoryBytes: Number(form.memoryGiB) * 1024 ** 3,
        timeoutSeconds: Number(form.timeout), environment: form.crossCompile && form.toolchain === "go" ? { GOOS: "linux", GOARCH: form.architecture } : {},
      }, token);
      setShowForm(false); setForm({ ...initial, architecture: workers[0]?.architecture ?? "", builderArchitecture: workers[0]?.architecture ?? "", workerId: workers[0]?.id ?? "" }); await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : t("构建提交失败", "Build submission failed")); }
    finally { setBusy(false); }
  }

  async function applyRetention() {
    setBusy(true); setError("");
    try { setRetention(await updateBuildRetention(retention, token)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("无法保存保留策略", "Unable to save retention policy")); }
    finally { setBusy(false); }
  }

  async function cleanHistory() {
    if (!window.confirm(t("立即按照保留策略清理终态构建记录和对应编译产物？已启用永久保留时，成功构建不会被清理。", "Clean terminal build records and compiled outputs according to the retention policy now? Successful builds remain when permanent preservation is enabled."))) return;
    setBusy(true); setError("");
    try { await cleanupBuilds(token); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("构建历史清理失败", "Build cleanup failed")); }
    finally { setBusy(false); }
  }

  async function actOnBuild(build: NativeBuild, action: "cancel" | "delete") {
    const prompt = action === "cancel" ? t(`取消构建 ${build.recipe.name}？`, `Cancel build ${build.recipe.name}?`) : t(`删除构建 ${build.recipe.name} 的记录和编译产物？`, `Delete the record and compiled outputs for ${build.recipe.name}?`);
    if (!window.confirm(prompt)) return;
    setActionID(build.id); setError("");
    try { if (action === "cancel") await cancelBuild(build.id, token); else await deleteBuild(build.id, token); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("构建操作失败", "Build operation failed")); }
    finally { setActionID(""); }
  }

  async function saveRuntimeTargets(build: NativeBuild) {
    setActionID(build.id); setError("");
    try { await updateBuildRuntimeTargets(build.id, targetDraft[build.id] ?? [], token); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("无法保存运行节点映射", "Unable to save runtime node mapping")); }
    finally { setActionID(""); }
  }

  const architectures = [...new Set(["amd64", "arm64", "riscv64", ...workers.flatMap((worker) => worker.toolchainCapabilities?.flatMap((item) => item.targetArchitectures) ?? [worker.architecture])])];
  const toolchains = [...new Set(workers.flatMap((worker) => worker.toolchains))];
  const builderArchitecture = form.crossCompile ? form.builderArchitecture : form.architecture;
  const eligibleWorkers = workers.filter((worker) => worker.ready && worker.architecture === builderArchitecture && supportsTarget(worker, form.toolchain, form.architecture));
  const compatibleRuntimeNodes = nodes.filter((node) => node.architecture === form.architecture && (node.roles?.includes("compute") ?? true));
  return <section>
    <header className="page-header"><div><span className="eyebrow">{t("生成可验证的任务编译产物", "Compile verifiable job outputs")}</span><h1>{t("原生构建", "Native builds")}</h1><p>{t("选择匹配架构的源码与工具链，完成后复制编译产物 URI、摘要和签名到任务脚本。", "Select source and a matching architecture toolchain, then use the signed compiled output in a job script.")}</p></div><button className="primary-button" disabled={!workers.some((worker) => worker.ready)} onClick={() => setShowForm((value) => !value)}>{showForm ? t("关闭配方", "Close recipe") : t("新建构建", "New build")}<span>+</span></button></header>
    {error && <p className="error-banner" role="alert">{error}</p>}
    <section className="build-worker-section"><header><span className="eyebrow">{t("可用于执行构建", "Eligible for build execution")}</span><h2>{t("构建节点与工具链能力", "Build nodes and toolchain capabilities")}</h2></header><div className="build-workers">{workers.map((worker) => <article key={worker.id}><i className={worker.ready ? "ready" : ""} /><div><span>{worker.id}</span><strong>{worker.architecture}</strong>{workerMatrix(worker).map((item) => <small key={item.name}>{item.name}{item.version ? ` · ${item.version}` : ""}<b> → {item.targetArchitectures.join(" / ")}</b>{item.executables?.length ? <code>{item.executables.join(" · ")}</code> : null}</small>)}</div><em>{worker.isolation}</em></article>)}</div></section>
    <section className="build-retention"><header><div><span className="eyebrow">{t("控制历史增长", "Control history growth")}</span><h2>{t("构建保留策略", "Build retention policy")}</h2></div><button type="button" onClick={cleanHistory} disabled={busy}>{t("立即清理", "Clean now")}</button></header><div><label><span>{t("最多保留记录", "Maximum records")}</span><input type="number" min="1" max="1000" value={retention.maximumRecords} onChange={(event) => setRetention({ ...retention, maximumRecords: Number(event.target.value) })} /></label><label><span>{t("最长保留天数", "Maximum age in days")}</span><input type="number" min="1" max="3650" value={Math.round(retention.maximumAgeSeconds / 86400)} onChange={(event) => setRetention({ ...retention, maximumAgeSeconds: Number(event.target.value) * 86400 })} /></label><label className="build-retention-toggle"><input type="checkbox" checked={retention.preserveSuccessfulBuilds} onChange={(event) => setRetention({ ...retention, preserveSuccessfulBuilds: event.target.checked })} /><span><strong>{t("永久保留成功构建", "Preserve successful builds permanently")}</strong><small>{t("自动清理仍会移除过期的失败和取消记录", "Automatic cleanup still removes expired failed and canceled records")}</small></span></label><button className="primary-button" type="button" disabled={busy} onClick={applyRetention}>{t("保存策略", "Save policy")}</button></div></section>
    {workers.length > 0 && !workers.some((worker) => worker.ready) && <p className="form-notice">{t("Build API 已连接，但本机原生构建 worker 尚未启用。管理员可启用 build-enabled 后提交配方。", "The Build API is connected, but the native worker is not enabled. Enable build-enabled before submitting recipes.")}</p>}
    {showForm && <form className="build-form" onSubmit={submit}>
      <header><div><span className="eyebrow">{t("填写构建参数", "Enter build parameters")}</span><h2>{t("提交可验证源码", "Submit verifiable source")}</h2></div><p>{t("控制节点会自动计算并固定源码树 SHA-256；输入与发布位置必须位于已挂载共享空间，源码不能包含符号链接。", "The controller automatically calculates and pins the source-tree SHA-256. Input and publish locations must be mounted shared spaces and source cannot contain symlinks.")}</p></header>
      <div><BuildField label={t("名称", "Name")} value={form.name} set={(value) => setForm({ ...form, name: value })} /><BuildPathField label={t("源码相对路径", "Source relative path")} value={form.sourcePath} set={(value) => setForm({ ...form, sourcePath: value })} browse={() => setPathPicker("source")} /></div>
      <div><BuildField label={t("应用名（如 gromacs）", "Application (for example gromacs)")} value={form.application} set={(value) => setForm({ ...form, application: value })} required={false} /><BuildField label={t("构建变体（如 amd64-nvidia）", "Build variant (for example amd64-nvidia)")} value={form.variant} set={(value) => setForm({ ...form, variant: value })} required={false} /></div>
      <div><BuildSelect label={t("目标架构", "Target architecture")} value={form.architecture} options={architectures} set={(value) => setForm({ ...form, architecture: value, command: defaultBuildCommand(form.toolchain, value, workers.find((worker) => worker.id === form.workerId)), ...(!form.crossCompile ? { builderArchitecture: value, workerId: "" } : {}) })} placeholder={t("请选择", "Select")} /><BuildSelect label={t("工具链", "Toolchain")} value={form.toolchain} options={toolchains} set={(value) => setForm({ ...form, toolchain: value, command: defaultBuildCommand(value, form.architecture, workers.find((worker) => worker.id === form.workerId)), workerId: "" })} placeholder={t("请选择", "Select")} /></div>
      <label className="build-cross-toggle"><input type="checkbox" checked={form.crossCompile} onChange={(event) => setForm({ ...form, crossCompile: event.target.checked, builderArchitecture: event.target.checked ? form.builderArchitecture || workers[0]?.architecture || "" : form.architecture, workerId: "" })} /><span><strong>{t("进行交叉编译", "Cross-compile")}</strong><small>{t("目标架构与执行编译的节点架构不同", "Target architecture differs from the node running the compiler")}</small></span></label>
      <div>{form.crossCompile && <BuildSelect label={t("编译机架构", "Builder architecture")} value={form.builderArchitecture} options={architectures.filter((arch) => workers.some((worker) => worker.architecture === arch))} set={(value) => setForm({ ...form, builderArchitecture: value, workerId: "" })} placeholder={t("请选择", "Select")} />}<BuildSelect label={t("执行节点", "Build node")} value={form.workerId} options={eligibleWorkers.map((worker) => worker.id)} set={(value) => setForm({ ...form, workerId: value })} placeholder={eligibleWorkers.length ? t("自动选择符合条件的节点", "Automatically select an eligible node") : t("没有符合条件的节点", "No eligible node")} required={false} /></div>
      <BuildArea label={t("构建步骤（每行一个 argv 命令，不经 shell）", "Build steps (one argv command per line, no shell)")} value={form.command} set={(value) => setForm({ ...form, command: value })} />
      <div><BuildField label={t("构建产物相对路径", "Build artifact relative path")} value={form.artifactPath} set={(value) => setForm({ ...form, artifactPath: value })} /><BuildPathField label={t("共享输出相对路径", "Shared output relative path")} value={form.outputPath} set={(value) => setForm({ ...form, outputPath: value })} browse={() => setPathPicker("output")} /></div>
      {form.application && <fieldset className="build-runtime-targets"><legend>{t("运行时契约与节点映射", "Runtime contract and node mapping")}</legend><BuildField label={t("运行入口", "Runtime entrypoint")} value={form.runtimeEntrypoint} set={(value) => setForm({ ...form, runtimeEntrypoint: value })} /><BuildArea label={t("额外运行时需求（JSON；版本可用 >=、<=、>、<）", "Additional runtime requirements (JSON; versions may use >=, <=, >, <)")} value={form.runtimeRequirements} set={(value) => setForm({ ...form, runtimeRequirements: value })} /><div className="build-node-options">{compatibleRuntimeNodes.map((node) => <label key={node.id}><input type="checkbox" checked={form.runtimeNodeIds.includes(node.id)} onChange={(event) => setForm({ ...form, runtimeNodeIds: event.target.checked ? [...form.runtimeNodeIds, node.id] : form.runtimeNodeIds.filter((id) => id !== node.id) })} /><span><strong>{node.name}</strong><small>{node.architecture} · {gpuSummary(node, t)}</small></span></label>)}</div></fieldset>}
      <div><BuildField label={t("内存 (G)", "Memory (G)")} value={form.memoryGiB} set={(value) => setForm({ ...form, memoryGiB: value })} type="number" /><BuildField label={t("超时 (秒)", "Timeout (seconds)")} value={form.timeout} set={(value) => setForm({ ...form, timeout: value })} type="number" /></div>
      <button className="primary-button" disabled={busy}>{busy ? t("正在提交…", "Submitting…") : t("进入原生构建池", "Submit to native build pool")}<span>→</span></button>
    </form>}
    <header className="section-heading"><span className="eyebrow">{t("任务状态与历史", "Status and history")}</span><h2>{t("历史构建", "Historical builds")}</h2></header><div className="build-list">{builds.map((build) => <article key={build.id}>
      <header><span className={`build-state build-state--${build.status}`}>{build.status}</span><div><h2>{build.recipe.name}</h2><small>{build.id} · {build.workerId}</small></div><div className="build-actions">{build.status === "running" && workers.find((worker) => worker.id === build.workerId)?.supportsCancellation && <button type="button" disabled={actionID === build.id} onClick={() => actOnBuild(build, "cancel")}>{t("取消", "Cancel")}</button>}{["succeeded", "failed", "canceled"].includes(build.status) && <button className="danger-button" type="button" disabled={actionID === build.id} onClick={() => actOnBuild(build, "delete")}>{t("删除", "Delete")}</button>}<time>{relativeTime(build.createdAt)}</time></div></header>
      <div className="build-meta"><span><small>TARGET / BUILDER</small><strong>{build.recipe.architecture} / {build.recipe.builderArchitecture || build.recipe.architecture}</strong></span><span><small>RESOURCES</small><strong>{formatCoreCount(build.recipe.milliCpu)} {t("逻辑核心", "logical cores")} · {formatBytes(build.recipe.memoryBytes)}</strong></span><span><small>TOOLCHAIN</small><strong>{build.recipe.toolchain}</strong></span></div>
      {build.recipe.sourceSha256 && <div className="build-source-digest"><small>{t("控制节点固定的源码树 SHA-256", "Controller-pinned source tree SHA-256")}</small><code>{build.recipe.sourceSha256}</code></div>}
      {build.failure && <p className="form-error">{build.failure}</p>}
      {build.artifact?.sha256 && <div className="build-artifact"><div><span>{t("已签名编译产物 · SHA-256", "Signed compiled output · SHA-256")}</span><code>{build.artifact.sha256}</code><details><summary>{t("任务提交所需的 URI 与 Ed25519 签名", "URI and Ed25519 signature for job submission")}</summary><small>URI</small><code>{build.artifact.uri}</code><small>SIGNATURE · BASE64</small><code>{build.artifact.signature}</code><small>PUBLIC KEY · BASE64</small><code>{build.artifact.publicKey}</code></details></div><strong>{build.artifact.abi}</strong><a href={build.artifact.sbomUri}>SPDX SBOM ↗</a></div>}
      {build.artifact?.application && <details className="build-routing"><summary>{build.artifact.application} / {build.artifact.variant} · {t("运行节点映射", "Runtime node mapping")}</summary><div className="build-node-options">{nodes.filter((node) => node.architecture === build.artifact?.architecture).map((node) => <label key={node.id}><input type="checkbox" checked={(targetDraft[build.id] ?? []).includes(node.id)} onChange={(event) => setTargetDraft({ ...targetDraft, [build.id]: event.target.checked ? [...(targetDraft[build.id] ?? []), node.id] : (targetDraft[build.id] ?? []).filter((id) => id !== node.id) })} /><span><strong>{node.name}</strong><small>{node.architecture} · {gpuSummary(node, t)}</small></span></label>)}</div><button className="primary-button" type="button" disabled={actionID === build.id} onClick={() => saveRuntimeTargets(build)}>{t("保存映射", "Save mapping")}</button></details>}
      {build.logTail && <pre>{build.logTail}</pre>}
    </article>)}{builds.length === 0 && <div className="queue-empty"><strong>{t("尚无构建记录", "No build records yet")}</strong><span>{t("启用本地架构 worker 后提交第一份声明式配方", "Enable a native worker, then submit the first declarative recipe")}</span></div>}</div>{pathPicker && <SharedPathPicker token={token} initialSpaceID={pathPicker === "source" ? form.sourceSpaceId : form.outputSpaceId} initialPath={pathPicker === "source" ? form.sourcePath : form.outputPath.includes("/") ? form.outputPath.slice(0, form.outputPath.lastIndexOf("/")) : ""} onClose={() => setPathPicker("")} onSelect={(spaceID, path) => { if (pathPicker === "source") setForm({ ...form, sourceSpaceId: spaceID, sourcePath: path }); else { const name = form.outputPath.split("/").pop() || "app"; setForm({ ...form, outputSpaceId: spaceID, outputPath: path ? `${path}/${name}` : name }); } setPathPicker(""); }} />}
  </section>;
}

function BuildField({ label, value, set, type = "text", required = true }: { label: string; value: string; set: (value: string) => void; type?: string; required?: boolean }) {
  return <label><span>{label}</span><input type={type} value={value} onChange={(event) => set(event.target.value)} required={required} /></label>;
}

function parseRuntimeRequirements(value: string): Record<string, string> {
  const parsed: unknown = JSON.parse(value);
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object" || Object.values(parsed).some((item) => typeof item !== "string")) {
    throw new Error("Runtime requirements must be a JSON object whose values are strings");
  }
  return parsed as Record<string, string>;
}
function BuildPathField({ label, value, set, browse }: { label: string; value: string; set: (value: string) => void; browse: () => void }) { return <label><span>{label}</span><div className="build-path-input"><input value={value} onChange={(event) => set(event.target.value)} required /><button type="button" onClick={browse} aria-label={label}>◇</button></div></label>; }
function BuildArea({ label, value, set }: { label: string; value: string; set: (value: string) => void }) { return <label><span>{label}</span><textarea value={value} onChange={(event) => set(event.target.value)} rows={4} required /></label>; }
function BuildSelect({ label, value, options, set, placeholder = "Select", required = true }: { label: string; value: string; options: string[]; set: (value: string) => void; placeholder?: string; required?: boolean }) { return <label><span>{label}</span><select value={value} onChange={(event) => set(event.target.value)} required={required}><option value="">{placeholder}</option>{options.map((option) => <option value={option} key={option}>{option}</option>)}</select></label>; }
type WorkerToolchain = { name: string; version?: string; targetArchitectures: string[]; executables?: string[] };
function workerMatrix(worker: BuildWorker): WorkerToolchain[] { return worker.toolchainCapabilities?.length ? worker.toolchainCapabilities : worker.toolchains.map((name) => ({ name, targetArchitectures: [worker.architecture] })); }
function supportsTarget(worker: BuildWorker, toolchain: string, target: string) { return workerMatrix(worker).some((item) => item.name === toolchain && item.targetArchitectures.includes(target)); }
function gpuSummary(node: ClusterNode, t: (zh: string, en: string) => string) {
  const devices = Object.entries(node.capacity.devices ?? {}).filter(([name, quantity]) => quantity > 0 && /gpu/i.test(name));
  return devices.length ? devices.map(([name, quantity]) => `${name} × ${quantity}`).join(" · ") : t("仅 CPU", "CPU only");
}
function defaultBuildCommand(toolchain: string, target = "", worker?: BuildWorker) {
  if (toolchain === "cc" && worker && target !== worker.architecture) {
    const executable = workerMatrix(worker).find((item) => item.name === "cc")?.executables?.find((item) => item.startsWith(target === "arm64" ? "aarch64-" : target === "amd64" ? "x86_64-" : `${target}-`));
    if (executable) return `${executable} -O2 -o bin/app main.c`;
  }
  return ({ go: "go build -trimpath -o bin/app ./...", cc: "cc -O2 -o bin/app main.c", make: "make", cmake: "cmake -S . -B build\ncmake --build build --parallel", cargo: "cargo build --release" } as Record<string, string>)[toolchain] ?? "";
}

function parseBuildSteps(value: string): string[][] {
  return value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean).map((line) => {
    const result: string[] = []; let token = ""; let quote = ""; let escaped = false;
    for (const character of line) {
      if (escaped) { token += character; escaped = false; continue; }
      if (character === "\\") { escaped = true; continue; }
      if (quote) { if (character === quote) quote = ""; else token += character; continue; }
      if (character === "\"" || character === "'") { quote = character; continue; }
      if (/\s/.test(character)) { if (token) { result.push(token); token = ""; } continue; }
      token += character;
    }
    if (escaped || quote) throw new Error(`Invalid quoting in build step: ${line}`);
    if (token) result.push(token);
    if (!result.length) throw new Error("Build step cannot be empty");
    return result;
  });
}
