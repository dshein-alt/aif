import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { tmpdir } from "node:os";
import { createRequire } from "node:module";
import { pathToFileURL, fileURLToPath } from "node:url";
import test from "node:test";

// Use the host's loader: raw jiti does not supply pi's typebox/SDK aliases.
const piEntry = fileURLToPath(import.meta.resolve("@earendil-works/pi-coding-agent"));
const piRequire = createRequire(piEntry);
const { Compile } = await import(pathToFileURL(piRequire.resolve("typebox/compile")).href);
const { loadExtensions } = await import(pathToFileURL(path.join(path.dirname(piEntry), "core/extensions/loader.js")).href);

test("pi loads the nested entrypoints and validates resident tool schemas", async (t) => {
	const dir = fs.mkdtempSync(path.join(tmpdir(), "pi-loader-test-"));
	const oldArgs = process.argv;
	const oldDir = process.env.PI_RESIDENT_STATE_DIR;
	t.after(() => {
		process.argv = oldArgs;
		if (oldDir === undefined) delete process.env.PI_RESIDENT_STATE_DIR;
		else process.env.PI_RESIDENT_STATE_DIR = oldDir;
		fs.rmSync(dir, { recursive: true, force: true });
	});
	const mcp = path.join(dir, "mcp.json");
	// Lazy adapter avoids opening a network connection in this loader test.
	fs.writeFileSync(mcp, JSON.stringify({ mcpServers: { aif: {
		url: "http://127.0.0.1:1/mcp", auth: "bearer", bearerToken: "test", lifecycle: "lazy",
	} } }));
	process.argv = [process.execPath, "loader-test", "--resident-mcp-config", mcp];
	process.env.PI_RESIDENT_STATE_DIR = dir;
	for (const name of ["index.ts", "resident-child.ts"]) {
		const filename = fileURLToPath(new URL(`../aif-resident-agent/${name}`, import.meta.url));
		const result = await loadExtensions([filename], dir);
		assert.deepEqual(result.errors, []);
		assert.equal(result.extensions.length, 1);
		if (name === "resident-child.ts") {
			const tools = result.extensions[0].tools;
			const inbox = Compile(tools.get("resident_inbox").definition.parameters);
			assert.equal(inbox.Check({ action: "read" }), true);
			assert.equal(inbox.Check({ action: "delete-everything" }), false);
			const memory = Compile(tools.get("resident_memory").definition.parameters);
			assert.equal(memory.Check({ action: "journal", text: "checkpoint" }), true);
			assert.equal(memory.Check({ action: "reset" }), true);
		}
	}
});
