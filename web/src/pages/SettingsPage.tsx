import { FormEvent, useEffect, useRef, useState } from "react";
import { getClusterBranding, updateClusterBranding } from "../api/client";
import type { ClusterBranding } from "../api/contracts";
import { usePreferences } from "../app/preferences";

export const defaultClusterBranding: ClusterBranding = { name: "XIRANG", logoDataUrl: "", revision: 0 };
export const defaultClusterLogo = "/brand/xirang-mark.png";

export function SettingsPage({ token, onSaved }: { token: string; onSaved: (value: ClusterBranding) => void }) {
  const { t } = usePreferences();
  const [draft, setDraft] = useState<ClusterBranding>(defaultClusterBranding);
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const fileInput = useRef<HTMLInputElement>(null);

  async function reload() {
    setBusy(true); setError(""); setNotice("");
    try { setDraft(await getClusterBranding()); setLoaded(true); }
    catch (cause) { setError(cause instanceof Error ? cause.message : t("无法读取集群设置", "Unable to load cluster settings")); }
    finally { setBusy(false); }
  }
  useEffect(() => { void reload(); }, []);

  async function chooseLogo(file?: File) {
    if (!file) return;
    setError(""); setNotice("");
    if (!["image/png", "image/jpeg"].includes(file.type) || file.size > 512 * 1024) {
      setError(t("请选择不超过 512 KiB 的 PNG 或 JPEG 图片。", "Choose a PNG or JPEG image up to 512 KiB."));
      if (fileInput.current) fileInput.current.value = "";
      return;
    }
    setBusy(true);
    try {
      const dataUrl = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result)); reader.onerror = () => reject(new Error(t("无法读取图片", "Unable to read image")));
        reader.readAsDataURL(file);
      });
      const image = new Image();
      image.src = dataUrl;
      await image.decode();
      if (image.naturalWidth > 2048 || image.naturalHeight > 2048) throw new Error(t("图片宽度和高度均不能超过 2048 像素。", "Image width and height must not exceed 2048 pixels."));
      setDraft((current) => ({ ...current, logoDataUrl: dataUrl }));
    } catch (cause) { setError(cause instanceof Error ? cause.message : t("图片无效", "Invalid image")); }
    finally { setBusy(false); if (fileInput.current) fileInput.current.value = ""; }
  }

  async function save(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError(""); setNotice("");
    try {
      const saved = await updateClusterBranding(draft, token);
      setDraft(saved); onSaved(saved);
      setNotice(t("集群设置已保存，登录页和侧栏已更新。", "Cluster settings saved. The sign-in page and sidebar are updated."));
    } catch (cause) { setError(cause instanceof Error ? cause.message : t("保存失败", "Unable to save settings")); }
    finally { setBusy(false); }
  }

  return <section>
    <header className="page-header"><div><span className="eyebrow">{t("管理员", "Administrator")}</span><h1>{t("集群设置", "Cluster settings")}</h1><p>{t("设置所有用户看到的集群名称和 Logo。", "Choose the cluster name and logo shown to all users.")}</p></div></header>
    <div className="branding-layout">
      <form className="panel branding-form" onSubmit={save}>
        <h2>{t("名称与标识", "Name and identity")}</h2>
        <label className="job-field"><span>{t("集群名称", "Cluster name")}</span><input value={draft.name} maxLength={64} required disabled={busy || !loaded} onChange={(event) => { setDraft({ ...draft, name: event.target.value }); setNotice(""); }} /></label>
        <label className="job-field"><span>{t("集群 Logo", "Cluster logo")}</span><input ref={fileInput} type="file" accept="image/png,image/jpeg" disabled={busy || !loaded} onChange={(event) => void chooseLogo(event.target.files?.[0])} /></label>
        <p className="branding-help">{t("PNG 或 JPEG，最大 512 KiB，尺寸不超过 2048 × 2048 像素。建议使用透明背景的 PNG。", "PNG or JPEG, up to 512 KiB and 2048 × 2048 pixels. A transparent PNG is recommended.")}</p>
        <div className="branding-actions"><button className="text-button" type="button" disabled={busy || !loaded || !draft.logoDataUrl} onClick={() => { setDraft({ ...draft, logoDataUrl: "" }); setNotice(""); }}>{t("恢复默认 Logo", "Restore default logo")}</button><button className="text-button" type="button" disabled={busy || !loaded} onClick={() => { setDraft({ ...draft, name: "XIRANG", logoDataUrl: "" }); setNotice(""); }}>{t("恢复默认名称和 Logo", "Restore default name and logo")}</button></div>
        {error && <p className="form-error" role="alert">{error}</p>}
        {notice && <p className="form-notice" role="status">{notice}</p>}
        <div className="branding-actions"><button className="primary-button" type="submit" disabled={busy || !loaded || !draft.name.trim()}>{busy ? t("处理中…", "Working…") : t("保存设置", "Save settings")}</button><button className="text-button" type="button" disabled={busy} onClick={() => void reload()}>{t("重新载入", "Reload")}</button></div>
      </form>
      <aside className="panel branding-preview"><h2>{t("显示预览", "Display preview")}</h2><div className="branding-preview-identity"><img src={draft.logoDataUrl || defaultClusterLogo} alt={t("集群 Logo 预览", "Cluster logo preview")} /><strong>{draft.name.trim() || "XIRANG"}</strong></div><p className="branding-help">{t("保存后生效。名称和 Logo 也会显示在公开的登录页。", "Changes take effect after saving. The name and logo also appear on the public sign-in page.")}</p></aside>
    </div>
  </section>;
}
