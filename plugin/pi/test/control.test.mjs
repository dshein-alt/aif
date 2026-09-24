import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";
import { spawnSync } from "node:child_process";
import { acknowledgeMessage, enqueueMessage, readInbox, writeJournal, boundJournal, JOURNAL_MAX_BYTES, JOURNAL_HEADER, requestControl, applyReset } from "../aif-resident-agent/control.mjs";
import { registerResidentTools } from "../aif-resident-agent/resident-tools.mjs";

function fixture(t) {
	const dir = fs.mkdtempSync(path.join(tmpdir(), "pi-control-test-"));
	t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
	return dir;
}

test("inbox preserves unacknowledged work and concurrent arrivals, ack is bounded to claimed IDs", (t) => {
	const dir = fixture(t);
	const first = enqueueMessage(dir, "first");
	assert.equal(readInbox(dir).messages[0].id, first);
	const second = enqueueMessage(dir, "second");
	acknowledgeMessage(dir, second); // cannot acknowledge an unread message
	acknowledgeMessage(dir, first);
	assert.deepEqual(readInbox(dir).messages, [{ id: second, text: "second\n" }]);
	assert.deepEqual(readInbox(dir).messages, [{ id: second, text: "second\n" }]);
	assert.throws(() => acknowledgeMessage(dir, "../../GOAL.md"), /invalid/);
	acknowledgeMessage(dir, second);
	acknowledgeMessage(dir, second);
	assert.deepEqual(readInbox(dir).messages, []);
});

test("queue batches and message size are bounded; partial files stay invisible", (t) => {
	const dir = fixture(t);
	for (let i = 0; i < 20; i++) enqueueMessage(dir, `message ${i}`);
	fs.writeFileSync(path.join(dir, "inbox/pending/incomplete.tmp"), "partial");
	const batch = readInbox(dir);
	assert.equal(batch.messages.length, 16);
	assert.equal(batch.remaining, 4);
	for (const item of batch.messages) acknowledgeMessage(dir, item.id);
	assert.equal(readInbox(dir).messages.length, 4);
	assert.throws(() => enqueueMessage(dir, "x".repeat(16385)), /16 KiB/);
});

test("working memory rejects oversized updates and bounds direct edits", (t) => {
	const dir = fixture(t);
	writeJournal(dir, "important checkpoint");
	assert.throws(() => writeJournal(dir, "é".repeat(JOURNAL_MAX_BYTES)), /summarize/);
	assert.equal(fs.readFileSync(path.join(dir, "journal.md"), "utf8"), "important checkpoint");
	fs.writeFileSync(path.join(dir, "journal.md"), "old\n".repeat(20000) + "next step\n");
	assert.equal(boundJournal(dir), true);
	assert.ok(fs.statSync(path.join(dir, "journal.md")).size <= JOURNAL_MAX_BYTES);
	assert.match(fs.readFileSync(path.join(dir, "journal.md"), "utf8"), /next step/);
	assert.equal(boundJournal(dir), false);
});

test("reset drops session and journal but preserves identity, goal and claimed queue", (t) => {
	const dir = fixture(t);
	fs.mkdirSync(path.join(dir, "sessions"));
	fs.writeFileSync(path.join(dir, "sessions/old.jsonl"), "old conversation");
	for (const name of ["GOAL.md", "SYSTEM.md", "HOME_THREAD", "mcp.json", "DONE", "BLOCKED", "context.json"]) fs.writeFileSync(path.join(dir, name), name);
	writeJournal(dir, "old summary");
	const id = enqueueMessage(dir, "unfinished request");
	readInbox(dir);
	const config = { stateDir: dir, id: "resident" };
	requestControl(dir, "RESET");
	assert.equal(applyReset(config), true);
	assert.notEqual(config.sessionId, config.id);
	assert.deepEqual(fs.readdirSync(path.join(dir, "sessions")), []);
	assert.equal(fs.readFileSync(path.join(dir, "journal.md"), "utf8"), JOURNAL_HEADER);
	for (const name of ["DONE", "BLOCKED", "context.json", "RESET"]) assert.equal(fs.existsSync(path.join(dir, name)), false);
	for (const name of ["GOAL.md", "SYSTEM.md", "HOME_THREAD", "mcp.json"]) assert.equal(fs.readFileSync(path.join(dir, name), "utf8"), name);
	assert.equal(readInbox(dir).messages[0].id, id);
	assert.equal(applyReset(config), false);
});

test("child telemetry reports native overflow, compaction failure and recovery", async (t) => {
	const dir = fixture(t);
	const handlers = {}, tools = {};
	registerResidentTools({ on: (name, fn) => { handlers[name] = fn; }, registerTool: (tool) => { tools[tool.name] = tool; } }, dir);
	const ctx = { getContextUsage: () => ({ tokens: 9500, contextWindow: 10000, percent: 95 }), sessionManager: { getSessionId: () => "session" } };
	await handlers.session_start({}, ctx);
	handlers.session_before_compact({ reason: "overflow" }, ctx);
	handlers.session_compact_failed({ reason: "overflow", errorMessage: "provider unavailable", aborted: false }, ctx);
	let status = JSON.parse(fs.readFileSync(path.join(dir, "context.json")));
	assert.equal(status.overflows, 1);
	assert.equal(status.compaction, "failed");
	assert.equal(status.error, "provider unavailable");
	handlers.session_compact({ reason: "manual" }, ctx);
	status = JSON.parse(fs.readFileSync(path.join(dir, "context.json")));
	assert.equal(status.error, null);
	assert.equal(status.compactions, 1);
	assert.equal(status.overflows, 1);
	await tools.resident_memory.execute("call", { action: "journal", text: "checkpoint" });
	await tools.resident_memory.execute("call", { action: "reset" });
	assert.equal(fs.readFileSync(path.join(dir, "journal.md"), "utf8"), "checkpoint"); // request only
	assert.ok(fs.existsSync(path.join(dir, "RESET")));
	const cli = spawnSync(process.execPath, [new URL("../aif-resident-agent/residentctl.mjs", import.meta.url).pathname, "reset", "missing"], { encoding: "utf8" });
	assert.notEqual(cli.status, 0);
});

test("manual compaction completes before startup handler returns; errors remain observable", async (t) => {
	const dir = fixture(t);
	const handlers = {};
	registerResidentTools({ on: (name, fn) => { handlers[name] = fn; }, registerTool() {} }, dir);
	requestControl(dir, "COMPACT");
	let completed = false;
	await handlers.session_start({}, {
		getContextUsage: () => undefined,
		sessionManager: { getSessionId: () => "session" },
		compact: ({ onError }) => setTimeout(() => { completed = true; onError(new Error("nothing to compact")); }, 5),
	});
	assert.equal(completed, true);
	assert.equal(fs.existsSync(path.join(dir, "COMPACT")), false);
	assert.match(JSON.parse(fs.readFileSync(path.join(dir, "context.json"))).error, /nothing to compact/);
});
