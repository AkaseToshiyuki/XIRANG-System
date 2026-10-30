import { FormEvent, ReactNode, useEffect, useState } from "react";
import { getClusterBranding, getNodes, getPrincipal, getSetupStatus, getTelemetryInterval, getVersion, loginLocalUser, registerLocalUser, setupAdmin, updateTelemetryInterval } from "../api/client";
import type { ClusterBranding, AccessPrincipal, VersionInfo } from "../api/contracts";
import { OverviewPage } from "../pages/OverviewPage";
import { AlertsPage } from "../pages/AlertsPage";
import { NodesPage } from "../pages/NodesPage";
import { TopologyPage } from "../pages/TopologyPage";
import { JobsPage } from "../pages/JobsPage";
import { QueuesPage } from "../pages/QueuesPage";
import { EnrollmentPage } from "../pages/EnrollmentPage";
import { BuildsPage } from "../pages/BuildsPage";
import { AccessPage } from "../pages/AccessPage";
import { StoragePage } from "../pages/StoragePage";
import { TerminalPage } from "../pages/TerminalPage";
import { AccountingPage } from "../pages/AccountingPage";
import { PageErrorBoundary } from "../components/PageErrorBoundary";
import { WorkspaceIcon } from "../components/WorkspaceIcon";
import { usePreferences } from "./preferences";

import { HealthPage } from "../pages/HealthPage";
import { SettingsPage, defaultClusterBranding, defaultClusterLogo } from "../pages/SettingsPage";

const navigation = [
  { label: "Overview", en: "Cluster overview", zh: "集群概览", icon: "overview", path: "/", available: true },
  { label: "Topology", en: "Network topology", zh: "网络拓扑", icon: "topology", path: "/topology", available: true },
  { label: "Alerts", en: "Alert center", zh: "告警中心", icon: "alerts", path: "/alerts", available: true },
  { label: "Health", en: "System health", zh: "系统健康", icon: "health", path: "/health", available: true },
  { label: "Nodes", en: "Node status", zh: "节点状态", icon: "nodes", path: "/nodes", available: true },
  { label: "Enrollment", en: "Node enrollment", zh: "节点接入", icon: "enrollment", path: "/enrollment", available: true },
  { label: "Jobs", en: "Job scheduling", zh: "作业调度", icon: "jobs", path: "/jobs", available: true },
  { label: "Queues", en: "Queue policies", zh: "队列策略", icon: "queues", path: "/queues", available: true },
  { label: "Builds", en: "Native builds", zh: "原生构建", icon: "builds", path: "/builds", available: true },
  { label: "Storage", en: "Shared storage", zh: "共享存储", icon: "storage", path: "/storage", available: true },
  { label: "Accounting", en: "Usage accounting", zh: "用量核算", icon: "accounting", path: "/accounting", available: true },
  { label: "Terminal", en: "Terminal access", zh: "终端连接", icon: "terminal", path: "/terminal", available: true },
  { label: "Settings", en: "Cluster settings", zh: "集群设置", icon: "settings", path: "/settings", available: true },
  { label: "Access", en: "Identity management", zh: "身份管理", icon: "access", path: "/access", available: true },
] as const;

type Page = typeof navigation[number]["label"];

