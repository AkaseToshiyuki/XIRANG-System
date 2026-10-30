import { ChangeEvent, Dispatch, FormEvent, SetStateAction, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  applySharedSpace, createSharedDirectory, deleteSharedFile, downloadSharedFile,
  getNodes, getSharedFiles, getSharedSpaces, renameSharedFile, uploadSharedFile,
  unshareSharedSpace,
} from "../api/client";
import type { ClusterNode, SharedFile, SharedSpace } from "../api/contracts";
import { formatBytes, relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

type TargetDraft = Pick<SharedSpace, "target" | "targetGroup" | "targetNodeIds">;
type SharedSpaceForm = { name: string; sourceNodeId: string; sourcePath: string; mountPath: string } & TargetDraft;
const initial: SharedSpaceForm = { name: "", sourceNodeId: "", sourcePath: "/srv/xirang/shared", mountPath: "/shared", target: "all", targetGroup: "", targetNodeIds: [] };

export function StoragePage({ token, isAdmin }: { token: string; isAdmin: boolean }) {
  const { refreshMs, t } = usePreferences();
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [spaces, setSpaces] = useState<SharedSpace[]>([]);
  const [form, setForm] = useState(initial);
  const [editing, setEditing] = useState<string | null>(null);
  const [targetDraft, setTargetDraft] = useState<TargetDraft>({ target: "all", targetGroup: "", targetNodeIds: [] });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const [nextNodes, nextSpaces] = await Promise.all([getNodes(token, signal), getSharedSpaces(token, signal)]);
      setNodes(nextNodes);
      setSpaces(nextSpaces);
      setForm((current) => ({ ...current, sourceNodeId: current.sourceNodeId || nextNodes.find((node) => roles(node).includes("storage"))?.id || "" }));
      setError("");
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : t("无法读取共享空间", "Unable to load shared storage"));
    }
  }, [t, token]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    const interval = window.setInterval(() => void load(controller.signal), refreshMs);
    return () => { controller.abort(); window.clearInterval(interval); };
  }, [load, refreshMs]);

  const storageNodes = nodes.filter((node) => roles(node).includes("storage"));
  const groups = useMemo(() => [...new Set(nodes.map((node) => node.group).filter(Boolean) as string[])], [nodes]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      const created = await applySharedSpace(form, token);
      setSpaces((current) => [created, ...current.filter((item) => item.id !== created.id)]);
      setForm((current) => ({ ...initial, sourceNodeId: current.sourceNodeId }));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("共享空间应用失败", "Unable to apply shared storage"));
    } finally {
      setBusy(false);
    }
  }

  function beginTargetEdit(space: SharedSpace) {
    setEditing(space.id);
    setTargetDraft({ target: space.target, targetGroup: space.targetGroup ?? "", targetNodeIds: space.targetNodeIds ?? [] });
  }

  async function saveTarget(space: SharedSpace) {
    setBusy(true);
    try {
      const updated = await applySharedSpace({ ...space, ...targetDraft }, token);
      setSpaces((current) => current.map((item) => item.id === updated.id ? updated : item));
      setEditing(null);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("挂载范围更新失败", "Unable to update mount targets"));
    } finally {
      setBusy(false);
    }
  }

  async function remove(space: SharedSpace) {
    if (!window.confirm(t(`解除共享“${space.name}”？系统会撤销节点挂载与共享配置，但保留存储节点上的目录和文件。`, `Unshare “${space.name}”? XIRANG removes mounts and sharing configuration but keeps the source directory and files.`))) return;
    setBusy(true);
    try {
      await unshareSharedSpace(space.id, token);
      setSpaces((current) => current.filter((item) => item.id !== space.id));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : t("解除共享失败", "Unable to unshare"));
    } finally {
      setBusy(false);
    }
  }

  return <section>
    <header className="page-header"><div><h1>{t("共享空间", "Shared storage")}</h1></div></header>
    {error && <p className="error-banner" role="alert">{error}</p>}
    {isAdmin && <form className="storage-form" onSubmit={submit}>
      <label><span>{t("名称", "Name")}</span><input required value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder={t("项目共享空间", "Project shared space")} /></label>
      <label><span>{t("存储节点", "Storage node")}</span><select required value={form.sourceNodeId} onChange={(event) => setForm({ ...form, sourceNodeId: event.target.value })}><option value="">{t("选择存储节点", "Select a storage node")}</option>{storageNodes.map((node) => <option value={node.id} key={node.id}>{node.name}</option>)}</select></label>
      <label><span>{t("存储节点路径", "Source path")}</span><input required value={form.sourcePath} onChange={(event) => setForm({ ...form, sourcePath: event.target.value })} /></label>
      <label><span>{t("节点挂载路径", "Mount path")}</span><input required value={form.mountPath} onChange={(event) => setForm({ ...form, mountPath: event.target.value })} /></label>
      <TargetFields draft={form} setDraft={setForm} nodes={nodes} groups={groups} />
      <div className="storage-form-actions"><span>{t("节点或分组成员变化后，挂载状态会自动同步。", "Mounts automatically follow node and group membership changes.")}</span><button className="primary-button" disabled={busy || storageNodes.length === 0}>{busy ? t("正在应用…", "Applying…") : t("创建并挂载", "Create and mount")}<span aria-hidden="true">→</span></button></div>
    </form>}
    {isAdmin && storageNodes.length === 0 && <p className="form-notice">{t("请先在“节点”页面为至少一个节点启用“存储”角色。", "Enable the storage role on at least one node first.")}</p>}
    <div className="storage-list">
      {spaces.map((space) => <article key={space.id} className="storage-card">
        <header><div><strong>{space.name}</strong><small>{space.sourcePath} → {space.mountPath}</small></div><b className={`storage-state storage-state--${space.status}`}>{space.status}</b></header>
        <p>{targetLabel(space, t)} · {t("更新于", "Updated")} {relativeTime(space.updatedAt)}</p>
        <div className="storage-capacity">
          <span><small>{t("文件夹占用", "Folder used")}</small><strong>{space.capacityObservedAt === undefined ? "—" : formatBytes(space.usedBytes ?? 0)}</strong></span>
          <span><small>{t("所在磁盘剩余", "Filesystem free")}</small><strong>{space.availableBytes === undefined ? "—" : formatBytes(space.availableBytes)}</strong></span>
          <span><small>{t("磁盘总容量", "Filesystem total")}</small><strong>{space.totalBytes === undefined ? "—" : formatBytes(space.totalBytes)}</strong></span>
          {space.capacityError && <em title={space.capacityError}>{t("容量探测暂不可用", "Capacity probe unavailable")}</em>}
        </div>
        {isAdmin && <div className="storage-target-actions">
          {space.id === "share_default" ? <span className="storage-system-badge">{t("系统默认 · 自动挂载全部节点", "System default · all nodes")}</span> : <>
            <button className="secondary-button" type="button" onClick={() => beginTargetEdit(space)}>{t("编辑挂载范围", "Edit mount targets")}</button>
            <button className="danger-button" type="button" disabled={busy} onClick={() => void remove(space)}>{t("解除共享", "Unshare")}</button>
          </>}
        </div>}
        {editing === space.id && <div className="storage-target-editor">
          <TargetFields draft={targetDraft} setDraft={setTargetDraft} nodes={nodes} groups={groups} />
          <footer><button type="button" className="secondary-button" onClick={() => setEditing(null)}>{t("取消", "Cancel")}</button><button type="button" className="primary-button" disabled={busy} onClick={() => void saveTarget(space)}>{t("保存并同步", "Save and reconcile")}<span aria-hidden="true">→</span></button></footer>
        </div>}
        {isAdmin && <div className="storage-results">{space.results?.map((result) => <span key={`${result.nodeId}-${result.status}`} title={result.message}>{nodeName(nodes, result.nodeId)}：{result.status}</span>)}</div>}
        <SharedFileBrowser space={space} token={token} isAdmin={isAdmin} reportError={setError} />
      </article>)}
      {spaces.length === 0 && <div className="table-empty"><strong>{t("尚未配置共享空间", "No shared storage configured")}</strong><span>{isAdmin ? t("创建后可作为任务工作目录使用。", "Create one to use it as a task workspace.") : t("请联系管理员创建共享空间。", "Ask an administrator to create shared storage.")}</span></div>}
    </div>
  </section>;
}

