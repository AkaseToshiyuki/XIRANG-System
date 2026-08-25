export type JobScriptDraft = {
  project: string; queue: string; priority: string; attempts: string;
  application: string; artifact: string; sha256: string; architecture: string; abi: string; entrypoint: string;
  cpu: string; memory: string; timeout: string; nodes: string; tasksPerNode: string;
  launcher: string; workdir: string; array: string; arguments: string; dependencies: string;
  environment: string; devices: string; batchSpace: string; batchFile: string; batchArg: string; batchNodes: string;
};

export const emptyJobScriptDraft: JobScriptDraft = {
  project: "default", queue: "default", priority: "0", attempts: "1", application: "", artifact: "", sha256: "",
  architecture: "amd64", abi: "", entrypoint: "", cpu: "1", memory: "1G", timeout: "",
  nodes: "1", tasksPerNode: "1", launcher: "native", workdir: "", array: "", arguments: "",
  dependencies: "", environment: "", devices: "", batchSpace: "", batchFile: "", batchArg: "", batchNodes: "",
};

export function parseJobScript(script: string): JobScriptDraft {
  const draft = { ...emptyJobScriptDraft };
  const args: string[] = [], dependencies: string[] = [], environment: string[] = [], devices: string[] = [];
  for (const source of script.split(/\r?\n/)) {
    const line = source.trim();
    if (!line || line.startsWith("#") || line.toUpperCase() === "XIRANG 1") continue;
    const separator = line.indexOf("=");
    if (separator < 1) throw new Error(`Invalid template line: ${source}`);
    const key = line.slice(0, separator).trim();
    const value = unquote(line.slice(separator + 1).trim());
    switch (key) {
      case "project": case "queue": case "priority": case "attempts": case "application": case "artifact": case "sha256":
      case "architecture": case "abi": case "entrypoint": case "cpu": case "memory": case "timeout":
      case "nodes": case "launcher": case "workdir": case "array":
        draft[key] = value; break;
      case "tasks_per_node": draft.tasksPerNode = value; break;
      case "batch_space": draft.batchSpace = value; break;
      case "batch_file": draft.batchFile = value; break;
      case "batch_arg": draft.batchArg = value; break;
      case "batch_nodes": draft.batchNodes = value; break;
      case "arg": args.push(value); break;
      case "depends": dependencies.push(value); break;
      default:
        if (key.startsWith("env.")) environment.push(`${key.slice(4)}=${value}`);
        else if (key.startsWith("device.")) devices.push(`${key.slice(7)}=${value}`);
        else throw new Error(`Unsupported template field: ${key}`);
    }
  }
  draft.arguments = args.join("\n"); draft.dependencies = dependencies.join("\n");
  draft.environment = environment.join("\n"); draft.devices = devices.join("\n");
  return draft;
}

export function serializeJobScript(draft: JobScriptDraft): string {
  const lines = ["XIRANG 1"];
  const add = (key: string, value: string) => { if (value.trim()) lines.push(`${key} = ${value.trim()}`); };
  add("project", draft.project); add("queue", draft.queue); add("priority", draft.priority); add("attempts", draft.attempts);
  add("array", draft.array);
  for (const value of splitLines(draft.dependencies)) add("depends", value);
  add("application", draft.application);
  if (!draft.application.trim()) { add("artifact", draft.artifact); add("sha256", draft.sha256); add("architecture", draft.architecture); add("abi", draft.abi); add("entrypoint", draft.entrypoint); }
  add("batch_space", draft.batchSpace); add("batch_file", draft.batchFile); add("batch_arg", draft.batchArg); add("batch_nodes", draft.batchNodes);
  add("cpu", draft.cpu); add("memory", draft.memory); add("timeout", draft.timeout);
  add("nodes", draft.nodes); add("tasks_per_node", draft.tasksPerNode); add("launcher", draft.launcher); add("workdir", draft.workdir);
  for (const value of splitLines(draft.environment)) { const [name, rest] = splitPair(value); if (name) add(`env.${name}`, rest); }
  for (const value of splitLines(draft.devices)) { const [name, rest] = splitPair(value); if (name) add(`device.${name}`, rest); }
  for (const value of splitLines(draft.arguments)) add("arg", value);
  return `${lines.join("\n")}\n`;
}

function splitLines(value: string) { return value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean); }
function splitPair(value: string): [string, string] { const index = value.indexOf("="); return index < 1 ? [value.trim(), ""] : [value.slice(0, index).trim(), value.slice(index + 1).trim()]; }
function unquote(value: string) { return value.length > 1 && ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) ? value.slice(1, -1) : value; }
