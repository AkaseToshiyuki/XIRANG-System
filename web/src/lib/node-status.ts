import type { Alert, ClusterNode, NodeHealthSnapshot } from "../api/contracts";
import { isActiveAlert } from "./alerts";

export function nodeStatusTone(node: ClusterNode, snapshot: NodeHealthSnapshot | undefined, alerts: Alert[], now = Date.now()): "ready" | "warning" | "critical" | "offline" {
  if (node.status === "offline" || node.status === "removed") return "offline";
  if (node.status === "unreachable" || node.status === "rejected") return "critical";
  const active = alerts.filter((alert) => alert.sourceNodeId === node.id && isActiveAlert(alert));
  if (active.some((alert) => alert.severity === "critical")) return "critical";
  let warning = active.some((alert) => alert.severity === "warning") || node.status !== "ready" || Boolean(node.pendingModeCommandId) || Boolean(node.managementPanelStatus?.startsWith("error:"));
  if (snapshot) {
    const ages = [snapshot.observedAt, snapshot.receivedAt].map((value) => now - Date.parse(value));
    if (ages.some((age) => !Number.isFinite(age) || age < -30_000 || age > 180_000)) return "warning";
    for (const sensor of snapshot.sensors) {
      const status = sensor.status.trim().toLowerCase();
      if (["cr", "nr", "lcr", "ucr", "lnr", "unr", "critical", "non-recoverable", "lower critical", "upper critical"].includes(status) || (sensor.available && ((sensor.lowerCritical !== undefined && sensor.value <= sensor.lowerCritical) || (sensor.upperCritical !== undefined && sensor.value >= sensor.upperCritical)))) return "critical";
      if (["nc", "lnc", "unc", "warning", "non-critical", "lower non-critical", "upper non-critical"].includes(status)) warning = true;
    }
    warning ||= snapshot.sources.some((source) => source.status === "partial" || source.status === "unavailable");
  }
  return warning ? "warning" : "ready";
}
