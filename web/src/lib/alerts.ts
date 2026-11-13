import type { Alert } from "../api/contracts";

export function isActiveAlert(alert: Alert): boolean {
  return alert.status === "firing" || alert.status === "acknowledged";
}

// An older poll response must not undo a state transition already received by
// the event stream or by the operator's action response.
export function mergeAlerts(current: Alert[], updates: Alert[]): Alert[] {
  const result = new Map(current.map((alert) => [alert.id, alert]));
  for (const alert of updates) {
    const previous = result.get(alert.id);
    if (!previous || alert.revision >= previous.revision) result.set(alert.id, alert);
  }
  return [...result.values()];
}