function TargetFields<T extends TargetDraft>({ draft, setDraft, nodes, groups }: { draft: T; setDraft: Dispatch<SetStateAction<T>>; nodes: ClusterNode[]; groups: string[] }) {
  const { t } = usePreferences();
  return <>
    <label><span>{t("挂载范围", "Mount target")}</span><select value={draft.target} onChange={(event) => setDraft({ ...draft, target: event.target.value as T["target"] })}><option value="all">{t("全部节点（动态）", "All nodes (dynamic)")}</option><option value="group">{t("指定分组（动态）", "Node group (dynamic)")}</option><option value="nodes">{t("指定节点", "Selected nodes")}</option></select></label>
    {draft.target === "group" && <label><span>{t("节点分组", "Node group")}</span><select required value={draft.targetGroup ?? ""} onChange={(event) => setDraft({ ...draft, targetGroup: event.target.value })}><option value="">{t("选择分组", "Select a group")}</option>{groups.map((group) => <option key={group}>{group}</option>)}</select></label>}
    {draft.target === "nodes" && <fieldset><legend>{t("目标节点", "Target nodes")}</legend>{nodes.map((node) => <label key={node.id}><input type="checkbox" checked={(draft.targetNodeIds ?? []).includes(node.id)} onChange={(event) => setDraft({ ...draft, targetNodeIds: event.target.checked ? [...(draft.targetNodeIds ?? []), node.id] : (draft.targetNodeIds ?? []).filter((id) => id !== node.id) })} />{node.name}</label>)}</fieldset>}
  </>;
}

