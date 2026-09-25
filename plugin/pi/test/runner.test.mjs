import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import * as path from "node:path";
import test from "node:test";
import { buildPiArgs, runResident } from "../aif-resident-agent/runner.mjs";

function exampleConfig(stateDir) {
	return {
		version: 2,
		id: "abc123",
		cwd: stateDir,
		stateDir,
		intervalSeconds: 1,
		maxTurns: 1,
		provider: "provider",
		model: "model",
		thinkingLevel: "high",
		mcpConfigPath: path.join(stateDir, "mcp.json"),
		childExtensionPath: "/plugin/pi/aif-resident-agent/resident-child.ts",
		agentName: "resident-test",
		trustedProject: true,
		piInvocation: { command: process.execPath, prefixArgs: ["/fake/pi.js"] },
	};
}

function writeMcp(stateDir, url = "http://127.0.0.1:1/mcp") {
	return writeFile(path.join(stateDir, "mcp.json"), JSON.stringify({
		mcpServers: { aif: { url, auth: "bearer", bearerToken: "aif_test", lifecycle: "eager" } },
	}));
}

test("buildPiArgs carries explicit provider, model, thinking, MCP config, trust, and session", () => {
	const config = exampleConfig("/tmp/resident-state");
	const args = buildPiArgs(config, 3);
	assert.deepEqual(args.slice(0, 1), ["/fake/pi.js"]);
	assert.equal(args[args.indexOf("--provider") + 1], "provider");
	assert.ok(args.includes("--no-context-files"));
	assert.equal(args[args.indexOf("--model") + 1], "model");
	assert.equal(args[args.indexOf("--thinking") + 1], "high");
	assert.equal(args[args.indexOf("--resident-mcp-config") + 1], "/tmp/resident-state/mcp.json");
	assert.equal(args[args.indexOf("--extension") + 1], "/plugin/pi/aif-resident-agent/resident-child.ts");
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
	for (const name of ["SYSTEM.md", "GOAL.md", "journal.md"]) {
		await writeFile(path.join(stateDir, name), `${name}\n`);
	}
	await writeMcp(stateDir);
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
	for (const name of ["SYSTEM.md", "GOAL.md", "journal.md"]) {
		await writeFile(path.join(stateDir, name), `${name}\n`);
	}
	await writeMcp(stateDir);
	const fakePi = path.join(stateDir, "fake-pi.mjs");
	await writeFile(
		fakePi,
		`import { writeFileSync } from "node:fs"; writeFileSync(${JSON.stringify(path.join(stateDir, "DONE"))}, "shutdown requested\\n");\n`,
	);
	const config = {
		...exampleConfig(stateDir),
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

test("RESET waits for the child and starts a fresh session immediately", async (t) => {
	const stateDir = await mkdtemp(path.join(tmpdir(), "pi-resident-reset-test-"));
	t.after(() => rm(stateDir, { recursive: true, force: true }));
	await mkdir(path.join(stateDir, "sessions"));
	await writeMcp(stateDir);
	await writeFile(path.join(stateDir, "sessions/old.jsonl"), "old context");
	await writeFile(path.join(stateDir, "GOAL.md"), "keep working");
	const fakePi = path.join(stateDir, "fake-pi.mjs");
	await writeFile(fakePi, `
import * as fs from 'node:fs';
import * as path from 'node:path';
const dir = process.env.PI_RESIDENT_STATE_DIR;
const turnFile = path.join(dir, 'turn-number');
const turn = fs.existsSync(turnFile) ? 2 : 1;
fs.writeFileSync(turnFile, String(turn));
const session = process.argv[process.argv.indexOf('--session-id') + 1];
fs.appendFileSync(path.join(dir, 'seen-sessions'), session + '\\n');
if (turn === 1) {
  fs.writeFileSync(path.join(dir, 'RESET'), 'requested');
  await new Promise(resolve => setTimeout(resolve, 15));
  if (!fs.existsSync(path.join(dir, 'sessions/old.jsonl'))) process.exit(3);
  fs.writeFileSync(path.join(dir, 'journal.md'), 'written after reset request');
  fs.writeFileSync(path.join(dir, 'DONE'), 'old task completion');
} else {
  fs.writeFileSync(path.join(dir, 'reset-observed.json'), JSON.stringify({
    journal: fs.readFileSync(path.join(dir, 'journal.md'), 'utf8'),
    oldSession: fs.existsSync(path.join(dir, 'sessions/old.jsonl')),
    done: fs.existsSync(path.join(dir, 'DONE')),
    goal: fs.readFileSync(path.join(dir, 'GOAL.md'), 'utf8')
  }));
  fs.writeFileSync(path.join(dir, 'DONE'), 'finished new session');
}
`);
	const config = { ...exampleConfig(stateDir), maxTurns: 2,
		piInvocation: { command: process.execPath, prefixArgs: [fakePi] } };
	const configPath = path.join(stateDir, "config.json");
	await writeFile(configPath, JSON.stringify(config));
	const result = await runResident(configPath);
	assert.deepEqual(result, { status: "done", turn: 2 });
	const sessions = (await readFile(path.join(stateDir, "seen-sessions"), "utf8")).trim().split("\n");
	assert.equal(sessions[0], config.id);
	assert.notEqual(sessions[1], sessions[0]);
	const observed = JSON.parse(await readFile(path.join(stateDir, "reset-observed.json"), "utf8"));
	assert.equal(observed.oldSession, false);
	assert.equal(observed.done, false);
	assert.equal(observed.goal, "keep working");
	assert.doesNotMatch(observed.journal, /written after/);
	assert.equal(JSON.parse(await readFile(path.join(stateDir, "resident.json"))).lastExitCode, 0);
});

test("the supervisor long-polls AIF itself and runs a turn only when the inbox grew", async (t) => {
	const stateDir = await mkdtemp(path.join(tmpdir(), "pi-resident-poll-test-"));
	t.after(() => rm(stateDir, { recursive: true, force: true }));
	await mkdir(path.join(stateDir, "sessions"));
	const polls = [];
	// Turn 1 is unconditional; then n:1 with a fresh seq must start turn 2, and the same seq again
	// (a message left unread) must not start turn 3.
	const answers = [{ n: 1, seq: 7 }, { n: 1, seq: 7 }, { n: 1, seq: 7 }];
	const server = createServer((request, response) => {
		polls.push({ url: request.url, auth: request.headers.authorization });
		response.setHeader("content-type", "application/json");
		response.end(JSON.stringify({ ...(answers.shift() ?? { n: 0, seq: 7 }), men: 0, cursor: 0 }));
	});
	await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
	t.after(() => server.close());
	await writeMcp(stateDir, `http://127.0.0.1:${server.address().port}/mcp`);
	const fakePi = path.join(stateDir, "fake-pi.mjs");
	await writeFile(fakePi, `import { appendFileSync } from "node:fs";
appendFileSync(${JSON.stringify(path.join(stateDir, "prompts.txt"))}, process.argv.at(-1) + "\\n");`);
	const config = { ...exampleConfig(stateDir), maxTurns: 3,
		piInvocation: { command: process.execPath, prefixArgs: [fakePi] } };
	const configPath = path.join(stateDir, "config.json");
	await writeFile(configPath, JSON.stringify(config));

	const run = runResident(configPath);
	// Nothing new after turn 2: the supervisor sits in poll/backoff, so stop it with the same signal
	// the extension sends. (A SIGTERM sent before turn 2 would be a bug in this test's timing.)
	await new Promise((resolve) => {
		const check = () => {
			if (existsSync(path.join(stateDir, "turn-0002.jsonl")) && polls.length >= 3) resolve();
			else setTimeout(check, 20);
		};
		check();
	});
	process.emit("SIGTERM");
	const result = await run;
	assert.equal(result.turn, 2, "the unchanged seq must not have triggered a third turn");
	assert.equal(result.status, "stopped");
	const prompts = (await readFile(path.join(stateDir, "prompts.txt"), "utf8")).trim().split("\n");
	assert.match(prompts[0], /Resident turn 1, woken because first turn/);
	assert.match(prompts[1], /Resident turn 2, woken because AIF reports unread messages/);
	assert.equal(polls[0].auth, "Bearer aif_test");
	assert.match(polls[0].url, /^\/api\/poll\?wait=1$/, "the poll must long-poll for the interval and never pass advance");
});
