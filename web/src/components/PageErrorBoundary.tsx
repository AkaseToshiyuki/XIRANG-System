import { Component, type ErrorInfo, type ReactNode } from "react";

interface Props {
  children: ReactNode;
  page: string;
  language: "zh" | "en";
}

interface State {
  error: Error | null;
}

export class PageErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error(`XIRANG page "${this.props.page}" crashed`, error, info.componentStack);
  }

  render() {
    if (!this.state.error) return this.props.children;
    const zh = this.props.language === "zh";
    return <section className="page-crash" role="alert">
      <span className="eyebrow">{zh ? "页面恢复" : "Page recovery"}</span>
      <h1>{zh ? `${this.props.page} 暂时无法显示` : `${this.props.page} is temporarily unavailable`}</h1>
      <p>{zh ? "页面遇到了未处理的数据。控制台其余功能仍可继续使用。" : "This page encountered unhandled data. The rest of the console remains available."}</p>
      <details><summary>{zh ? "诊断信息" : "Diagnostic details"}</summary><code>{this.state.error.message}</code></details>
      <div>
        <button className="primary-button" type="button" onClick={() => this.setState({ error: null })}>{zh ? "重新载入此页面" : "Reload this page"}<span>↻</span></button>
        <button className="secondary-button" type="button" onClick={() => window.location.reload()}>{zh ? "刷新控制台" : "Refresh console"}</button>
      </div>
    </section>;
  }
}
