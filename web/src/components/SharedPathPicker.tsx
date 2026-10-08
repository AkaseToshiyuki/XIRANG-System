import { useCallback, useEffect, useState } from "react";
import { getSharedFiles, getSharedSpaces } from "../api/client";
import type { SharedFile, SharedSpace } from "../api/contracts";
import { usePreferences } from "../app/preferences";

export function SharedPathPicker({ token, initialSpaceID, initialPath, onSelect, onClose }: {
  token: string; initialSpaceID: string; initialPath: string;
  onSelect: (spaceID: string, path: string) => void; onClose: () => void;
}) {
  const { t } = usePreferences();
  const [spaces, setSpaces] = useState<SharedSpace[]>([]);
  const [spaceID, setSpaceID] = useState(initialSpaceID);
  const [path, setPath] = useState(initialPath);
  const [entries, setEntries] = useState<SharedFile[]>([]);
  const [error, setError] = useState("");
  useEffect(() => { getSharedSpaces(token).then((items) => { const active = items.filter((item) => item.status === "active" || item.status === "degraded"); setSpaces(active); setSpaceID((current) => current || active[0]?.id || ""); }).catch((cause) => setError(cause instanceof Error ? cause.message : t("无法读取共享空间", "Unable to load shared spaces"))); }, [t, token]);
  const load = useCallback(async () => { if (!spaceID) { setEntries([]); return; } try { setEntries((await getSharedFiles(spaceID, path, token)).filter((entry) => entry.directory)); setError(""); } catch (cause) { setError(cause instanceof Error ? cause.message : t("无法读取文件夹", "Unable to load folders")); } }, [path, spaceID, t, token]);
  useEffect(() => { void load(); }, [load]);
  const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
  return <div className="path-picker-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}><section className="path-picker" role="dialog" aria-modal="true" aria-labelledby="path-picker-title"><header><div><span className="eyebrow">{t("共享空间", "Shared storage")}</span><h2 id="path-picker-title">{t("选择文件夹", "Select folder")}</h2></div><button type="button" onClick={onClose}>×</button></header>{error && <p className="error-banner">{error}</p>}<label><span>{t("共享空间", "Shared space")}</span><select value={spaceID} onChange={(event) => { setSpaceID(event.target.value); setPath(""); }}><option value="">{t("选择共享空间", "Select a shared space")}</option>{spaces.map((space) => <option key={space.id} value={space.id}>{space.name} · {space.mountPath}</option>)}</select></label><nav><button type="button" disabled={!path} onClick={() => setPath(parent)}>↑ {t("上一级", "Parent")}</button><code>/{path}</code></nav><div className="path-picker-folders">{entries.map((entry) => <button type="button" key={entry.path} onDoubleClick={() => setPath(entry.path)} onClick={() => setPath(entry.path)}><span>◇</span><strong>{entry.name}</strong></button>)}{spaceID && entries.length === 0 && <p>{t("此目录没有子文件夹", "This folder has no subfolders")}</p>}</div><footer><button type="button" className="secondary-button" onClick={onClose}>{t("取消", "Cancel")}</button><button type="button" className="primary-button" disabled={!spaceID} onClick={() => onSelect(spaceID, path)}>{t("选择当前文件夹", "Select current folder")}<span aria-hidden="true">→</span></button></footer></section></div>;
}