function SharedFileBrowser({ space, token, isAdmin, reportError }: { space: SharedSpace; token: string; isAdmin: boolean; reportError: (message: string) => void }) {
  const { t } = usePreferences();
  const uploadInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);
  const [directories, setDirectories] = useState<Record<string, SharedFile[]>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [destination, setDestination] = useState("");
  const [busy, setBusy] = useState(false);
  const [transferStatus, setTransferStatus] = useState("");
  const [search, setSearch] = useState("");

  const loadDirectory = useCallback(async (path: string) => {
    try {
      const entries = await getSharedFiles(space.id, path, token);
      setDirectories((current) => ({ ...current, [path]: entries }));
    } catch (cause) {
      reportError(cause instanceof Error ? cause.message : t("无法读取文件", "Unable to list files"));
    }
  }, [reportError, space.id, t, token]);

  useEffect(() => { void loadDirectory(""); }, [loadDirectory]);
  useEffect(() => { folderInput.current?.setAttribute("webkitdirectory", ""); }, []);

  async function refreshParents(path: string) {
    const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
    await loadDirectory(parent);
    if (directories[path]) await loadDirectory(path);
  }

  async function toggle(entry: SharedFile) {
    if (!entry.directory) return;
    setDestination(entry.path);
    const next = new Set(expanded);
    if (next.has(entry.path)) next.delete(entry.path);
    else {
      next.add(entry.path);
      if (!directories[entry.path]) await loadDirectory(entry.path);
    }
    setExpanded(next);
  }

  async function navigate(path: string) {
    setDestination(path);
    if (path) {
      const parts = path.split("/");
      setExpanded((current) => {
        const next = new Set(current);
        for (let index = 1; index <= parts.length; index += 1) next.add(parts.slice(0, index).join("/"));
        return next;
      });
    }
    if (!directories[path]) await loadDirectory(path);
  }

  async function upload(event: ChangeEvent<HTMLInputElement>, preserveHierarchy: boolean) {
    const selectedFiles = Array.from(event.target.files ?? []);
    event.target.value = "";
    if (selectedFiles.length === 0) return;
    setBusy(true);
    setTransferStatus("");
    try {
      const files = selectedFiles.map((file) => ({
        file,
        relativePath: normalizeUploadPath(preserveHierarchy ? file.webkitRelativePath || file.name : file.name),
      }));
      const directoryPaths = new Set<string>();
      for (const { relativePath } of files) {
        const segments = relativePath.split("/");
        segments.pop();
        for (let depth = 1; depth <= segments.length; depth += 1) {
          directoryPaths.add(segments.slice(0, depth).join("/"));
        }
      }
      const orderedDirectories = [...directoryPaths].sort((left, right) => {
        const depthDifference = left.split("/").length - right.split("/").length;
        return depthDifference || left.localeCompare(right);
      });
      for (let index = 0; index < orderedDirectories.length; index += 1) {
        setTransferStatus(t(`正在创建目录 ${index + 1}/${orderedDirectories.length}`, `Creating folders ${index + 1}/${orderedDirectories.length}`));
        await createSharedDirectory(space.id, joinPath(destination, orderedDirectories[index]), token);
      }
      for (let index = 0; index < files.length; index += 1) {
        setTransferStatus(t(`正在上传文件 ${index + 1}/${files.length}`, `Uploading files ${index + 1}/${files.length}`));
        await uploadSharedFile(space.id, joinPath(destination, files[index].relativePath), files[index].file, token);
      }
      await loadDirectory(destination);
      setTransferStatus(t(`已上传 ${files.length} 个文件`, `${files.length} files uploaded`));
    } catch (cause) {
      reportError(cause instanceof Error ? cause.message : t("上传失败", "Upload failed"));
      setTransferStatus("");
    } finally {
      setBusy(false);
    }
  }

  async function createDirectory() {
    const name = window.prompt(t("新文件夹名称", "New folder name"));
    if (!name) return;
    const path = joinPath(destination, name);
    setBusy(true);
    try {
      await createSharedDirectory(space.id, path, token);
      await refreshParents(path);
    } catch (cause) {
      reportError(cause instanceof Error ? cause.message : t("创建文件夹失败", "Unable to create directory"));
    } finally {
      setBusy(false);
    }
  }

  async function rename(entry: SharedFile) {
    const name = window.prompt(t("新的名称", "New name"), entry.name);
    if (!name || name === entry.name) return;
    const parent = entry.path.includes("/") ? entry.path.slice(0, entry.path.lastIndexOf("/")) : "";
    setBusy(true);
    try {
      await renameSharedFile(space.id, entry.path, joinPath(parent, name), token);
      await loadDirectory(parent);
    } catch (cause) {
      reportError(cause instanceof Error ? cause.message : t("重命名失败", "Rename failed"));
    } finally {
      setBusy(false);
    }
  }

  async function remove(entry: SharedFile) {
    if (!window.confirm(t(`删除“${entry.name}”？`, `Delete “${entry.name}”?`))) return;
    setBusy(true);
    try {
      await deleteSharedFile(space.id, entry.path, token);
      await refreshParents(entry.path);
    } catch (cause) {
      reportError(cause instanceof Error ? cause.message : t("删除失败", "Delete failed"));
    } finally {
      setBusy(false);
    }
  }

  async function download(entry: SharedFile) {
    setBusy(true);
    try {
      const blob = await downloadSharedFile(space.id, entry.path, token);
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = entry.name;
      anchor.click();
      URL.revokeObjectURL(url);
    } catch (cause) {
      reportError(cause instanceof Error ? cause.message : t("下载失败", "Download failed"));
    } finally {
      setBusy(false);
    }
  }

  const visibleEntries = (directories[destination] ?? []).filter((entry) => entry.name.toLocaleLowerCase().includes(search.trim().toLocaleLowerCase()));
  const breadcrumb = destination ? destination.split("/") : [];
  return <section className="file-browser">
    <header><div className="file-location"><strong>{t("文件管理器", "File manager")}</strong><small>{t(isAdmin ? "可管理所有所有者的文件" : "仅显示并管理归属于你的文件", isAdmin ? "Manage files from every owner" : "Only your files are visible and manageable")}</small>{transferStatus && <em>{transferStatus}</em>}</div><div>
      <button type="button" className="secondary-button" disabled={busy} onClick={() => void loadDirectory(destination)} aria-label={t("刷新当前文件夹", "Refresh current folder")}>{t("刷新", "Refresh")}</button>
      <button type="button" className="secondary-button" disabled={busy} onClick={() => void createDirectory()}>{t("新建文件夹", "New folder")}</button>
      <button type="button" className="secondary-button" disabled={busy} onClick={() => folderInput.current?.click()}>{t("上传文件夹", "Upload folder")}</button>
      <button type="button" className="primary-button" disabled={busy} onClick={() => uploadInput.current?.click()}>{t("上传文件", "Upload files")}<span>↑</span></button>
      <input ref={uploadInput} hidden aria-label={t("上传文件", "Upload files")} type="file" multiple onChange={(event) => void upload(event, false)} />
      <input ref={folderInput} hidden aria-label={t("上传文件夹", "Upload folder")} type="file" multiple onChange={(event) => void upload(event, true)} />
    </div></header>
    <div className="file-browser-toolbar"><nav aria-label={t("当前文件夹路径", "Current folder path")}><button className={!destination ? "active" : ""} onClick={() => void navigate("")} type="button">{t("根目录", "Root")}</button>{breadcrumb.map((segment, index) => { const path = breadcrumb.slice(0, index + 1).join("/"); return <span key={path}>› <button className={path === destination ? "active" : ""} onClick={() => void navigate(path)} type="button">{segment}</button></span>; })}</nav><label><span aria-hidden="true">⌕</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("筛选当前文件夹", "Filter current folder")} /></label><small>{t(`${visibleEntries.length} 项`, `${visibleEntries.length} items`)}</small></div>
    <div className="file-browser-workspace">
      <aside className="file-directory-tree"><button className={!destination ? "active" : ""} type="button" onClick={() => void navigate("")}><span>⌂</span>{t("根目录", "Root")}</button><DirectoryTree parent="" directories={directories} expanded={expanded} destination={destination} onToggle={toggle} /></aside>
      <div className="file-list-pane"><div className="file-table-head"><span>{t("名称", "Name")}</span><span>{t("所有者", "Owner")}</span><span>{t("修改时间", "Modified")}</span><span>{t("大小", "Size")}</span><span>{t("操作", "Actions")}</span></div><div className="file-tree">
        <FileRows entries={visibleEntries} isAdmin={isAdmin} busy={busy} onOpenDirectory={navigate} onRename={rename} onDelete={remove} onDownload={download} t={t} />
        {visibleEntries.length === 0 && <div className="file-empty">{search ? t("没有匹配的文件", "No matching files") : t("此文件夹中没有可见文件", "No visible files in this folder")}</div>}
      </div></div>
    </div>
  </section>;
}

