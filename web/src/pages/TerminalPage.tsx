import { FormEvent, useEffect, useRef, useState } from "react";
import { createTerminalTicket, getNodes } from "../api/client";
import type { ClusterNode } from "../api/contracts";
import { usePreferences } from "../app/preferences";

export function TerminalPage({ token }: { token: string }) {
  const { t } = usePreferences();
  const [nodes, setNodes] = useState<ClusterNode[]>([]);
  const [nodeID, setNodeID] = useState("");
  const [output, setOutput] = useState(() => t("选择节点后建立管理员终端。\n", "Select a node to open an administrator terminal.\n"));
  const [command, setCommand] = useState("");
  const [status, setStatus] = useState<"closed" | "connecting" | "open">("closed");
  const [error, setError] = useState("");
  const socket = useRef<WebSocket | null>(null);
  const outputElement = useRef<HTMLPreElement>(null);

  useEffect(() => {
    const controller = new AbortController();
    getNodes(token, controller.signal).then((items) => {
      const accessible = items.filter((node) => node.status === "ready" || node.status === "cordoned" || node.status === "draining");
      setNodes(accessible);
      setNodeID(accessible[0]?.id ?? "");
    }).catch((cause) => setError(cause instanceof Error ? cause.message : t("无法初始化终端", "Unable to initialize terminal")));
    return () => { controller.abort(); socket.current?.close(); };
  }, [token, t]);

  useEffect(() => {
    outputElement.current?.scrollTo({ top: outputElement.current.scrollHeight });
  }, [output]);

  async function connect() {
    socket.current?.close();
    setStatus("connecting");
    setError("");
    try {
      const ticket = await createTerminalTicket(nodeID, token);
      const scheme = window.location.protocol === "https:" ? "wss" : "ws";
      const next = new WebSocket(`${scheme}://${window.location.host}/api/v1/terminal/connect?ticket=${encodeURIComponent(ticket)}`);
      next.binaryType = "arraybuffer";
      next.onopen = () => { setStatus("open"); setOutput((current) => current + t("\n会话已连接。\n", "\nSession connected.\n")); };
      next.onmessage = (event) => {
        const text = typeof event.data === "string" ? event.data : new TextDecoder().decode(event.data);
        setOutput((current) => (current + stripANSI(text)).slice(-200_000));
      };
      next.onerror = () => setError(t("Agent PTY 会话连接失败，请检查节点 Agent 的在线状态。", "Agent PTY session failed. Check that the node Agent is online."));
      next.onclose = () => setStatus("closed");
      socket.current = next;
    } catch (cause) {
      setStatus("closed");
      setError(cause instanceof Error ? cause.message : t("终端连接失败", "Terminal connection failed"));
    }
  }

  function send(event: FormEvent) {
    event.preventDefault();
    if (!command || socket.current?.readyState !== WebSocket.OPEN) return;
    socket.current.send(command + "\n");
    setCommand("");
  }

  return <section>
    <header className="page-header"><div><span className="eyebrow">{t("连接节点命令行", "Open a node shell")}</span><h1>{t("管理员终端", "Administrator terminal")}</h1><p>{t("通过节点现有的 mTLS Agent 通道建立临时 PTY；关闭页面或断开连接会立即结束会话。", "Open a temporary PTY over the node's authenticated mTLS Agent channel. Closing or disconnecting ends it immediately.")}</p></div></header>
    {error && <p className="error-banner" role="alert">{error}</p>}
    <div className="terminal-toolbar">
      <label><span>{t("目标节点", "Target node")}</span><select value={nodeID} onChange={(event) => setNodeID(event.target.value)}>{nodes.map((node) => <option value={node.id} key={node.id}>{node.name} · {node.architecture}</option>)}</select></label>
      <button className="primary-button" type="button" disabled={!nodeID || status === "connecting"} onClick={() => void connect()}>{status === "open" ? t("重新连接", "Reconnect") : status === "connecting" ? t("连接中…", "Connecting…") : t("连接终端", "Connect terminal")}<span>→</span></button>
      <button type="button" disabled={status !== "open"} onClick={() => socket.current?.send("\u0003")}>{t("发送 Ctrl-C", "Send Ctrl-C")}</button>
    </div>
    <div className="terminal-key"><span>{t("安全边界", "Security boundary")}</span><code>Agent mTLS · PTY · 10 min idle · 30 min max</code><small>{t("会话受管理员权限、一次性票据、并发限制和审计约束；不会分发 SSH 私钥。", "Sessions require administrator access and a one-use ticket, and enforce concurrency limits and audit metadata. No SSH private key is distributed.")}</small></div>
    <pre ref={outputElement} className="terminal-output" aria-live="polite">{output}</pre>
    <form className="terminal-input" onSubmit={send}><span>$</span><input value={command} onChange={(event) => setCommand(event.target.value)} disabled={status !== "open"} autoComplete="off" spellCheck={false} placeholder={status === "open" ? t("输入命令并按 Enter", "Enter a command and press Enter") : t("请先连接节点", "Connect to a node first")} /><button disabled={status !== "open" || !command}>{t("发送", "Send")}</button></form>
  </section>;
}

function stripANSI(value: string) {
  return value.replace(/\u001b\][^\u0007]*(?:\u0007|\u001b\\)/g, "").replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "").replace(/\r(?!\n)/g, "");
}
