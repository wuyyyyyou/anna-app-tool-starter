import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const json = (path) => JSON.parse(readFileSync(new URL(`../${path}`, import.meta.url), "utf8"));
const app = json("app.json");
const manifest = json("manifest.json");
const executa = json("executas/type-demo/executa.json");
const protocol = json("executas/type-demo/manifest.json");

assert.equal(app.bundled_executas["type-demo"].path, "executas/type-demo");
assert.equal(manifest.required_executas[0].tool_id, "bundled:type-demo");
assert.deepEqual(manifest.ui.host_api.tools, ["required:bundled:type-demo"]);
assert.equal(executa.version, protocol.version);
assert.equal(manifest.required_executas[0].min_version, protocol.version);
assert.equal(executa.name, protocol.display_name);
assert.equal(executa.description, protocol.description);
assert.equal(executa.distribution.type, "binary");
assert.equal(executa.distribution.executable_name, protocol.name);
assert.deepEqual(protocol.host_capabilities, []);
assert.equal(protocol.tools.length, 1);
assert.deepEqual(protocol.tools[0].parameters.map((p) => p.type), ["string", "integer", "number", "boolean", "array", "object"]);
assert.equal(protocol.tools[0].parameters.find((p) => p.type === "array").items_type, "string");

function run(command, args) {
  const result = spawnSync(command, args, { cwd: root, stdio: "inherit" });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
}

console.log("Passed: App / Executa configuration and six parameter types");
run("go", ["-C", "executas/type-demo", "test", "./..."]);
const cli = fileURLToPath(import.meta.resolve("@anna-ai/cli"));
run(process.execPath, [cli, "validate", "--strict"]);
const buildArgs = ["run", "scripts/build-binary.go", "--check"];
if (process.argv.includes("--all")) buildArgs.push("--all");
run("go", buildArgs);
console.log("All checks passed.");
