// The light theme used to be maintained by chasing dark-theme colour literals
// with per-selector overrides, which is why --gold spent several releases with
// no light value at all and 78 rules painted dark backgrounds onto a white
// page. The stylesheet is now a token vocabulary plus one swap, and this keeps
// it that way: it fails on a literal outside the token blocks, on a token that
// exists in one theme and not the other, and on any pairing that cannot be read.
//
// Contrast is checked against WCAG AA (4.5:1 for body text). This is a static
// check on the token definitions; e2e/console.spec.ts measures what the browser
// actually composites, which is what catches a legible token used on a ground
// it was never meant for.
import fs from "node:fs";
import path from "node:path";

const file = path.resolve("src/app/styles.css");
const css = fs.readFileSync(file, "utf8");
const withoutComments = css.replace(/\/\*[\s\S]*?\*\//g, "");
const failures = [];

const rules = [];
for (const match of withoutComments.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
  rules.push({ selector: match[1].trim().replace(/\s+/g, " "), body: match[2] });
}

const LIGHT_BLOCK = 'html[data-theme="light"]';
const tokensOf = (selector) => {
  const rule = rules.find((candidate) => candidate.selector === selector);
  if (!rule) throw new Error(`missing token block: ${selector}`);
  return Object.fromEntries([...rule.body.matchAll(/(--[\w-]+)\s*:\s*([^;]+)/g)].map((m) => [m[1], m[2].trim()]));
};
const dark = tokensOf(":root");
const light = tokensOf(LIGHT_BLOCK);

// ---------------------------------------------------------------- parity ---
// Fonts are not theme-dependent; every other token has to exist in both.
const themeless = new Set(["--font-sans", "--font-mono"]);
for (const name of Object.keys(dark)) {
  if (!themeless.has(name) && !(name in light)) failures.push(`${name} is defined for the dark theme but not the light one, so it keeps its dark value on a light page`);
}
for (const name of Object.keys(light)) {
  if (!(name in dark)) failures.push(`${name} is defined for the light theme only`);
}

// -------------------------------------------------------------- contrast ---
const parse = (value) => {
  const text = value.trim();
  if (text.startsWith("#")) {
    let hex = text.slice(1);
    if (hex.length === 3) hex = [...hex].map((c) => c + c).join("");
    if (hex.length !== 6 && hex.length !== 8) return null;
    return [0, 2, 4].map((i) => parseInt(hex.slice(i, i + 2), 16));
  }
  const parts = text.match(/rgba?\(([^)]*)\)/);
  if (!parts) return null;
  const numbers = parts[1].split(/[,\s/]+/).filter(Boolean).map(Number);
  return numbers.length >= 3 && numbers.slice(0, 3).every((n) => !Number.isNaN(n)) ? numbers.slice(0, 3) : null;
};
const channel = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
const luminance = (rgb) => 0.2126 * channel(rgb[0]) + 0.7152 * channel(rgb[1]) + 0.0722 * channel(rgb[2]);
const contrast = (a, b) => {
  const [high, low] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (high + 0.05) / (low + 0.05);
};

// Tokens that carry text, and the grounds a page can put them on.
const TEXT_TOKENS = ["--text", "--text-soft", "--muted", "--cyan", "--violet", "--amber", "--gold", "--green", "--red"];
const GROUNDS = ["--bg", "--surface", "--surface-raised", "--surface-sunken"];
// Foreground/background pairs the stylesheet relies on by construction.
const PAIRS = [
  ["--primary-fg", "--cyan"], ["--primary-fg", "--violet"], ["--primary-fg", "--amber"],
  ["--primary-fg", "--green"], ["--primary-fg", "--red"], ["--primary-fg", "--gold"],
  ["--danger-fg", "--danger-bg"], ["--danger-fg", "--danger-hover"],
  ["--violet-fg", "--violet-bg"], ["--violet-fg", "--violet-hover"],
  ["--code-fg", "--code-bg"], ["--cyan", "--cyan-soft"], ["--text-soft", "--track"], ["--muted", "--track"],
];
const MINIMUM = 4.5;

for (const [themeName, tokens] of [["dark", dark], ["light", light]]) {
  const colour = (name) => {
    const raw = tokens[name] ?? dark[name];
    const rgb = raw ? parse(raw) : null;
    if (!rgb) failures.push(`${themeName}: ${name} is not a colour this check can read (${raw})`);
    return rgb;
  };
  for (const token of TEXT_TOKENS) {
    const foreground = colour(token);
    if (!foreground) continue;
    for (const groundName of GROUNDS) {
      const ground = colour(groundName);
      if (!ground) continue;
      const ratio = contrast(foreground, ground);
      if (ratio < MINIMUM) failures.push(`${themeName}: ${token} on ${groundName} is ${ratio.toFixed(2)}:1, below the ${MINIMUM}:1 needed for body text`);
    }
  }
  for (const [foregroundName, backgroundName] of PAIRS) {
    const foreground = colour(foregroundName);
    const background = colour(backgroundName);
    if (!foreground || !background) continue;
    const ratio = contrast(foreground, background);
    if (ratio < MINIMUM) failures.push(`${themeName}: ${foregroundName} on ${backgroundName} is ${ratio.toFixed(2)}:1, below ${MINIMUM}:1`);
  }
}

// --------------------------------------------------------------- literals ---
// A mask needs an opaque paint rather than a themed one, so it is the one
// literal allowed to stand.
const ALLOWED = [/mask-image\s*:/];
const colourLiteral = /#[0-9a-fA-F]{3,8}\b|\brgba?\(\s*[\d.\s,%/]+\)/g;
for (const rule of rules) {
  if (rule.selector === ":root" || rule.selector === LIGHT_BLOCK || rule.selector.startsWith("@")) continue;
  for (const declaration of rule.body.split(";")) {
    if (ALLOWED.some((pattern) => pattern.test(declaration))) continue;
    for (const literal of declaration.match(colourLiteral) ?? []) {
      failures.push(`${rule.selector.slice(0, 60)}: hardcoded ${literal} in "${declaration.trim().slice(0, 70)}" -- it cannot follow the theme swap`);
    }
  }
}

// ------------------------------------------------------------- pure swap ---
for (const rule of rules) {
  if (rule.selector.includes('data-theme="light"') && rule.selector !== LIGHT_BLOCK) {
    failures.push(`${rule.selector.slice(0, 70)}: the light theme is a token swap; a per-selector override means some rule above it is not using tokens`);
  }
}

if (failures.length) {
  console.error(`Theme audit failed (${failures.length}):\n  ` + failures.join("\n  "));
  process.exit(1);
}
console.log(`theme audit passed: ${Object.keys(light).length} tokens swap cleanly, every text pairing clears ${MINIMUM}:1, no colour literals outside the token blocks`);
