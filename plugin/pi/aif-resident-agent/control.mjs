import * as fs from "node:fs";
import * as path from "node:path";
import { randomUUID } from "node:crypto";

export const JOURNAL_MAX_BYTES = 16 * 1024;
export const MESSAGE_MAX_BYTES = 16 * 1024;
export const JOURNAL_HEADER = "# Resident working memory\n\n";
const messageID = /^\d{17}-[0-9a-f-]{36}\.md$/;

export function atomicWrite(file, text) {
	const temporary = `${file}.${randomUUID()}.tmp`;
	try {
		fs.writeFileSync(temporary, text, { mode: 0o600, flag: "wx" });
		fs.renameSync(temporary, file);
	} finally {
		fs.rmSync(temporary, { force: true });
	}
}

export function initInbox(stateDir) {
	for (const name of ["pending", "processing"]) {
		fs.mkdirSync(path.join(stateDir, "inbox", name), { recursive: true, mode: 0o700 });
	}
}

export function enqueueMessage(stateDir, text) {
	if (!text.trim()) throw new Error("wake requires a message");
	if (Buffer.byteLength(text) > MESSAGE_MAX_BYTES) throw new Error("message exceeds 16 KiB");
	initInbox(stateDir);
	const id = `${new Date().toISOString().replace(/\D/g, "")}-${randomUUID()}.md`;
	atomicWrite(path.join(stateDir, "inbox", "pending", id), text.trim() + "\n");
	return id;
}

// One consumer per resident. Claimed messages survive child crashes until explicitly acknowledged.
export function readInbox(stateDir) {
	initInbox(stateDir);
	const entries = [];
	for (const dir of ["processing", "pending"]) {
		for (const id of fs.readdirSync(path.join(stateDir, "inbox", dir))) {
			if (messageID.test(id)) entries.push({ id, dir });
		}
	}
	entries.sort((a, b) => a.id.localeCompare(b.id));
	const messages = [];
	let bytes = 0;
	for (const { id, dir } of entries) {
		if (messages.length >= 16) break;
		const source = path.join(stateDir, "inbox", dir, id);
		const size = fs.statSync(source).size;
		if (size > MESSAGE_MAX_BYTES + 1) throw new Error(`oversized inbox message: ${id}`);
		if (bytes + size > 32 * 1024 && messages.length) break;
		const target = path.join(stateDir, "inbox", "processing", id);
		if (dir === "pending") fs.renameSync(source, target);
		messages.push({ id, text: fs.readFileSync(target, "utf8") });
		bytes += size;
	}
	return { messages, remaining: entries.length - messages.length };
}

export function acknowledgeMessage(stateDir, id) {
	if (!messageID.test(id)) throw new Error("invalid message ID");
	// Idempotent acknowledgement cannot remove a message still waiting to be read.
	fs.rmSync(path.join(stateDir, "inbox", "processing", id), { force: true });
}

export function writeJournal(stateDir, text) {
	if (Buffer.byteLength(text) > JOURNAL_MAX_BYTES) throw new Error("journal exceeds 16 KiB; summarize it before replacing");
	atomicWrite(path.join(stateDir, "journal.md"), text);
}

// Backstop for direct file edits, run only after the child exits.
export function boundJournal(stateDir) {
	const file = path.join(stateDir, "journal.md");
	if (!fs.existsSync(file) || fs.statSync(file).size <= JOURNAL_MAX_BYTES) return false;
	const buffer = Buffer.alloc(JOURNAL_MAX_BYTES - 256);
	const fd = fs.openSync(file, "r");
	let length;
	try {
		length = fs.readSync(fd, buffer, 0, buffer.length, fs.fstatSync(fd).size - buffer.length);
	} finally {
		fs.closeSync(fd);
	}
	let tail = buffer.subarray(0, length).toString("utf8");
	const newline = tail.indexOf("\n");
	if (newline >= 0) tail = tail.slice(newline + 1);
	const bounded = JOURNAL_HEADER + "Older content removed by the size guard. Consolidate the retained notes.\n\n" + tail;
	writeJournal(stateDir, bounded);
	return true;
}

export function requestControl(stateDir, command) {
	if (!["RESET", "COMPACT"].includes(command)) throw new Error("unknown resident control command");
	atomicWrite(path.join(stateDir, command), `${new Date().toISOString()}\n`);
}

export function readContextStatus(stateDir) {
	try { return JSON.parse(fs.readFileSync(path.join(stateDir, "context.json"), "utf8")); }
	catch (error) { if (error.code === "ENOENT") return {}; throw error; }
}

export function contextDescription(stateDir) {
	const info = readContextStatus(stateDir);
	return `context=${info.percent == null ? "unknown" : `${Math.round(info.percent)}%`} compaction=${info.compaction ?? "none"} overflows=${info.overflows ?? 0}${info.error ? ` error=${JSON.stringify(info.error)}` : ""}`;
}

// Called only at a child-process boundary. Identity, goal, role, home thread and queue survive.
export function applyReset(config) {
	const marker = path.join(config.stateDir, "RESET");
	if (!fs.existsSync(marker)) return false;
	// Claim this request first so another RESET arriving during cleanup is not lost.
	const claimed = path.join(config.stateDir, "RESET.processing");
	fs.renameSync(marker, claimed);
	config.sessionId = randomUUID();
	atomicWrite(path.join(config.stateDir, "session.json"), JSON.stringify({ id: config.sessionId }));
	fs.rmSync(path.join(config.stateDir, "sessions"), { recursive: true, force: true });
	fs.mkdirSync(path.join(config.stateDir, "sessions"), { mode: 0o700 });
	writeJournal(config.stateDir, JOURNAL_HEADER);
	for (const name of ["DONE", "BLOCKED", "COMPACT", "context.json"]) {
		fs.rmSync(path.join(config.stateDir, name), { force: true });
	}
	fs.rmSync(claimed, { force: true });
	return true;
}
