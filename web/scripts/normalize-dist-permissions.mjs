import { chmod, lstat, readdir } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../dist", import.meta.url));

async function normalize(path) {
  const info = await lstat(path);
  if (info.isSymbolicLink()) return;
  if (!info.isDirectory()) {
    await chmod(path, 0o644);
    return;
  }
  await chmod(path, 0o755);
  const entries = await readdir(path);
  await Promise.all(entries.map((entry) => normalize(join(path, entry))));
}

await normalize(root);
