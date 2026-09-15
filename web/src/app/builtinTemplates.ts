import type { Language } from "./preferences";
import type { JobTemplate } from "../api/contracts";

// A job template's name and description live in the control-plane database, so
// the console cannot translate them the way it translates its own copy. The
// built-in templates are the exception worth handling: XIRANG ships them
// itself, under names that internal/jobtemplate/builtins.go and this table both
// know, so they can be presented in the reader's language from a lookup keyed
// on the canonical English name.
//
// Anything an administrator wrote is shown exactly as written. A cluster's own
// templates carry whatever language their author chose, and guessing at a
// translation for them would be worse than leaving them alone.
//
// `en` is the description builtins.go publishes. It is here so an administrator
// who rewrites a built-in's description keeps their text instead of having the
// shipped translation put back over it. scripts/audit-builtin-templates.mjs
// fails the build if either language drifts from the Go definitions.
type BuiltinTemplate = { en: string; zh: { name: string; description: string } };

const BUILTIN_TEMPLATES: Record<string, BuiltinTemplate> = {
  "CPU pi calculation": {
    en: "Runs the Leibniz series across 4 parallel AWK processes and checks the value of pi. Adjust the last two arg lines to change the process count and the iterations per process.",
    zh: {
      name: "CPU 圆周率计算",
      description: "使用 4 个并行 AWK 计算进程执行 Leibniz 级数并校验圆周率结果；可修改最后两个 arg 调整进程数和每进程迭代次数。",
    },
  },
  "CUDA smoke test": {
    en: "Claims one NVIDIA GPU, then compiles and runs a CUDA vector addition with nvcc to check device enumeration, memory transfer and the kernel result.",
    zh: {
      name: "CUDA 基础测试",
      description: "申请 1 张 NVIDIA GPU，使用 nvcc 编译并运行 CUDA 向量加法，同时验证设备枚举、内存传输和核函数结果。",
    },
  },
  "ARM64 node acceptance": {
    en: "Schedules only onto ARM64 nodes and checks the architecture, kernel, CPU count, instruction set, memory, integer arithmetic and whether the job working directory is writable.",
    zh: {
      name: "ARM64 节点验收",
      description: "仅调度到 ARM64 节点，检查架构、内核、CPU 数量、指令集、内存、整数计算和任务工作目录写入能力。",
    },
  },
};

/** The template name to display, translated when XIRANG shipped the template. */
export function templateName(template: JobTemplate, language: Language): string {
  const builtin = BUILTIN_TEMPLATES[template.name.trim()];
  return builtin && language === "zh" ? builtin.zh.name : template.name;
}

/** The template description to display, under the same rule. */
export function templateDescription(template: JobTemplate, language: Language): string {
  const description = template.description ?? "";
  const builtin = BUILTIN_TEMPLATES[template.name.trim()];
  if (!builtin || language !== "zh") return description;
  // An administrator who rewrote the description owns it now.
  return description.trim() === builtin.en.trim() ? builtin.zh.description : description;
}
