import { FormEvent, useEffect, useState } from "react";
import { getNodes, getPrincipal, getSetupStatus, getTelemetryInterval, getVersion, loginLocalUser, registerLocalUser, setupAdmin, updateTelemetryInterval } from "../api/client";
import type { AccessPrincipal, VersionInfo } from "../api/contracts";
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

const navigation = [
  { label: "Overview", en: "Cluster overview", zh: "集群概览", icon: "overview", path: "/", available: true },
  { label: "Topology", en: "Network topology", zh: "网络拓扑", icon: "topology", path: "/topology", available: true },
  { label: "Alerts", en: "Alert center", zh: "告警中心", icon: "alerts", path: "/alerts", available: true },
  { label: "Nodes", en: "Node status", zh: "节点状态", icon: "nodes", path: "/nodes", available: true },
  { label: "Enrollment", en: "Node enrollment", zh: "节点接入", icon: "enrollment", path: "/enrollment", available: true },
  { label: "Jobs", en: "Job scheduling", zh: "作业调度", icon: "jobs", path: "/jobs", available: true },
  { label: "Queues", en: "Queue policies", zh: "队列策略", icon: "queues", path: "/queues", available: true },
  { label: "Builds", en: "Native builds", zh: "原生构建", icon: "builds", path: "/builds", available: true },
  { label: "Storage", en: "Shared storage", zh: "共享存储", icon: "storage", path: "/storage", available: true },
  { label: "Accounting", en: "Usage accounting", zh: "用量核算", icon: "accounting", path: "/accounting", available: true },
  { label: "Terminal", en: "Terminal access", zh: "终端连接", icon: "terminal", path: "/terminal", available: true },
  { label: "Access", en: "Identity management", zh: "身份管理", icon: "access", path: "/access", available: true },
] as const;

type Page = typeof navigation[number]["label"];

