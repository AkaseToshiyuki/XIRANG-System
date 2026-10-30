type IconName = "overview" | "topology" | "alerts" | "nodes" | "enrollment" | "jobs" | "queues" | "builds" | "storage" | "accounting" | "terminal" | "access" | "settings" | "health";

export function WorkspaceIcon({ name }: { name: IconName }) {
  const common = { fill: "none", stroke: "currentColor", strokeWidth: 1.7, strokeLinecap: "round" as const, strokeLinejoin: "round" as const };
  return <span className="workspace-icon" aria-hidden="true"><svg viewBox="0 0 24 24" {...common}>
    {name === "health" && <><path d="M3 12h4l3-7 4 14 3-7h4" /><rect x="2" y="2" width="20" height="20" rx="4" /></>}
    {name === "settings" && <><path d="M4 6h16M4 12h16M4 18h16" /><circle cx="9" cy="6" r="2" /><circle cx="16" cy="12" r="2" /><circle cx="8" cy="18" r="2" /></>}
    {name === "overview" && <><path d="M4 4h6v6H4zM14 4h6v4h-6zM14 12h6v8h-6zM4 14h6v6H4z" /></>}
    {name === "topology" && <><circle cx="6" cy="6" r="2.2" /><circle cx="18" cy="7" r="2.2" /><circle cx="12" cy="18" r="2.2" /><path d="m8 7 7.8-.2M7.4 7.8l3.4 8M16.8 9l-3.5 6.8" /></>}
    {name === "alerts" && <><path d="M6.5 9a5.5 5.5 0 0 1 11 0c0 6 2.5 6 2.5 7.5H4c0-1.5 2.5-1.5 2.5-7.5Z" /><path d="M9.5 19a2.7 2.7 0 0 0 5 0" /></>}
    {name === "nodes" && <><rect x="4" y="4" width="16" height="6" rx="1.5" /><rect x="4" y="14" width="16" height="6" rx="1.5" /><path d="M8 7h.01M8 17h.01M12 7h5M12 17h5" /></>}
    {name === "enrollment" && <><circle cx="9" cy="8" r="3" /><path d="M3.8 19c.5-3.5 2.2-5.3 5.2-5.3 1.4 0 2.5.4 3.3 1.1M17 12v8M13 16h8" /></>}
    {name === "jobs" && <><path d="m5 7 4 4-4 4M11 17h8" /><rect x="2.5" y="3.5" width="19" height="17" rx="2" /></>}
    {name === "queues" && <><path d="m4 7 8-4 8 4-8 4-8-4Z" /><path d="m4 12 8 4 8-4M4 17l8 4 8-4" /></>}
    {name === "builds" && <><path d="M14.5 5.5 18 2l4 4-3.5 3.5M13 7l4 4-8.5 8.5-4.5.5.5-4.5L13 7Z" /><path d="m5 15 4 4" /></>}
    {name === "storage" && <><ellipse cx="12" cy="5.5" rx="7.5" ry="3" /><path d="M4.5 5.5v6c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3v-6M4.5 11.5v6c0 1.7 3.4 3 7.5 3s7.5-1.3 7.5-3v-6" /></>}
    {name === "accounting" && <><path d="M4 19V9M9.3 19V5M14.7 19v-7M20 19V3" /><path d="M2.5 19.5h19M4 7l5.3-3 5.4 6L20 2.8" /></>}
    {name === "terminal" && <><rect x="2.5" y="4" width="19" height="16" rx="2" /><path d="m6 9 3 3-3 3M12 16h5" /></>}
    {name === "access" && <><path d="M12 2.8 19 6v5.2c0 4.5-2.8 8-7 10-4.2-2-7-5.5-7-10V6l7-3.2Z" /><path d="M9.5 11.5 11 13l3.8-4" /></>}
  </svg></span>;
}
