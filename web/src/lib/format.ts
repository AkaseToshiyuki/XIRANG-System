import type { NodeResourceObservation } from "../api/contracts";

export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  const units = ["B", "K", "M", "G", "T"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(value >= 10 || unit === 0 ? 0 : 1)}${units[unit]}`;
}

export function formatByteRate(bytesPerSecond: number): string {
  return `${formatBytes(bytesPerSecond)}/s`;
}

export function formatCoreCount(milliCpu: number): string {
  const cores = milliCpu / 1000;
  return Number.isInteger(cores) ? String(cores) : cores.toFixed(3).replace(/0+$/, "").replace(/\.$/, "");
}

export function formatBitRate(bitsPerSecond: number): string {
  if (!Number.isFinite(bitsPerSecond) || bitsPerSecond < 0) return "—";
  const units = ["bit/s", "Kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"];
  let value = bitsPerSecond;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit++;
  }
  return `${value.toFixed(value >= 10 || unit === 0 ? 0 : 1)} ${units[unit]}`;
}

export function percentage(used: number, capacity: number): number {
  if (capacity <= 0) return 0;
  return Math.max(0, Math.min(100, used / capacity * 100));
}

export function formatPercent(used: number, capacity: number): string {
  if (capacity <= 0) return "—";
  return `${percentage(used, capacity).toFixed(0)}%`;
}

export function networkBytesPerSecond(observations: NodeResourceObservation[]): number | undefined {
  const rates = networkDirectionBytesPerSecond(observations);
  if (!rates) return undefined;
  return rates.received + rates.transmitted;
}

export function networkDirectionBytesPerSecond(observations: NodeResourceObservation[]): { received: number; transmitted: number } | undefined {
  if (observations.length < 2) return undefined;
  const current = observations[0];
  const previous = observations[1];
  const seconds = (Date.parse(current.observedAt) - Date.parse(previous.observedAt)) / 1000;
  if (seconds <= 0 || current.receivedBytes < previous.receivedBytes || current.transmittedBytes < previous.transmittedBytes) return undefined;
  return {
    received: (current.receivedBytes - previous.receivedBytes) / seconds,
    transmitted: (current.transmittedBytes - previous.transmittedBytes) / seconds,
  };
}

export function relativeTime(value: string): string {
  const elapsed = Date.now() - Date.parse(value);
  if (!Number.isFinite(elapsed)) return "unknown";
  if (elapsed < 10_000) return "just now";
  if (elapsed < 60_000) return `${Math.floor(elapsed / 1000)}s ago`;
  if (elapsed < 3_600_000) return `${Math.floor(elapsed / 60_000)}m ago`;
  if (elapsed < 86_400_000) return `${Math.floor(elapsed / 3_600_000)}h ago`;
  return `${Math.floor(elapsed / 86_400_000)}d ago`;
}

export function clockTime(value: string): string {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "—";
  return [date.getHours(), date.getMinutes(), date.getSeconds()].map((part) => String(part).padStart(2, "0")).join(":");
}
