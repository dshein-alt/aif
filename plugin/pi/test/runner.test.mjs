import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import * as path from "node:path";
import test from "node:test";
import { buildPiArgs, runResident } from "../runner.mjs";

function exampleConfig(stateDir) {
	return {
		version: 2,
		id: "abc123",
		cwd: stateDir,
		stateDir,
		intervalSeconds: 10,
		maxTurns: 1,
		provider: "provider",
		model: "model",
		thinkingLevel: "high",
		mcpConfigPath: path.join(stateDir, "mcp.json"),
		childExtensionPath: "/plugin/pi/resident-child.ts",
		agentName: "resident-test",
		trustedProject: true,
		piInvocation: { command: process.execPath, prefixArgs: ["/fake/pi.js"] },
	};
}

test("buildPiArgs carries explicit provider, model, thinking, MCP config, trust, and session", () => {
	const config = exampleConfig("/tmp/resident-state");
	const args = buildPiArgs(config, 3);
	assert.deepEqual(args.slice(0, 1), ["/fake/pi.js"]);
	assert.equal(args[args.indexOf("--provider") + 1], "provider");
	assert.equal(args[args.indexOf("--model") + 1], "model");
	assert.equal(args[args.indexOf("--thinking") + 1], "high");
	assert.equal(args[args.indexOf("--resident-mcp-config") + 1], "/tmp/resident-state/mcp.json");
	assert.equal(args[args.indexOf("--extension") + 1], "/plugin/pi/resident-child.ts");
	assert.ok(args.includes("--no-extensions"));
	assert.ok(!args.includes("--tools"));
	assert.equal(args[args.indexOf("--session-id") + 1], "abc123");
	assert.ok(args.includes("--approve"));
	assert.equal(args.at(-2), "-p");
	assert.match(args.at(-1), /Resident turn 3/);
});

test("supervisor runs a bounded turn and records final metadata", async () => {
	const stateDir = await mkdtemp(path.join(tmpdir(), "pi-resident-test-"));
	await mkdir(path.join(stateDir, "sessions"));
	for (const name of ["SYSTEM.md", "GOAL.md", "INBOX.md", "journal.md"]) {
		await writeFile(path.join(stateDir, name), `${name}\n`);
	}
	await writeFile(path.join(stateDir, "mcp.json"), JSON.stringify({ mcpServers: {} }));
	const fakePi = path.join(stateDir, "fake-pi.mjs");
	await writeFile(
		fakePi,
		`import { appendFileSync } from "node:fs"; appendFileSync(${JSON.stringify(path.join(stateDir, "observed.txt"))}, process.argv.slice(2).join("\\n"));\n`,
	);
	const config = {
		...exampleConfig(stateDir),
		piInvocation: { command: process.execPath, prefixArgs: [fakePi] },
	};
	const configPath = path.join(stateDir, "config.json");
	await writeFile(configPath, JSON.stringify(config));
	await writeFile(
		path.join(stateDir, "resident.json"),
		JSON.stringify({ id: config.id, pid: process.pid, stateDir, status: "starting", turn: 0 }),
	);

	const result = await runResident(configPath);
	assert.deepEqual(result, { status: "turn-limit", turn: 1 });
	const metadata = JSON.parse(await readFile(path.join(stateDir, "resident.json"), "utf8"));
	assert.equal(metadata.status, "turn-limit");
	assert.equal(metadata.turn, 1);
	const observed = await readFile(path.join(stateDir, "observed.txt"), "utf8");
	assert.match(observed, /--provider\nprovider/);
	assert.match(observed, /--model\nmodel/);
	assert.match(observed, /--resident-mcp-config/);
	await rm(stateDir, { recursive: true, force: true });
});

test("DONE created during a turn terminates the supervisor before sleeping", async () => {
	const stateDir = await mkdtemp(path.join(tmpdir(), "pi-resident-done-test-"));
	await mkdir(path.join(stateDir, "sessions"));
	for (const name of ["SYSTEM.md", "GOAL.md", "INBOX.md", "journal.md"]) {
		await writeFile(path.join(stateDir, name), `${name}\n`);
	}
	await writeFile(path.join(stateDir, "mcp.json"), JSON.stringify({ mcpServers: {} }));
	const fakePi = path.join(stateDir, "fake-pi.mjs");
	await writeFile(
		fakePi,
		`import { writeFileSync } from "node:fs"; writeFileSync(${JSON.stringify(path.join(stateDir, "DONE"))}, "shutdown requested\\n");\n`,
	);
	const config = {
		...exampleConfig(stateDir),
		intervalSeconds: 60,
		maxTurns: 3,
		piInvocation: { command: process.execPath, prefixArgs: [fakePi] },
	};
	const configPath = path.join(stateDir, "config.json");
	await writeFile(configPath, JSON.stringify(config));
	await writeFile(
		path.join(stateDir, "resident.json"),
		JSON.stringify({ id: config.id, pid: process.pid, stateDir, status: "starting", turn: 0 }),
	);

	const result = await runResident(configPath);
	assert.deepEqual(result, { status: "done", turn: 1 });
	const metadata = JSON.parse(await readFile(path.join(stateDir, "resident.json"), "utf8"));
	assert.equal(metadata.status, "done");
	await rm(stateDir, { recursive: true, force: true });
});
