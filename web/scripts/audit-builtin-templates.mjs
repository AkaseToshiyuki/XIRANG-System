// The console localises XIRANG's own job templates from a table keyed on the
// English name that internal/jobtemplate/builtins.go publishes. Two files have
// to agree for that to work, and nothing about editing one of them prompts you
// to open the other: rename a built-in in Go and the console silently stops
// translating it, because the lookup misses and the row is treated as an
// administrator's own template. This compares the two and fails the build on
// any drift.
import fs from "node:fs";
import path from "node:path";
import ts from "../node_modules/typescript/lib/typescript.js";

const goFile = path.resolve("../internal/jobtemplate/builtins.go");
const tsFile = path.resolve("src/app/builtinTemplates.ts");
const failures = [];

// Go string literals in these definitions are interpreted (double-quoted) and
// carry no escapes beyond the ordinary ones, so a JSON parse reads them exactly.
function goDefinitions(source) {
  const block = source.match(/var builtinDefinitions = \[\]builtinDefinition\{([\s\S]*?)\n\}/);
  if (!block) throw new Error("could not find builtinDefinitions in builtins.go");
  const definitions = [];
  for (const entry of block[1].split(/\n\t\{\n/).slice(1)) {
    const field = (name) => {
      const found = entry.match(new RegExp(`\\b${name}:\\s*("(?:[^"\\\\]|\\\\.)*")`));
      return found ? JSON.parse(found[1]) : undefined;
    };
    definitions.push({
      name: field("name"),
      description: field("description"),
      legacyName: field("legacyName"),
      legacyDescription: field("legacyDescription"),
    });
  }
  return definitions;
}

function consoleTable(source) {
  const file = ts.createSourceFile(tsFile, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  let literal;
  const visit = (node) => {
    if (ts.isVariableDeclaration(node) && node.name.getText() === "BUILTIN_TEMPLATES") {
      literal = node.initializer;
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  if (!literal || !ts.isObjectLiteralExpression(literal)) throw new Error("could not find BUILTIN_TEMPLATES in builtinTemplates.ts");
  const read = (object, key) => {
    const property = object.properties.find((p) => p.name && p.name.getText().replace(/^["']|["']$/g, "") === key);
    return property?.initializer;
  };
  const table = {};
  for (const property of literal.properties) {
    const key = property.name.getText().replace(/^["']|["']$/g, "");
    const zh = read(property.initializer, "zh");
    table[key] = {
      en: read(property.initializer, "en")?.text,
      zhName: zh ? read(zh, "name")?.text : undefined,
      zhDescription: zh ? read(zh, "description")?.text : undefined,
    };
  }
  return table;
}

const definitions = goDefinitions(fs.readFileSync(goFile, "utf8"));
const table = consoleTable(fs.readFileSync(tsFile, "utf8"));

for (const definition of definitions) {
  const entry = table[definition.name];
  if (!entry) {
    failures.push(`builtins.go publishes "${definition.name}" but builtinTemplates.ts has no entry for it, so the console cannot translate it`);
    continue;
  }
  if (entry.en !== definition.description) {
    failures.push(`"${definition.name}": the en description in builtinTemplates.ts does not match builtins.go, so an untouched template reads as administrator-edited and keeps its English text`);
  }
  if (definition.legacyName && entry.zhName !== definition.legacyName) {
    failures.push(`"${definition.name}": zh.name is ${JSON.stringify(entry.zhName)} but builtins.go records the shipped Chinese name as ${JSON.stringify(definition.legacyName)}`);
  }
  if (definition.legacyDescription && entry.zhDescription !== definition.legacyDescription) {
    failures.push(`"${definition.name}": zh.description does not match the shipped Chinese description in builtins.go`);
  }
}
for (const name of Object.keys(table)) {
  if (!definitions.some((definition) => definition.name === name)) {
    failures.push(`builtinTemplates.ts translates "${name}", which builtins.go no longer publishes`);
  }
}

if (failures.length) {
  console.error("Built-in job template metadata is out of sync:\n  " + failures.join("\n  "));
  process.exit(1);
}
console.log(`built-in template audit passed: ${definitions.length} templates match between builtins.go and the console`);