export function App() {
  const { language, refreshSeconds, setRefreshSeconds, t } = usePreferences();
  const [active, setActive] = useState<Page>(() => pageFromPath(window.location.pathname));
  const [branding, setBranding] = useState<ClusterBranding>(defaultClusterBranding);
  const [version, setVersion] = useState<VersionInfo | null>(null);
  const [principal, setPrincipal] = useState<AccessPrincipal | null>(null);
  const [token, setToken] = useState("");
  const [tokenInput, setTokenInput] = useState("");
  const [authError, setAuthError] = useState("");
  const [authenticating, setAuthenticating] = useState(false);
  const [setupRequired, setSetupRequired] = useState(false);
  const [authMode, setAuthMode] = useState<"login" | "register" | "token">("login");
  const [credentials, setCredentials] = useState({ username: "", displayName: "", password: "" });
  const [authNotice, setAuthNotice] = useState("");
  const [refreshDraft, setRefreshDraft] = useState(() => String(refreshSeconds));
  const [refreshSaving, setRefreshSaving] = useState(false);
  const [refreshError, setRefreshError] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    const refresh = () => getClusterBranding(controller.signal).then((value) => {
      if (!controller.signal.aborted) setBranding((current) => value.revision >= current.revision ? value : current);
    }).catch(() => undefined);
    void refresh();
    const timer = window.setInterval(() => void refresh(), 60000);
    window.addEventListener("focus", refresh);
    return () => { controller.abort(); window.clearInterval(timer); window.removeEventListener("focus", refresh); };
  }, []);

  useEffect(() => { document.title = branding.name === "XIRANG" ? "XIRANG System" : branding.name; }, [branding.name]);

  useEffect(() => setRefreshDraft(String(refreshSeconds)), [refreshSeconds]);

  useEffect(() => {
    const controller = new AbortController();
    getVersion(controller.signal).then(setVersion).catch(() => setVersion(null));
    getSetupStatus().then((required) => { setSetupRequired(required); if (required) setAuthMode("register"); }).catch(() => undefined);
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!token) return;
    const controller = new AbortController();
    getTelemetryInterval(token, controller.signal).then((settings) => {
      setRefreshSeconds(settings.intervalSeconds);
      setRefreshError("");
    }).catch((cause) => {
      if (!controller.signal.aborted) setRefreshError(cause instanceof Error ? cause.message : "Unable to read telemetry interval");
    });
    return () => controller.abort();
  }, [token]);

  useEffect(() => {
    const synchronizePath = () => {
      const page = navigation.find((item) => item.path === normalizedPath(window.location.pathname));
      if (page) setActive(page.label);
      else {
        window.history.replaceState({}, "", "/");
        setActive("Overview");
      }
    };
    synchronizePath();
    window.addEventListener("popstate", synchronizePath);
    return () => window.removeEventListener("popstate", synchronizePath);
  }, []);

  useEffect(() => {
    if (!principal || pagePermitted(active, principal)) return;
    window.history.replaceState({}, "", "/");
    setActive("Overview");
  }, [active, principal]);

  function navigate(page: Page) {
    const path = navigation.find((item) => item.label === page)?.path ?? "/";
    if (normalizedPath(window.location.pathname) !== path) window.history.pushState({}, "", path);
    setActive(page);
  }

  async function commitRefreshDraft() {
    const parsed = Number(refreshDraft);
    const bounded = Math.min(60, Math.max(2, Math.round(Number.isFinite(parsed) ? parsed : refreshSeconds)));
    if (principal?.role !== "admin" || !token) {
      setRefreshDraft(String(refreshSeconds));
      return;
    }
    setRefreshSaving(true);
    try {
      const settings = await updateTelemetryInterval(bounded, token);
      setRefreshSeconds(settings.intervalSeconds);
      setRefreshError("");
    } catch (cause) {
      setRefreshDraft(String(refreshSeconds));
      setRefreshError(cause instanceof Error ? cause.message : "Unable to update telemetry interval");
    } finally {
      setRefreshSaving(false);
    }
  }

  async function authenticate(event: FormEvent) {
    event.preventDefault();
    setAuthenticating(true);
    setAuthError("");
    try {
      const [, authenticatedPrincipal] = await Promise.all([getNodes(tokenInput), getPrincipal(tokenInput)]);
      setToken(tokenInput);
      setPrincipal(authenticatedPrincipal);
      setTokenInput("");
    } catch (cause) {
      setAuthError(cause instanceof Error ? cause.message : "Authentication failed");
    } finally {
      setAuthenticating(false);
    }
  }

  async function authenticateLocal(event: FormEvent) {
    event.preventDefault(); setAuthenticating(true); setAuthError(""); setAuthNotice("");
    try {
      if (setupRequired) {
        await setupAdmin(credentials);
        setSetupRequired(false);
      } else if (authMode === "register") {
        await registerLocalUser(credentials);
        setAuthNotice(t("注册申请已提交，请等待管理员审批后登录。", "Registration submitted. Sign in after an administrator approves the account."));
        setAuthMode("login");
        return;
      }
      const secret = await loginLocalUser(credentials.username, credentials.password, language);
      const authenticatedPrincipal = await getPrincipal(secret);
      setToken(secret); setPrincipal(authenticatedPrincipal); setCredentials({ username: "", displayName: "", password: "" });
    } catch (cause) {
      setAuthError(cause instanceof Error ? cause.message : "Authentication failed");
    } finally { setAuthenticating(false); }
  }

  if (!token) {
    return <main className="auth-shell">
      <section className="auth-card" aria-labelledby="auth-title">
        <div className="auth-topline"><div className="brand"><img className="brand-mark" src={branding.logoDataUrl || defaultClusterLogo} alt="" aria-hidden="true" /><strong title={branding.name}>{branding.name}</strong></div><InterfacePreferences /></div>
        <h2 id="auth-title">{setupRequired ? t("创建首位管理员", "Create the first administrator") : authMode === "register" ? t("申请账户", "Request an account") : t("进入控制台", "Enter the console")}</h2>
        {!setupRequired && <div className="auth-tabs"><button type="button" className={authMode === "login" ? "active" : ""} onClick={() => setAuthMode("login")}>{t("账户登录", "Account login")}</button><button type="button" className={authMode === "register" ? "active" : ""} onClick={() => setAuthMode("register")}>{t("注册申请", "Registration")}</button><button type="button" className={authMode === "token" ? "active" : ""} onClick={() => setAuthMode("token")}>{t("令牌", "Token")}</button></div>}
        {authMode === "token" && !setupRequired ? <form onSubmit={authenticate}>
          <label htmlFor="admin-token">{t("访问令牌", "Access token")}</label>
          <input id="admin-token" type="password" value={tokenInput} onChange={(event) => setTokenInput(event.target.value)} autoComplete="current-password" autoFocus placeholder={t("输入访问令牌", "Enter access token")} />
          {authError && <p className="form-error" role="alert">{authError}</p>}
          <button className="primary-button" type="submit" disabled={!tokenInput || authenticating}>{authenticating ? t("正在验证…", "Verifying…") : t("登录", "Sign in")}<span aria-hidden="true">→</span></button>
        </form> : <form onSubmit={authenticateLocal}>
          <label htmlFor="username">{t("用户名", "Username")}</label><input id="username" value={credentials.username} onChange={(event) => setCredentials({ ...credentials, username: event.target.value })} minLength={3} maxLength={64} required autoFocus />
          {(setupRequired || authMode === "register") && <><label htmlFor="display-name">{t("显示名称", "Display name")}</label><input id="display-name" value={credentials.displayName} onChange={(event) => setCredentials({ ...credentials, displayName: event.target.value })} maxLength={128} /></>}
          <label htmlFor="password">{t("密码（至少 12 位）", "Password (at least 12 characters)")}</label><input id="password" type="password" value={credentials.password} onChange={(event) => setCredentials({ ...credentials, password: event.target.value })} minLength={12} maxLength={1024} required autoComplete={authMode === "register" || setupRequired ? "new-password" : "current-password"} />
          {authError && <p className="form-error" role="alert">{authError}</p>}{authNotice && <p className="form-notice">{authNotice}</p>}
          <button className="primary-button" type="submit" disabled={authenticating}>{authenticating ? t("处理中…", "Working…") : setupRequired ? t("创建并登录", "Create and sign in") : authMode === "register" ? t("提交审批", "Submit for approval") : t("登录", "Sign in")}<span aria-hidden="true">→</span></button>
        </form>}
        <small>{authMode === "token" ? t("刷新或关闭页面后需重新登录。", "Sign in again after refreshing or closing this page.") : t("新账户需经管理员审批。", "New accounts require administrator approval.")}</small>
        {version && <div className="auth-version">{version.version}</div>}
      </section>
    </main>;
  }

  // A Record keyed by Page makes the console exhaustive by type: adding a
  // navigation entry without a matching view becomes a compile error rather
  // than a silent fall-through to the empty state.
  const isAdministrator = principal?.role === "admin";
  const pageViews: Record<Page, () => ReactNode> = {
    Overview: () => <OverviewPage token={token} onNavigate={navigate} />,
    Topology: () => <TopologyPage token={token} />,
    Alerts: () => <AlertsPage token={token} />,
    Nodes: () => <NodesPage token={token} isAdmin={isAdministrator} />,
    Enrollment: () => <EnrollmentPage token={token} />,
    Jobs: () => <JobsPage token={token} isAdmin={isAdministrator} />,
    Queues: () => <QueuesPage token={token} />,
    Builds: () => <BuildsPage token={token} isAdmin={isAdministrator} />,
    Storage: () => <StoragePage token={token} isAdmin={isAdministrator} />,
    Accounting: () => <AccountingPage token={token} isAdmin={isAdministrator} />,
    Terminal: () => <TerminalPage token={token} />,
    Access: () => <AccessPage token={token} />,
    Health: () => <HealthPage token={token} />,
    Settings: () => <SettingsPage token={token} onSaved={setBranding} />,
  };
  const activeView = pageViews[active];

  return (
    <div className="app-shell">
      <a className="skip-navigation" href="#main-content" onClick={(event) => { event.preventDefault(); document.getElementById("main-content")?.focus(); }}>{t("跳到主要内容", "Skip to main content")}</a>
      <aside className="sidebar">
        <div className="brand"><img className="brand-mark" src={branding.logoDataUrl || defaultClusterLogo} alt="" aria-hidden="true" /><div><strong title={branding.name}>{branding.name}</strong></div></div>
        <nav aria-label={t("主导航", "Primary navigation")}>
          {navigation.filter((item) => item.available && Boolean(principal && pagePermitted(item.label, principal))).map((item) => {
            const permitted = item.available && Boolean(principal && pagePermitted(item.label, principal));
            return (
            <button className={item.label === active ? "active" : ""} key={item.label} onClick={() => permitted && navigate(item.label)} type="button" aria-label={language === "zh" ? item.zh : item.en} aria-current={item.label === active ? "page" : undefined} title={language === "zh" ? item.zh : item.en}>
              <WorkspaceIcon name={item.icon} /><span>{language === "zh" ? item.zh : item.en}</span>
            </button>
          )})}
        </nav>
        <div className="sidebar-footer">
          <small>{principal?.displayName || principal?.subject}</small>
          <strong>{version?.version ?? "development"}</strong>
          <button type="button" onClick={() => { setToken(""); setPrincipal(null); }}>{t("退出登录", "Sign out")}</button>
        </div>
      </aside>
      <main className="main-content" id="main-content" tabIndex={-1}>
        <div className="workspace-controls" aria-label={t("界面偏好", "Interface preferences")}>
          <label className={`refresh-control${refreshError ? " refresh-control--error" : ""}`} title={refreshError || (principal?.role === "admin" ? t("同步设置浏览器轮询与所有 Agent 的遥测上报间隔", "Synchronizes browser polling and telemetry reporting for every Agent") : t("遥测间隔由管理员统一设置", "Telemetry interval is managed by an administrator"))}>
            <span>{refreshSaving ? t("同步中", "Syncing") : t("遥测", "Telemetry")}</span>
            <input type="number" min={2} max={60} step={1} value={refreshDraft} disabled={principal?.role !== "admin" || refreshSaving} onChange={(event) => setRefreshDraft(event.target.value)} onBlur={() => void commitRefreshDraft()} onKeyDown={(event) => { if (event.key === "Enter") event.currentTarget.blur(); }} aria-label={t("Agent 遥测间隔（秒）", "Agent telemetry interval in seconds")} />
            <small>s</small>
          </label>
          <InterfacePreferences />
        </div>
        <PageErrorBoundary key={active} page={active} language={language}>
          {activeView ? activeView() : (
            <section className="empty-state">
              <span className="eyebrow">{t("页面不可用", "Page unavailable")}</span>
              <h1>{active}</h1>
              <p>{t("请从左侧导航选择可用页面。", "Choose an available page from the navigation.")}</p>
            </section>
          )}
        </PageErrorBoundary>
      </main>
    </div>
  );
}

