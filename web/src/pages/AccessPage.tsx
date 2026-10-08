import { FormEvent, useCallback, useEffect, useState } from "react";
import { createAPIToken, createBackup, createUserGroup, deleteUserGroup, getAccessState, getBackups, revokeAPIToken, updateAccessSettings, updateLocalUser, updateUserGroup, verifyBackup } from "../api/client";
import type { AccessPrincipal, AccessRole, APIToken, BackupManifest, LocalUser, UserGroup } from "../api/contracts";
import { relativeTime } from "../lib/format";
import { usePreferences } from "../app/preferences";

export function AccessPage({ token }: { token: string }) {
  const { t } = usePreferences();
  const [principal, setPrincipal] = useState<AccessPrincipal | null>(null);
  const [roles, setRoles] = useState<AccessRole[]>([]);
  const [tokens, setTokens] = useState<APIToken[]>([]);
  const [backups, setBackups] = useState<BackupManifest[]>([]);
  const [users, setUsers] = useState<LocalUser[]>([]);
  const [groups, setGroups] = useState<UserGroup[]>([]);
  const [membershipDrafts, setMembershipDrafts] = useState<Record<string, string[]>>({});
  const [defaultGroupID, setDefaultGroupID] = useState("");
  const [savedDefaultGroupID, setSavedDefaultGroupID] = useState("");
  const [groupForm, setGroupForm] = useState({ name: "", cpu: "16", memory: "64", storage: "0", concurrent: "8", timeoutDays: "7" });
  const [groupDrafts, setGroupDrafts] = useState<Record<string, GroupDraft>>({});
  const [form, setForm] = useState({ name: "", role: "viewer", days: "30" });
  const [secret, setSecret] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const refresh = useCallback(async (signal?: AbortSignal) => {
    const [state, nextBackups] = await Promise.all([getAccessState(token, signal), getBackups(token, signal)]);
    setPrincipal(state.principal); setRoles(state.roles); setTokens(state.tokens); setUsers(state.users); setGroups(state.groups);
    setMembershipDrafts(Object.fromEntries(state.users.map((user) => [user.id, [...(user.groupIds ?? [])].sort()])));
    setDefaultGroupID(state.settings.defaultUserGroupId ?? ""); setSavedDefaultGroupID(state.settings.defaultUserGroupId ?? "");
    setGroupDrafts(Object.fromEntries(state.groups.map((group) => [group.id, draftFromGroup(group)])));
    setBackups(nextBackups);
  }, [token]);
  useEffect(() => {
    const controller = new AbortController();
    refresh(controller.signal).catch((cause) => !controller.signal.aborted && setError(cause instanceof Error ? cause.message : "Unable to load access control"));
    return () => controller.abort();
  }, [refresh]);
  async function create(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try {
      const created = await createAPIToken({ name: form.name, role: form.role, ttlSeconds: Number(form.days) * 86400 }, token);
      setSecret(created.secret); setForm({ ...form, name: "" }); await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Token creation failed"); }
    finally { setBusy(false); }
  }
  async function updateUser(id: string, payload: { status?: string; role?: string; groupIds?: string[] }) {
    setBusy(true); setError("");
    try { await updateLocalUser(id, payload, token); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "User update failed"); }
    finally { setBusy(false); }
  }
  async function createGroup(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try {
      await createUserGroup({
        name: groupForm.name,
        resourceQuota: { milliCpu: Number(groupForm.cpu) * 1000, memoryBytes: Number(groupForm.memory) * 2 ** 30, ephemeralStorageBytes: 0 },
        storageQuotaBytes: Number(groupForm.storage) * 2 ** 30,
        maxConcurrentJobs: Number(groupForm.concurrent),
        maxJobTimeoutSeconds: Number(groupForm.timeoutDays) * 86400,
        disabled: false,
        disableSuspendQueued: false,
        disableEndSessions: false,
      }, token);
      setGroupForm({ ...groupForm, name: "" }); await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Group creation failed"); }
    finally { setBusy(false); }
  }
  async function saveGroup(group: UserGroup, disabled = group.disabled ?? false) {
    const draft = groupDrafts[group.id] ?? draftFromGroup(group);
    const values = [draft.cpu, draft.memory, draft.storage, draft.concurrent, draft.timeoutDays].map(Number);
    if (values.some((value) => !Number.isFinite(value) || value < 0) || Number(draft.timeoutDays) <= 0) {
      setError(t("限额必须为非负数，最长运行时间必须大于 0 天", "Limits must be non-negative and maximum runtime must be greater than 0 days")); return;
    }
    setBusy(true); setError("");
    try {
      await updateUserGroup(group.id, {
        name: draft.name,
        resourceQuota: { milliCpu: Number(draft.cpu) * 1000, memoryBytes: Number(draft.memory) * 2 ** 30, ephemeralStorageBytes: 0 },
        storageQuotaBytes: Number(draft.storage) * 2 ** 30,
        maxConcurrentJobs: Number(draft.concurrent),
        maxJobTimeoutSeconds: Math.round(Number(draft.timeoutDays) * 86400),
        disabled,
        disableSuspendQueued: draft.suspendQueued,
        disableEndSessions: draft.endSessions,
      }, token);
      await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Group update failed"); }
    finally { setBusy(false); }
  }
  async function removeGroup(group: UserGroup) {
    const memberCount = users.filter((user) => user.groupIds?.includes(group.id)).length;
    if (!window.confirm(t(
      `移除用户组“${group.name}”？系统会同时解除 ${memberCount} 个账户与该组的关联；没有其他组的账户将采用默认无限额策略。`,
      `Remove group “${group.name}”? It will be detached from ${memberCount} account(s). Accounts with no other group will use the default unlimited policy.`,
    ))) return;
    setBusy(true); setError("");
    try { await deleteUserGroup(group.id, token); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Group removal failed"); }
    finally { setBusy(false); }
  }
  function toggleMembership(user: LocalUser, groupID: string) {
    setMembershipDrafts((current) => {
      const selected = new Set(current[user.id] ?? user.groupIds ?? []);
      if (selected.has(groupID)) selected.delete(groupID); else selected.add(groupID);
      return { ...current, [user.id]: [...selected].sort() };
    });
  }
  async function saveDefaultGroup() {
    setBusy(true); setError("");
    try {
      const settings = await updateAccessSettings(defaultGroupID, token);
      setDefaultGroupID(settings.defaultUserGroupId ?? ""); setSavedDefaultGroupID(settings.defaultUserGroupId ?? "");
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Access settings update failed"); }
    finally { setBusy(false); }
  }
  async function revoke(id: string) {
    setBusy(true); setError("");
    try { await revokeAPIToken(id, token); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Token revocation failed"); }
    finally { setBusy(false); }
  }
  async function backup(action: "create" | "verify", id?: string) {
    setBusy(true); setError("");
    try {
      if (action === "create") await createBackup(token);
      else if (id) await verifyBackup(id, token);
      await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Backup operation failed"); }
    finally { setBusy(false); }
  }
  return <section>
    <header className="page-header"><div><h1>{t("访问控制", "Access control")}</h1></div></header>
    {error && <p className="error-banner">{error}</p>}
    <div className="access-identity"><div><span className="eyebrow">{t("当前登录身份", "Current identity")}</span><strong>{principal?.displayName || principal?.subject || "—"}</strong><small>{principal?.authMethod} · {principal?.role}</small></div>{roles.map((role) => <article key={role.name}><span>{role.name}</span><p>{role.description}</p><small>{role.permissions.join(" · ")}</small></article>)}</div>
    {secret && <div className="bootstrap-panel"><header><div><span className="eyebrow">{t("请立即保存，仅显示一次", "Save now; displayed once")}</span><h2>{t("保存新的 API Token", "Save the new API token")}</h2></div><button onClick={() => setSecret("")}>{t("已保存", "Saved")}</button></header><code>{secret}</code></div>}
    <div className="backup-console local-account-console"><header><div><h2>{t("XIRANG 账户", "XIRANG accounts")}</h2></div><span>{t(`${users.filter((user) => user.status === "pending").length} 待审批`, `${users.filter((user) => user.status === "pending").length} pending`)}</span></header><p>{t("选择用户组后保存；多个用户组采用最严格的非零限额。", "Select groups and save. The most restrictive non-zero limit applies.")}</p><div className="local-account-list">{users.map((user) => { const selected = membershipDrafts[user.id] ?? user.groupIds ?? []; const saved = [...(user.groupIds ?? [])].sort(); const changed = selected.join("\u0000") !== saved.join("\u0000"); return <article key={user.id} className="local-account-row"><div className="local-account-summary"><strong>{user.displayName || user.username}</strong><small>@{user.username} · {user.status} · {user.role}</small></div><fieldset className="group-membership-picker" disabled={busy}><legend>{t("用户组", "User groups")}</legend>{groups.length > 0 ? <div className="group-membership-options">{groups.map((group) => { const checked = selected.includes(group.id); return <label key={group.id} className={`${checked ? "selected" : ""}${group.disabled ? " disabled" : ""}`}><input type="checkbox" checked={checked} onChange={() => toggleMembership(user, group.id)} /><span><strong>{group.name}</strong><small>{group.disabled ? t("已停用", "Disabled") : t("可用", "Available")}</small></span><i aria-hidden="true">{checked ? "✓" : "+"}</i></label>; })}</div> : <span className="membership-empty">{t("尚未创建用户组", "No user groups created")}</span>}<small className="membership-summary">{selected.length > 0 ? t(`已选择 ${selected.length} 个组`, `${selected.length} selected`) : t("未选择组 · 使用默认无限额策略", "No group selected · default unlimited policy")}</small></fieldset><div className="account-actions"><button className="primary-button membership-save" onClick={() => updateUser(user.id, { groupIds: selected })} disabled={busy || !changed}>{changed ? t("保存用户组", "Save groups") : t("用户组已保存", "Groups saved")}</button><div className="inline-actions">{user.status === "pending" && <button onClick={() => updateUser(user.id, { status: "active" })} disabled={busy}>{t("批准", "Approve")}</button>}{user.status === "active" && <button className="danger-button" onClick={() => updateUser(user.id, { status: "disabled" })} disabled={busy}>{t("禁用", "Disable")}</button>}{user.status !== "active" && user.status !== "pending" && <button onClick={() => updateUser(user.id, { status: "active" })} disabled={busy}>{t("启用", "Enable")}</button>}<button className="danger-button" onClick={() => updateUser(user.id, { role: user.role === "admin" ? "user" : "admin" })} disabled={busy}>{user.role === "admin" ? t("取消管理员", "Remove administrator") : t("设为管理员", "Make administrator")}</button></div></div></article>; })}</div></div>
    <div className="group-policy-console"><header><div><h2>{t("用户组管理", "User group management")}</h2></div><span className="policy-badge">{t(`${groups.length} 个用户组`, `${groups.length} groups`)}</span></header><p>{t("0 表示 CPU、内存、存储或并发数量不限制；多组用户采用最严格的非零限额。", "Zero means unlimited CPU, memory, storage, or concurrency. Multi-group users receive the most restrictive non-zero limits.")}</p><div className="group-default-policy"><label><span>{t("新用户默认用户组", "Default group for new accounts")}</span><select aria-label={t("新用户默认用户组", "Default group for new accounts")} value={defaultGroupID} onChange={(event) => setDefaultGroupID(event.target.value)} disabled={busy}><option value="">{t("不设置默认用户组", "Do not assign a default group")}</option>{groups.map((group) => <option key={group.id} value={group.id}>{group.name}{group.disabled ? t("（已停用）", " (disabled)") : ""}</option>)}</select></label><div><small>{t("只影响之后注册的 XIRANG 账户，不修改已有账户。", "Applies only to future XIRANG registrations and never changes existing accounts.")}</small><button type="button" className="primary-button" onClick={() => void saveDefaultGroup()} disabled={busy || defaultGroupID === savedDefaultGroupID}>{defaultGroupID === savedDefaultGroupID ? t("默认组已保存", "Default saved") : t("保存默认组", "Save default")}</button></div></div><div className="group-policy-grid">{groups.map((group) => { const draft = groupDrafts[group.id] ?? draftFromGroup(group); const update = (key: keyof GroupDraft, value: string | boolean) => setGroupDrafts((current) => ({ ...current, [group.id]: { ...draft, [key]: value } })); return <article key={group.id} className={group.disabled ? "disabled" : ""}><header><strong>{group.name}</strong><small>{group.disabled ? t("已停用", "Disabled") : t("运行中", "Active")}</small></header><div className="group-quota-fields"><label><span>{t("名称", "Name")}</span><input value={draft.name} onChange={(event) => update("name", event.target.value)} /></label><label><span>{t("CPU 核心（0 不限）", "CPU cores (0 unlimited)")}</span><input type="number" min="0" value={draft.cpu} onChange={(event) => update("cpu", event.target.value)} /></label><label><span>{t("内存 G（0 不限）", "Memory G (0 unlimited)")}</span><input type="number" min="0" value={draft.memory} onChange={(event) => update("memory", event.target.value)} /></label><label><span>{t("存储 G（0 不限）", "Storage G (0 unlimited)")}</span><input type="number" min="0" value={draft.storage} onChange={(event) => update("storage", event.target.value)} /></label><label><span>{t("并发任务（0 不限）", "Concurrent jobs (0 unlimited)")}</span><input type="number" min="0" value={draft.concurrent} onChange={(event) => update("concurrent", event.target.value)} /></label><label><span>{t("最长运行时间（天）", "Maximum runtime (days)")}</span><input type="number" min="0.01" max="100000" step="0.01" value={draft.timeoutDays} onChange={(event) => update("timeoutDays", event.target.value)} /></label></div><div className="group-disable-options"><label><input type="checkbox" checked={draft.suspendQueued} onChange={(event) => update("suspendQueued", event.target.checked)} /><span>{t("停用时挂起排队任务", "Suspend queued jobs when disabled")}</span></label><label><input type="checkbox" checked={draft.endSessions} onChange={(event) => update("endSessions", event.target.checked)} /><span>{t("停用时终止登录与终端会话", "End login and terminal sessions when disabled")}</span></label></div><footer><button type="button" onClick={() => void saveGroup(group)} disabled={busy}>{t("保存修改", "Save changes")}</button><button type="button" className={group.disabled ? "secondary-button" : "danger-button"} onClick={() => void saveGroup(group, !group.disabled)} disabled={busy}>{group.disabled ? t("重新启用", "Enable") : t("暂时停用", "Disable")}</button><button type="button" className="danger-button" onClick={() => void removeGroup(group)} disabled={busy}>{t("移除", "Remove")}</button></footer></article>; })}</div></div>
    <form className="queue-editor group-create-form" onSubmit={createGroup}><h2>{t("创建用户组", "Create user group")}</h2><label><span>{t("名称", "Name")}</span><input required value={groupForm.name} onChange={(event) => setGroupForm({ ...groupForm, name: event.target.value })} /></label><label><span>{t("逻辑 CPU 核心数（0 为不限）", "Logical CPU cores (0 for unlimited)")}</span><input type="number" min="0" value={groupForm.cpu} onChange={(event) => setGroupForm({ ...groupForm, cpu: event.target.value })} /></label><label><span>{t("内存 (G，0 为不限)", "Memory (G, 0 for unlimited)")}</span><input type="number" min="0" value={groupForm.memory} onChange={(event) => setGroupForm({ ...groupForm, memory: event.target.value })} /></label><label><span>{t("存储配额 (G，0 为不限)", "Storage quota (G, 0 for unlimited)")}</span><input type="number" min="0" value={groupForm.storage} onChange={(event) => setGroupForm({ ...groupForm, storage: event.target.value })} /></label><label><span>{t("并发任务（0 为不限）", "Concurrent jobs (0 for unlimited)")}</span><input type="number" min="0" value={groupForm.concurrent} onChange={(event) => setGroupForm({ ...groupForm, concurrent: event.target.value })} /></label><label><span>{t("最长运行时间（天）", "Maximum runtime (days)")}</span><input type="number" min="0.01" max="100000" step="0.01" value={groupForm.timeoutDays} onChange={(event) => setGroupForm({ ...groupForm, timeoutDays: event.target.value })} /></label><button className="primary-button" disabled={busy}>{t("创建配额组", "Create quota group")}<span aria-hidden="true">→</span></button></form>
    <div className={`access-layout${tokens.length ? "" : " access-layout--empty"}`}>
      <div className="token-list">{tokens.map((item) => <article key={item.id}><header><div><span className={`status-dot ${item.revokedAt ? "" : "active"}`} /><div><strong>{item.name}</strong><small>{item.id}</small></div></div><em>{item.role}</em></header><p>{t(`创建于 ${relativeTime(item.createdAt)} · 到期 ${new Date(item.expiresAt).toLocaleDateString()}${item.lastUsedAt ? ` · 最近使用 ${relativeTime(item.lastUsedAt)}` : ""}`, `Created ${relativeTime(item.createdAt)} · expires ${new Date(item.expiresAt).toLocaleDateString()}${item.lastUsedAt ? ` · last used ${relativeTime(item.lastUsedAt)}` : ""}`)}</p>{item.revokedAt ? <span className="token-revoked">REVOKED</span> : <button className="danger-button" onClick={() => revoke(item.id)} disabled={busy}>{t("撤销", "Revoke")}</button>}</article>)}</div>
      <form className="queue-editor" onSubmit={create}><h2>{t("创建 API Token", "Create API token")}</h2><p>{t("密钥仅显示一次，请妥善保存。", "The secret is shown once. Store it securely.")}</p><label><span>{t("名称", "Name")}</span><input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} required /></label><label><span>{t("角色", "Role")}</span><select value={form.role} onChange={(event) => setForm({ ...form, role: event.target.value })}>{roles.map((role) => <option key={role.name}>{role.name}</option>)}</select></label><label><span>{t("有效天数", "Validity (days)")}</span><input type="number" min="1" max="365" value={form.days} onChange={(event) => setForm({ ...form, days: event.target.value })} /></label><button className="primary-button" disabled={busy}>{t("生成一次性密钥", "Generate one-time secret")}<span aria-hidden="true">→</span></button></form>
    </div>
    <div className="backup-console"><header><div><h2>{t("在线备份与离线恢复", "Online backup and offline restore")}</h2></div><button className="primary-button" onClick={() => backup("create")} disabled={busy}>{t("创建恢复点", "Create restore point")}<span aria-hidden="true">+</span></button></header>{backups.map((item) => <article key={item.id}><div><strong>{item.id}</strong><small>{relativeTime(item.createdAt)} · schema {item.schemaVersion}</small></div><span>{item.files.length} databases</span><button onClick={() => backup("verify", item.id)} disabled={busy}>{t("校验备份", "Verify backup")}</button></article>)}<p>{t("恢复必须在所有控制节点停止后使用", "Restore only after stopping every controller with")} <code>xirang-maint restore</code>{t("，工具会拒绝覆盖仍被锁定的数据库。", "; the tool refuses to overwrite a locked database.")}</p></div>
  </section>;
}

type GroupDraft = { name: string; cpu: string; memory: string; storage: string; concurrent: string; timeoutDays: string; suspendQueued: boolean; endSessions: boolean };
function draftFromGroup(group: UserGroup): GroupDraft {
  return { name: group.name, cpu: String(group.resourceQuota.milliCpu / 1000), memory: String(group.resourceQuota.memoryBytes / 2 ** 30), storage: String(group.storageQuotaBytes / 2 ** 30), concurrent: String(group.maxConcurrentJobs), timeoutDays: String(group.maxJobTimeoutSeconds / 86400), suspendQueued: group.disableSuspendQueued ?? false, endSessions: group.disableEndSessions ?? false };
}