function DirectoryTree({ parent, directories, expanded, destination, onToggle }: { parent: string; directories: Record<string, SharedFile[]>; expanded: Set<string>; destination: string; onToggle: (entry: SharedFile) => Promise<void> }) {
  return <>{(directories[parent] ?? []).filter((entry) => entry.directory).map((entry) => <div key={entry.path}><button className={destination === entry.path ? "active" : ""} type="button" aria-expanded={expanded.has(entry.path)} onClick={() => void onToggle(entry)}><span>{expanded.has(entry.path) ? "⌄" : "›"}</span>{entry.name}</button>{expanded.has(entry.path) && <div><DirectoryTree parent={entry.path} directories={directories} expanded={expanded} destination={destination} onToggle={onToggle} /></div>}</div>)}</>;
}

function FileRows({ entries, isAdmin, busy, onOpenDirectory, onRename, onDelete, onDownload, t }: {
  entries: SharedFile[]; isAdmin: boolean; busy: boolean; onOpenDirectory: (path: string) => Promise<void>; onRename: (entry: SharedFile) => Promise<void>;
  onDelete: (entry: SharedFile) => Promise<void>; onDownload: (entry: SharedFile) => Promise<void>; t: (zh: string, en: string) => string;
}) {
  return <>{entries.map((entry) => {
    const allowed = isAdmin || entry.ownedByUser;
    return <div key={entry.path} className="file-row">
        <button className="file-name" type="button" onClick={() => entry.directory ? void onOpenDirectory(entry.path) : void onDownload(entry)}>
          <span>{entry.directory ? "›" : "·"}</span><i aria-hidden="true">{entry.directory ? "◇" : "—"}</i><strong>{entry.name}</strong>
        </button>
        <span>{entry.owner || "—"}</span><span>{new Date(entry.modifiedAt).toLocaleString()}</span><span>{entry.directory ? "—" : formatBytes(entry.sizeBytes)}</span>
        <div className="file-actions">
          {!entry.directory && allowed && <button type="button" disabled={busy} onClick={() => void onDownload(entry)}>{t("下载", "Download")}</button>}
          {allowed && <button type="button" disabled={busy} onClick={() => void onRename(entry)}>{t("重命名", "Rename")}</button>}
          {allowed && <button className="danger-button" type="button" disabled={busy} onClick={() => void onDelete(entry)}>{t("删除", "Delete")}</button>}
        </div>
    </div>;
  })}</>;
}

function targetLabel(space: SharedSpace, t: (zh: string, en: string) => string) {
  return space.target === "all" ? t("全部节点（动态）", "All nodes (dynamic)") : space.target === "group" ? `${t("动态分组", "Dynamic group")}：${space.targetGroup}` : t(`${space.targetNodeIds?.length ?? 0} 个指定节点`, `${space.targetNodeIds?.length ?? 0} selected nodes`);
}
function joinPath(parent: string, name: string) { return `${parent ? `${parent}/` : ""}${name.replace(/^\/+/, "")}`; }
function normalizeUploadPath(path: string) {
  const normalized = path.replaceAll("\\", "/").replace(/^\/+|\/+$/g, "");
  const segments = normalized.split("/");
  if (!normalized || segments.some((segment) => !segment || segment === "." || segment === "..")) {
    throw new Error("Invalid upload path");
  }
  return segments.join("/");
}
function roles(node: ClusterNode) { return node.roles?.length ? node.roles : ["compute"]; }
function nodeName(nodes: ClusterNode[], id: string) { return nodes.find((node) => node.id === id)?.name ?? id; }
