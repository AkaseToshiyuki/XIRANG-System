import { useEffect, useId, useRef, useState } from "react";
import { getSharedFiles, getSharedSpaces } from "../api/client";
import type { SharedFile, SharedSpace } from "../api/contracts";
import { usePreferences } from "../app/preferences";

export function SharedPathPicker({ token, initialSpaceID, initialPath, onSelect, onClose }: {
  token: string; initialSpaceID: string; initialPath: string;
  onSelect: (spaceID: string, path: string) => void; onClose: () => void;
}) {
  const { t } = usePreferences();
  const dialogRef = useRef<HTMLDialogElement>(null);
  const titleID = useId();
  const parentRef = useRef<HTMLButtonElement>(null);
  const spaceRef = useRef<HTMLSelectElement>(null);
  const previousPath = useRef(initialPath);
  const [spaces, setSpaces] = useState<SharedSpace[]>([]);
  const [spaceID, setSpaceID] = useState(initialSpaceID);
  const [path, setPath] = useState(initialPath);
  const [entries, setEntries] = useState<SharedFile[]>([]);
  const [spacesError, setSpacesError] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    const dialog = dialogRef.current!;
    const trigger = document.activeElement;
    dialog.showModal();
    return () => {
      dialog.close();
      if (trigger instanceof HTMLElement && trigger.isConnected) trigger.focus();
    };
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    getSharedSpaces(token, controller.signal).then((items) => {
      if (controller.signal.aborted) return;
      const active = items.filter((item) => item.status === "active" || item.status === "degraded");
      setSpaces(active);
      setSpaceID((current) => current || active[0]?.id || "");
      setSpacesError("");
    }).catch((cause) => {
      if (!controller.signal.aborted) setSpacesError(cause instanceof Error ? cause.message : t("无法读取共享空间", "Unable to load shared spaces"));
    });
    return () => controller.abort();
  }, [t, token]);

  useEffect(() => {
    const controller = new AbortController();
    setEntries([]);
    setError("");
    setLoading(Boolean(spaceID));
    if (spaceID) getSharedFiles(spaceID, path, token, controller.signal).then((items) => {
      if (!controller.signal.aborted) setEntries(items.filter((entry) => entry.directory));
    }).catch((cause) => {
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : t("无法读取文件夹", "Unable to load folders"));
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [path, spaceID, t, token]);

  useEffect(() => {
    if (previousPath.current === path) return;
    previousPath.current = path;
    (path ? parentRef.current : spaceRef.current)?.focus();
  }, [path]);

  const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
  return <dialog ref={dialogRef} className="path-picker" aria-labelledby={titleID} aria-modal="true"
    onCancel={(event) => { event.preventDefault(); onClose(); }}
    onMouseDown={(event) => {
      if (event.target !== event.currentTarget) return;
      const box = event.currentTarget.getBoundingClientRect();
      if (event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom) { event.preventDefault(); onClose(); }
    }}
    onKeyDown={(event) => {
      if (event.key !== "Tab") return;
      const controls = Array.from(event.currentTarget.querySelectorAll<HTMLElement>("button:not(:disabled), select:not(:disabled)"));
      const first = controls[0], last = controls[controls.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
    }}>
    <header><div><span className="eyebrow">{t("共享空间", "Shared storage")}</span><h2 id={titleID}>{t("选择文件夹", "Select folder")}</h2></div><button type="button" aria-label={t("关闭文件夹选择器", "Close folder picker")} onClick={onClose}>×</button></header>
    {(spacesError || error) && <p className="error-banner" role="alert">{spacesError || error}</p>}
    <label><span>{t("共享空间", "Shared space")}</span><select ref={spaceRef} value={spaceID} onChange={(event) => { setSpaceID(event.target.value); setPath(""); }}><option value="">{t("选择共享空间", "Select a shared space")}</option>{spaces.map((space) => <option key={space.id} value={space.id}>{space.name} · {space.mountPath}</option>)}</select></label>
    <nav aria-label={t("文件夹路径", "Folder path")}><button ref={parentRef} type="button" disabled={!path} onClick={() => setPath(parent)}>↑ {t("上一级", "Parent")}</button><code>/{path}</code></nav>
    <div className="path-picker-folders" aria-busy={loading}>
      {entries.map((entry) => <button type="button" key={entry.path} onClick={() => setPath(entry.path)}><span aria-hidden="true">◇</span><strong>{entry.name}</strong></button>)}
      <p role="status">{loading ? t("正在读取文件夹…", "Loading folders…") : !error && spaceID && entries.length === 0 ? t("此目录没有子文件夹", "This folder has no subfolders") : ""}</p>
    </div>
    <footer><button type="button" className="secondary-button" onClick={onClose}>{t("取消", "Cancel")}</button><button type="button" className="primary-button" disabled={loading || Boolean(spacesError || error) || !spaces.some((space) => space.id === spaceID)} onClick={() => onSelect(spaceID, path)}>{t("选择当前文件夹", "Select current folder")}<span aria-hidden="true">→</span></button></footer>
  </dialog>;
}
