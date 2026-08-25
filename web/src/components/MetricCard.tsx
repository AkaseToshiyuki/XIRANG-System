interface MetricCardProps {
  label: string;
  value: string;
  detail: string;
  tone?: "cyan" | "violet" | "amber";
  progress?: number;
}

export function MetricCard({ label, value, detail, tone = "cyan", progress }: MetricCardProps) {
  return (
    <article className={`metric-card metric-card--${tone}`}>
      <span className="eyebrow">{label}</span>
      <strong>{value}</strong>
      <span className="metric-detail">{detail}</span>
      {progress !== undefined && <span className="metric-progress"><i style={{ width: `${progress}%` }} /></span>}
    </article>
  );
}