export function App() {
  const { language, setLanguage, theme, setTheme, refreshSeconds, setRefreshSeconds, t } = usePreferences();
  const [active, setActive] = useState<Page>(() => pageFromPath(window.location.pathname));
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
      <section className="auth-intro">
        <div className="brand brand--large"><img className="brand-mark" src="/brand/xirang-mark.png" alt="" aria-hidden="true" /><div><strong>XIRANG</strong><small>Heterogeneous Cluster Control Plane</small></div></div>
        <span className="auth-kicker">ORCHESTRATION · SCHEDULING · TOPOLOGY</span>
        <h1><em>XIRANG</em><span><b>—</b> eXtensible Infrastructure for<br />Resource-aware Adaptive Networked Grids</span></h1>
        <div className="auth-positioning">
          <p>A self-expanding orchestration and scheduling platform for heterogeneous computing clusters.</p>
          {language === "zh" && <p lang="zh-CN">一个面向异构计算集群的自扩展编排与调度平台。</p>}
        </div>
        <div className="auth-features" aria-label="System capabilities">
          <span><i>01</i> {t("mTLS 节点身份", "mTLS node identity")}</span>
          <span><i>02</i> {t("实时资源遥测", "Live resource telemetry")}</span>
          <span><i>03</i> {t("当前节点拓扑", "Current node topology")}</span>
        </div>
      </section>
      <section className="auth-card" aria-labelledby="auth-title">
        <div className="auth-status"><span /> CONTROL PLANE {version ? "REACHABLE" : "DISCOVERING"}</div>
        <span className="eyebrow">{setupRequired ? "First-run setup" : "Identity portal"}</span>
        <h2 id="auth-title">{setupRequired ? t("创建首位管理员", "Create the first administrator") : authMode === "register" ? t("申请账户", "Request an account") : t("进入控制台", "Enter the console")}</h2>
        {!setupRequired && <div className="auth-tabs"><button type="button" className={authMode === "login" ? "active" : ""} onClick={() => setAuthMode("login")}>{t("账户登录", "Account login")}</button><button type="button" className={authMode === "register" ? "active" : ""} onClick={() => setAuthMode("register")}>{t("注册申请", "Registration")}</button><button type="button" className={authMode === "token" ? "active" : ""} onClick={() => setAuthMode("token")}>{t("令牌", "Token")}</button></div>}
        {authMode === "token" && !setupRequired ? <form onSubmit={authenticate}>
          <label htmlFor="admin-token">Bootstrap / API / OIDC Token</label>
          <input id="admin-token" type="password" value={tokenInput} onChange={(event) => setTokenInput(event.target.value)} autoComplete="current-password" autoFocus placeholder="Enter access credential" />
          {authError && <p className="form-error" role="alert">{authError}</p>}
          <button className="primary-button" type="submit" disabled={!tokenInput || authenticating}>{authenticating ? t("正在验证…", "Verifying…") : t("连接控制平面", "Connect to control plane")}<span>→</span></button>
        </form> : <form onSubmit={authenticateLocal}>
          <label htmlFor="username">{t("用户名", "Username")}</label><input id="username" value={credentials.username} onChange={(event) => setCredentials({ ...credentials, username: event.target.value })} minLength={3} maxLength={64} required autoFocus />
          {(setupRequired || authMode === "register") && <><label htmlFor="display-name">{t("显示名称", "Display name")}</label><input id="display-name" value={credentials.displayName} onChange={(event) => setCredentials({ ...credentials, displayName: event.target.value })} maxLength={128} /></>}
          <label htmlFor="password">{t("密码（至少 12 位）", "Password (at least 12 characters)")}</label><input id="password" type="password" value={credentials.password} onChange={(event) => setCredentials({ ...credentials, password: event.target.value })} minLength={12} maxLength={1024} required autoComplete={authMode === "register" || setupRequired ? "new-password" : "current-password"} />
          {authError && <p className="form-error" role="alert">{authError}</p>}{authNotice && <p className="form-notice">{authNotice}</p>}
          <button className="primary-button" type="submit" disabled={authenticating}>{authenticating ? t("处理中…", "Working…") : setupRequired ? t("创建并登录", "Create and sign in") : authMode === "register" ? t("提交审批", "Submit for approval") : t("登录", "Sign in")}<span>→</span></button>
        </form>}
        <small>{authMode === "token" ? t("令牌只保留在本页内存，刷新或关闭页面后自动清除。", "The token stays in page memory only and is cleared on refresh or close.") : t("新注册账户须由管理员审批；普通用户只能操作自己的任务。", "New accounts require administrator approval; users can operate only their own jobs.")}</small>
        <div className="auth-version"><span>{version?.version ?? "—"}</span><span>{version ? `${version.os} / ${version.architecture}` : "API unavailable"}</span></div>
      </section>
    </main>;
  }

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand"><img className="brand-mark" src="/brand/xirang-mark.png" alt="" aria-hidden="true" /><div><strong>XIRANG</strong><small>Adaptive Networked Grids</small></div></div>
        <span className="nav-caption">{t("工作区", "Workspace")}</span>
        <nav aria-label="Primary navigation">
          {navigation.filter((item) => item.available && Boolean(principal && pagePermitted(item.label, principal))).map((item) => {
            const permitted = item.available && Boolean(principal && pagePermitted(item.label, principal));
            return (
            <button className={item.label === active ? "active" : ""} key={item.label} onClick={() => permitted && navigate(item.label)} type="button" title={`${language === "zh" ? item.zh : item.en} · ${item.path}`}>
              <WorkspaceIcon name={item.icon} /><span>{language === "zh" ? item.zh : item.en}</span>
            </button>
          )})}
        </nav>
        <div className="sidebar-footer">
          <div className="session-line"><i /><span>{(principal?.role ?? "authenticated").toUpperCase()} SESSION</span></div>
          <small>{principal?.displayName || principal?.subject}</small>
          <strong>{version?.version ?? "development"}</strong>
          <small>{version ? `${version.os}/${version.architecture}` : "API connected"}</small>
          <button type="button" onClick={() => { setToken(""); setPrincipal(null); }}>{t("安全退出", "Sign out")}</button>
        </div>
      </aside>
      <main className="main-content">
        <div className="workspace-controls" aria-label={t("界面偏好", "Interface preferences")}>
          <label className={`refresh-control${refreshError ? " refresh-control--error" : ""}`} title={refreshError || (principal?.role === "admin" ? t("同步设置浏览器轮询与所有 Agent 的遥测上报间隔", "Synchronizes browser polling and telemetry reporting for every Agent") : t("遥测间隔由管理员统一设置", "Telemetry interval is managed by an administrator"))}>
            <span>{refreshSaving ? t("同步中", "Syncing") : t("遥测", "Telemetry")}</span>
            <input type="number" min={2} max={60} step={1} value={refreshDraft} disabled={principal?.role !== "admin" || refreshSaving} onChange={(event) => setRefreshDraft(event.target.value)} onBlur={() => void commitRefreshDraft()} onKeyDown={(event) => { if (event.key === "Enter") event.currentTarget.blur(); }} aria-label={t("Agent 遥测间隔（秒）", "Agent telemetry interval in seconds")} />
            <small>s</small>
          </label>
          <label className="preference-select"><span className="sr-only">{t("语言", "Language")}</span><select value={language} onChange={(event) => setLanguage(event.target.value as "zh" | "en")} aria-label={t("语言", "Language")}><option value="zh">中文</option><option value="en">English</option></select></label>
          <button className="theme-toggle" type="button" onClick={() => setTheme(theme === "dark" ? "light" : "dark")} aria-label={t("切换深浅色模式", "Toggle color theme")} title={theme === "dark" ? t("切换到浅色", "Use light theme") : t("切换到深色", "Use dark theme")}>
            {theme === "dark" ? <span aria-hidden="true">☼</span> : <span aria-hidden="true">◐</span>}
          </button>
        </div>
        <PageErrorBoundary key={`${active}-${language}`} page={active} language={language}>
          {active === "Overview" ? <OverviewPage token={token} onNavigate={navigate} /> : active === "Topology" ? <TopologyPage token={token} /> : active === "Alerts" ? <AlertsPage token={token} /> : active === "Nodes" ? <NodesPage token={token} isAdmin={principal?.role === "admin"} /> : active === "Enrollment" ? <EnrollmentPage token={token} /> : active === "Jobs" ? <JobsPage token={token} isAdmin={principal?.role === "admin"} /> : active === "Queues" ? <QueuesPage token={token} /> : active === "Builds" ? <BuildsPage token={token} /> : active === "Storage" ? <StoragePage token={token} isAdmin={principal?.role === "admin"} /> : active === "Accounting" ? <AccountingPage token={token} isAdmin={principal?.role === "admin"} /> : active === "Terminal" ? <TerminalPage token={token} /> : active === "Access" ? <AccessPage token={token} /> : (
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
