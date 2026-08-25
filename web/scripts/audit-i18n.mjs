import fs from "node:fs";
import path from "node:path";
import ts from "../node_modules/typescript/lib/typescript.js";

const sourceRoot = path.resolve("src");
const failures = [];

function filesIn(directory) {
  return fs.readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const target = path.join(directory, entry.name);
    return entry.isDirectory() ? filesIn(target) : /\.tsx?$/.test(entry.name) ? [target] : [];
  });
}

function isTranslated(node) {
  for (let current = node; current; current = current.parent) {
    if (ts.isCallExpression(current) && ts.isIdentifier(current.expression) && current.expression.text === "t") return true;
    if (ts.isPropertyAssignment(current) && current.name.getText() === "zh") return true;
    if (ts.isVariableDeclaration(current) && /ZH$/.test(current.name.getText())) return true;
    if ((ts.isConditionalExpression(current) || ts.isBinaryExpression(current)) && /\b(?:language\s*===\s*["']zh["']|zh)\b/.test((current.condition ?? current).getText())) return true;
    if (ts.isJsxElement(current) && current.openingElement.tagName.getText() === "option" && current.openingElement.attributes.getText().includes('value="zh"')) return true;
  }
  return false;
}

for (const file of filesIn(sourceRoot)) {
  const text = fs.readFileSync(file, "utf8");
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  function inspect(node) {
    const value = ts.isStringLiteralLike(node) || ts.isJsxText(node) ? node.text : "";
    if (/\p{Script=Han}/u.test(value) && !isTranslated(node)) {
      const { line } = source.getLineAndCharacterOfPosition(node.getStart(source));
      failures.push(`${path.relative(process.cwd(), file)}:${line + 1}: ${value.trim().slice(0, 100)}`);
    }
    ts.forEachChild(node, inspect);
  }
  inspect(source);
}

if (failures.length) {
  console.error("Untranslated Chinese UI strings found:\n" + failures.join("\n"));
  process.exit(1);
}
console.log("i18n audit passed: no unguarded Chinese UI strings");