function normalizedPath(pathname: string): string {
  if (pathname === "/") return pathname;
  return pathname.replace(/\/+$/, "") || "/";
}

function pageFromPath(pathname: string): Page {
  return navigation.find((item) => item.path === normalizedPath(pathname))?.label ?? "Overview";
}

function pagePermitted(page: Page, principal: AccessPrincipal): boolean {
  if (page === "Settings" || page === "Health") return principal.role === "admin";
  const permission = page === "Access" ? "access.manage"
    : page === "Terminal" ? "access.manage"
      : page === "Storage" || page === "Accounting" ? "cluster.read"
    : page === "Enrollment" ? "nodes.manage"
      : page === "Queues" ? "queues.manage"
        : page === "Builds" ? "builds.manage"
          : page === "Jobs" ? "jobs.manage"
            : "cluster.read";
  return principal.permissions.includes("*") || principal.permissions.includes(permission);
}

function InterfacePreferences() {
  const { language, setLanguage, theme, setTheme, t } = usePreferences();
  return <div className="interface-preferences">
    <label className="preference-select"><span className="sr-only">{t("语言", "Language")}</span><select value={language} onChange={(event) => setLanguage(event.target.value as "zh" | "en")}><option value="zh">中文</option><option value="en">English</option></select></label>
    <button className="theme-toggle" type="button" onClick={() => setTheme(theme === "dark" ? "light" : "dark")} aria-label={t("切换深浅色模式", "Toggle color theme")} title={theme === "dark" ? t("切换到浅色", "Use light theme") : t("切换到深色", "Use dark theme")}><span aria-hidden="true">{theme === "dark" ? "☼" : "◐"}</span></button>
  </div>;
}
