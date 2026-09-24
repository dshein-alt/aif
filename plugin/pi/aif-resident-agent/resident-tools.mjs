import * as fs from "node:fs";
import * as path from "node:path";
import {
	acknowledgeMessage, atomicWrite, readInbox, writeJournal, requestControl, readContextStatus,
} from "./control.mjs";

const textResult = (value) => ({ content: [{ type: "text", text: JSON.stringify(value) }], details: value });

export function registerResidentTools(pi, stateDir) {
	const report = (patch) => {
		const current = readContextStatus(stateDir);
		if (typeof patch.error === "string") patch.error = patch.error.slice(0, 2048);
		atomicWrite(path.join(stateDir, "context.json"), JSON.stringify({ ...current, ...patch, updatedAt: new Date().toISOString() }));
	};
	const usage = (ctx) => ctx.getContextUsage() ?? { tokens: null, percent: null };

	pi.registerTool({
		name: "resident_inbox", label: "Resident inbox",
		description: "Read a bounded batch of queued local messages (including unacknowledged messages from previous turns), or acknowledge one ID only after handling it. Delivery is at least once.",
		parameters: { type: "object", properties: { action: { type: "string", enum: ["read", "ack"] }, id: { type: "string" } }, required: ["action"], additionalProperties: false },
		async execute(_id, args) {
			if (args.action === "read") return textResult(readInbox(stateDir));
			if (args.action !== "ack" || typeof args.id !== "string") throw new Error("ack requires a message ID");
			acknowledgeMessage(stateDir, args.id);
			return textResult({ acknowledged: args.id });
		},
	});
	pi.registerTool({
		name: "resident_memory", label: "Resident memory",
		description: "Replace the bounded working journal (maximum 16 KiB), persist the home thread, or request RESET and end this turn. RESET clears the session and journal after this child exits, preserving goal, identity, home thread and pending messages.",
		parameters: { type: "object", properties: { action: { type: "string", enum: ["journal", "home", "reset"] }, text: { type: "string" }, thread: { type: "integer", minimum: 1 } }, required: ["action"], additionalProperties: false },
		async execute(_id, args) {
			if (args.action === "journal") {
				if (typeof args.text !== "string") throw new Error("journal requires text");
				writeJournal(stateDir, args.text);
			} else if (args.action === "home") {
				if (!Number.isSafeInteger(args.thread) || args.thread < 1) throw new Error("home requires a positive thread ID");
				atomicWrite(path.join(stateDir, "HOME_THREAD"), `${args.thread}\n`);
			} else if (args.action === "reset") requestControl(stateDir, "RESET");
			else throw new Error("unknown memory action");
			return textResult({ ok: true });
		},
	});

	pi.on("session_start", async (_event, ctx) => {
		report({ ...usage(ctx), sessionId: ctx.sessionManager.getSessionId() });
		const marker = path.join(stateDir, "COMPACT");
		if (!fs.existsSync(marker)) return;
		fs.unlinkSync(marker);
		await new Promise((resolve) => {
			ctx.compact({
				customInstructions: "Preserve the resident goal, decisions, verified facts, outstanding work, and acknowledged message IDs. Summarize bulky tool output.",
				onComplete: () => resolve(),
				onError: (error) => { report({ compaction: "failed", error: error.message }); resolve(); },
			});
		});
	});
	pi.on("turn_end", (_event, ctx) => { report(usage(ctx)); });
	pi.on("session_before_compact", (event, ctx) => {
		const current = readContextStatus(stateDir);
		report({ ...usage(ctx), compaction: "running", reason: event.reason, error: null,
			overflows: (current.overflows ?? 0) + (event.reason === "overflow" ? 1 : 0) });
	});
	pi.on("session_compact", (event, ctx) => {
		const current = readContextStatus(stateDir);
		report({ ...usage(ctx), compaction: "succeeded", reason: event.reason, error: null,
			compactions: (current.compactions ?? 0) + 1 });
	});
	pi.on("session_compact_failed", (event, ctx) => {
		report({ ...usage(ctx), compaction: event.aborted ? "aborted" : "failed", reason: event.reason,
			error: event.errorMessage ?? (event.aborted ? "compaction aborted" : "compaction failed") });
	});
}
