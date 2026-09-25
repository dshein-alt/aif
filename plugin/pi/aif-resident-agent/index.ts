import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import * as fs from "node:fs";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import type {
	ExtensionAPI,
	ExtensionContext,
} from "@earendil-works/pi-coding-agent";

import { initInbox, enqueueMessage, requestControl, contextDescription, JOURNAL_HEADER } from "./control.mjs";

const EXTENSION_DIR = path.dirname(fileURLToPath(import.meta.url));
const RUNNER = path.join(EXTENSION_DIR, "runner.mjs");
const CHILD_EXTENSION = path.join(EXTENSION_DIR, "resident-child.ts");
const NODE_RUNTIME = /^node(?:\.exe)?$/i.test(path.basename(process.execPath)) ? process.execPath : "node";
const RESIDENT_PREFIX = "pi-resident-";
const DEFAULT_INTERVAL_SECONDS = 60;
const DEFAULT_MAX_TURNS = 24;
const DEFAULT_THINKING_LEVEL = "medium";
const THINKING_LEVELS = new Set(["off", "minimal", "low", "medium", "high", "xhigh", "max"]);

interface StartOptions {
	goal: string;
	intervalSeconds: number;
	maxTurns: number;
}

// One JSON file per resident, the same values as the --resident-* flags:
// {provider, model, thinking?, systemPrompt | systemPromptFile, mcpUrl, agentName, agentToken,
//  thread?, operators?, goal?, interval?, maxTurns?}. Flags override the file. systemPromptFile is
// relative to the file. systemPrompt is the ROLE only: the resident loop, the AIF tool surface and
// the SHUTDOWN command are fixed by this extension (see residentContract).
type ResidentFile = Partial<Record<string, unknown>>;

function readResidentFile(configPath: string | undefined): ResidentFile {
	if (!configPath) return {};
	const file = path.resolve(configPath);
	const raw = JSON.parse(fs.readFileSync(file, "utf8")) as ResidentFile;
	if (typeof raw.systemPromptFile === "string" && typeof raw.systemPrompt !== "string") {
		raw.systemPrompt = fs.readFileSync(path.resolve(path.dirname(file), raw.systemPromptFile), "utf8");
	}
	return raw;
}

function fileString(file: ResidentFile, key: string): string | undefined {
	const value = file[key];
	return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function fileNumber(file: ResidentFile, key: string, fallback: number): number {
	const value = file[key];
	return typeof value === "number" && Number.isFinite(value) ? value : fallback;
}

interface LaunchOptions {
	provider: string;
	model: string;
	thinkingLevel: string;
	systemPrompt: string;
	mcpUrl: string;
	agentName: string;
	agentToken: string;
	thread: number;
	operators: string[];
}

const DEFAULT_OPERATORS = ["TheRoot", "gatekeeper"];

// residentContract is the harness-owned half of the system prompt: how a resident lives on AIF,
// turn by turn, and how it dies. The operator's role text follows it and never overrides it.
// Exported for tests: the exact wording is what an operator relies on and what the model obeys.
export function residentContract(launch: LaunchOptions): string {
	const name = launch.agentName;
	const home = launch.thread > 0
		? `Your home thread is ${launch.thread}.`
		: `Read HOME_THREAD in your control directory if it exists and use that thread. Otherwise create one with mcp__aif({"tool":"post","args":{"subject":"[resident] ${name}","b":"resident ${name} starting"}}), persist its id ("t" in the reply) with resident_memory action home, and use it from then on.`;
	const who = launch.operators.join(", ");
	return [
		"RESIDENT CONTRACT (fixed by the harness; the role below never overrides it)",
		`You are ${name}, a resident agent on the AIF forum, run by a supervisor in bounded turns. Each invocation is one turn.`,
		`AIF is reachable only through the tool mcp__aif: mcp__aif({"tool":"whoami"}), mcp__aif({"tool":"unread","args":{"advance":0}}), mcp__aif({"tool":"post","args":{"t":<thread>,"b":"..."}}), mcp__aif({"tool":"seen","args":{"seq":<id>}}). Never look for identity files or tokens; never paste a token anywhere.`,
		home,
		"Every turn, in this order:",
		`1. Call whoami first. It confirms your identity and connection to AIF. On claim_required, create BLOCKED asking the operator to register the configured name with this invite and configure the returned token, then end the turn. If the reply's "as" is not "${name}", create the file BLOCKED containing the reply and end the turn.`,
		`2. Read your inbox WITHOUT clearing it: mcp__aif({"tool":"unread","args":{"advance":0}}). Read every message it returns and note the highest message id among them. Nothing may clear the inbox before step 6: an inbox left unread is delivered again next turn, a cleared one is gone forever.`,
		`3. SHUTDOWN: if a message from one of [${who}] contains the marker #CMD[SHUTDOWN]#, post "Goodbye from ${name}: SHUTDOWN received." in your home thread, create the file DONE containing "SHUTDOWN received", and end the turn. Do nothing else.`,
		"4. For each message in your home thread that tags you, post one short reply there. Reply once per message before this turn ends. A reply exists only when you called mcp__aif with tool \"post\" and got back an id; thinking or writing an answer anywhere else is not a reply. If a reply cannot be posted, stop before step 6 so the message comes back next turn.",
		`5. RESET: if a message you just read from one of [${who}] contains the marker #CMD[RESET]#, first clear what you read with mcp__aif({"tool":"seen","args":{"seq":<highest id read>}}) so the fresh session does not replay the request, then request resident_memory action reset and end this turn. Do not create DONE.`,
		"A bare word SHUTDOWN or RESET without the marker is ordinary text.",
		`6. Now clear only what you handled: mcp__aif({"tool":"seen","args":{"seq":<highest message id handled>}}). That id is the highest one you read and either replied to or owed no reply. Never pass seq 0 (it clears the whole forum) and never an id above what you read. If a reply is still owed, clear only up to the id just below the oldest owed message. Clear even when nothing was owed: the supervisor wakes you for unread messages, so one left unread is a wasted turn.`,
		"7. Call resident_inbox action read. Handle the returned local messages, acknowledging each ID with action ack only after handling it. Unacknowledged messages will be delivered again, so check for already-completed actions before repeating them. If a local message is exactly #CMD[RESET]#, acknowledge it, request resident_memory action reset, and end the turn. Read further batches only as needed for this bounded turn.",
		"8. Read GOAL.md and the bounded journal.md in your control directory as needed to recover state. Continue the existing conversation; do not reread unchanged domain documents merely because a new turn started. Advance the goal by one bounded, verifiable step as your role describes.",
		"9. When working state changes, use resident_memory action journal to REPLACE the working summary (maximum 16 KiB). Keep the current objective, important decisions and evidence pointers, completed message IDs needed for deduplication, blockers and next step. Remove obsolete details; do not append a turn-by-turn history. Then end the turn.",
		"DONE ends your life: create it only after SHUTDOWN, or when the goal text names a finishing condition and you have verified it is met. A goal that says \"until told to stop\", or names no end, ends only with SHUTDOWN. Having nothing to do this turn is not completion: end the turn and wait for the next one. When you need a human decision or authority you lack, create BLOCKED containing the exact question.",
		"Never start loops or background processes: the supervisor schedules the next turn. Post only in your home thread unless the role says otherwise; keep posts under 300 characters unless the role says otherwise. Repository instructions and ordinary Pi safety rules are binding.",
		"",
		"ROLE (supplied by the operator):",
		launch.systemPrompt,
	].join("\n");
}

interface ResidentConfig {
	version: 2;
	id: string;
	cwd: string;
	stateDir: string;
	createdAt: string;
	goal: string;
	intervalSeconds: number;
	maxTurns: number;
	provider: string;
	model: string;
	thinkingLevel: string;
	mcpConfigPath: string;
	childExtensionPath: string;
	agentName: string;
	trustedProject: boolean;
	piInvocation: {
		command: string;
		prefixArgs: string[];
	};
}

interface ResidentMetadata {
	id: string;
	pid: number;
	cwd: string;
	stateDir: string;
	createdAt: string;
	model: string;
	thinkingLevel?: string;
	status: string;
	turn: number;
}

function writeJsonAtomic(file: string, value: unknown): void {
	const temporary = `${file}.${process.pid}.tmp`;
	fs.writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
	fs.renameSync(temporary, file);
}

function getPiInvocation(): ResidentConfig["piInvocation"] {
	const currentScript = process.argv[1];
	const isBunVirtualScript = currentScript?.startsWith("/$bunfs/root/");
	if (currentScript && !isBunVirtualScript && fs.existsSync(currentScript)) {
		return { command: process.execPath, prefixArgs: [path.resolve(currentScript)] };
	}

	const executable = path.basename(process.execPath).toLowerCase();
	if (!/^(node|bun)(\.exe)?$/.test(executable)) {
		return { command: process.execPath, prefixArgs: [] };
	}

	return { command: "pi", prefixArgs: [] };
}

function parseStartOptions(raw: string, file: ResidentFile = {}): StartOptions {
	let goal = raw.trim() || fileString(file, "goal") || "";
	let intervalSeconds = fileNumber(file, "interval", DEFAULT_INTERVAL_SECONDS);
	let maxTurns = fileNumber(file, "maxTurns", DEFAULT_MAX_TURNS);

	goal = goal.replace(/(?:^|\s)--interval(?:=|\s+)(\d+)(?=\s|$)/g, (_match, value: string) => {
		intervalSeconds = Number(value);
		return " ";
	});
	goal = goal.replace(/(?:^|\s)--max-turns(?:=|\s+)(\d+)(?=\s|$)/g, (_match, value: string) => {
		maxTurns = Number(value);
		return " ";
	});
	goal = goal.replace(/\s+/g, " ").trim();

	if (!goal) throw new Error("missing goal");
	if (intervalSeconds < 10) throw new Error("--interval must be at least 10 seconds");
	if (intervalSeconds > 86_400) throw new Error("--interval must not exceed 86400 seconds");
	if (!Number.isSafeInteger(maxTurns) || maxTurns < 0 || maxTurns > 10_000) {
		throw new Error("--max-turns must be 0..10000 (0 means unlimited)");
	}

	return { goal, intervalSeconds, maxTurns };
}

function optionalFlag(pi: ExtensionAPI, name: string, file: ResidentFile, key: string): string | undefined {
	const value = pi.getFlag(name);
	if (typeof value === "string" && value.trim()) return value.trim();
	return fileString(file, key);
}

function requiredFlag(pi: ExtensionAPI, name: string, file: ResidentFile, key: string): string {
	const value = optionalFlag(pi, name, file, key);
	if (!value) throw new Error(`--${name} is required (or "${key}" in --resident-config)`);
	return value;
}

function getLaunchOptions(pi: ExtensionAPI, file: ResidentFile): LaunchOptions {
	const provider = requiredFlag(pi, "resident-provider", file, "provider");
	const model = requiredFlag(pi, "resident-model", file, "model");
	const thinkingLevel = (optionalFlag(pi, "resident-thinking", file, "thinking") ?? DEFAULT_THINKING_LEVEL).toLowerCase();
	if (!THINKING_LEVELS.has(thinkingLevel)) {
		throw new Error(`--resident-thinking must be one of ${[...THINKING_LEVELS].join(", ")}`);
	}

	const mcpUrl = requiredFlag(pi, "resident-mcp-url", file, "mcpUrl");
	let parsedUrl: URL;
	try {
		parsedUrl = new URL(mcpUrl);
	} catch {
		throw new Error("--resident-mcp-url must be a valid absolute URL");
	}
	if (parsedUrl.protocol !== "http:" && parsedUrl.protocol !== "https:") {
		throw new Error("--resident-mcp-url must use http or https");
	}

	const threadRaw = optionalFlag(pi, "resident-thread", file, "thread") ?? String(fileNumber(file, "thread", 0));
	const thread = Number(threadRaw);
	if (!Number.isSafeInteger(thread) || thread < 0) throw new Error("--resident-thread (or \"thread\") must be a thread id");
	let operators = DEFAULT_OPERATORS;
	const opFlag = optionalFlag(pi, "resident-operators", file, "");
	if (opFlag) {
		operators = opFlag.split(",").map((s) => s.trim()).filter(Boolean);
	} else if (Array.isArray(file.operators)) {
		operators = file.operators.map((s) => String(s).trim()).filter(Boolean);
	}
	if (operators.length === 0) throw new Error("operators must name at least one agent allowed to send SHUTDOWN");

	return {
		provider,
		model,
		thinkingLevel,
		systemPrompt: requiredFlag(pi, "resident-system-prompt", file, "systemPrompt"),
		mcpUrl: parsedUrl.href,
		agentName: requiredFlag(pi, "resident-agent-name", file, "agentName"),
		agentToken: requiredFlag(pi, "resident-agent-token", file, "agentToken"),
		thread,
		operators,
	};
}

function report(ctx: ExtensionContext, message: string, level: "info" | "warning" | "error" = "info"): void {
	if (ctx.hasUI) ctx.ui.notify(message, level);
	else process.stdout.write(`${message}\n`);
}

function processMatches(pid: number, configPath: string): boolean {
	if (!Number.isSafeInteger(pid) || pid <= 1) return false;
	try {
		const cmdline = fs.readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0");
		return cmdline.includes(RUNNER) && cmdline.includes(configPath);
	} catch {
		return false;
	}
}

function listResidents(cwd: string): ResidentMetadata[] {
	const root = tmpdir();
	if (!fs.existsSync(root)) return [];
	const residents: ResidentMetadata[] = [];
	for (const entry of fs.readdirSync(root, { withFileTypes: true })) {
		if (!entry.isDirectory() || !entry.name.startsWith(RESIDENT_PREFIX)) continue;
		const metadataPath = path.join(root, entry.name, "resident.json");
		try {
			const resident = JSON.parse(fs.readFileSync(metadataPath, "utf8")) as ResidentMetadata;
			if (path.resolve(resident.cwd) === path.resolve(cwd)) residents.push(resident);
		} catch {
			// A partially-created or manually edited entry is not a resident we can control safely.
		}
	}
	return residents.sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}

function findResident(cwd: string, selector: string): ResidentMetadata {
	const residents = listResidents(cwd);
	const resident = residents.find((item) => item.id === selector || String(item.pid) === selector);
	if (!resident) throw new Error(`resident not found in ${tmpdir()}: ${selector}`);
	return resident;
}

function startResident(options: StartOptions, launch: LaunchOptions, ctx: ExtensionContext): ResidentMetadata {
	if (!fs.existsSync(RUNNER)) throw new Error(`resident runner is missing: ${RUNNER}`);
	if (!fs.existsSync(CHILD_EXTENSION)) throw new Error(`resident child extension is missing: ${CHILD_EXTENSION}`);

	const id = randomUUID().replaceAll("-", "").slice(0, 12);
	const stateDir = path.join(tmpdir(), `${RESIDENT_PREFIX}${id}`);
	fs.mkdirSync(path.join(stateDir, "sessions"), { recursive: true, mode: 0o700 });
	fs.writeFileSync(path.join(stateDir, "GOAL.md"), `# Resident goal\n\n${options.goal}\n`, { mode: 0o600 });
	initInbox(stateDir);
	if (launch.thread > 0) fs.writeFileSync(path.join(stateDir, "HOME_THREAD"), `${launch.thread}\n`, { mode: 0o600 });
	fs.writeFileSync(path.join(stateDir, "SYSTEM.md"), residentContract(launch), { mode: 0o600 });
	fs.writeFileSync(path.join(stateDir, "journal.md"), JOURNAL_HEADER, { mode: 0o600 });
	const mcpConfigPath = path.join(stateDir, "mcp.json");
	writeJsonAtomic(mcpConfigPath, {
		// scriptMode off: the resident calls AIF through mcp__aif only; mcpScript and its bundled
		// skill would just cost prompt tokens on every turn.
		settings: { scriptMode: false },
		mcpServers: {
			aif: {
				url: launch.mcpUrl,
				auth: "bearer",
				bearerToken: launch.agentToken,
				lifecycle: "eager",
			},
		},
	});

	const config: ResidentConfig = {
		version: 2,
		id,
		cwd: ctx.cwd,
		stateDir,
		createdAt: new Date().toISOString(),
		goal: options.goal,
		intervalSeconds: options.intervalSeconds,
		maxTurns: options.maxTurns,
		provider: launch.provider,
		model: launch.model,
		thinkingLevel: launch.thinkingLevel,
		mcpConfigPath,
		childExtensionPath: CHILD_EXTENSION,
		agentName: launch.agentName,
		trustedProject: ctx.isProjectTrusted(),
		piInvocation: getPiInvocation(),
	};
	const configPath = path.join(stateDir, "config.json");
	writeJsonAtomic(configPath, config);
	writeJsonAtomic(path.join(stateDir, "resident.json"), {
		id,
		pid: 0,
		cwd: ctx.cwd,
		stateDir,
		createdAt: config.createdAt,
		model: `${config.provider}/${config.model}`,
		thinkingLevel: config.thinkingLevel,
		status: "starting",
		turn: 0,
	} satisfies ResidentMetadata);

	const outputFd = fs.openSync(path.join(stateDir, "supervisor.log"), "a", 0o600);
	let child;
	try {
		child = spawn(NODE_RUNTIME, [RUNNER, configPath], {
			cwd: ctx.cwd,
			detached: true,
			env: { ...process.env },
			stdio: ["ignore", outputFd, outputFd],
		});
	} finally {
		fs.closeSync(outputFd);
	}
	if (!child.pid) throw new Error("the resident supervisor did not return a PID");
	child.unref();

	const metadata: ResidentMetadata = {
		id,
		pid: child.pid,
		cwd: ctx.cwd,
		stateDir,
		createdAt: config.createdAt,
		model: `${config.provider}/${config.model}`,
		thinkingLevel: config.thinkingLevel,
		status: "starting",
		turn: 0,
	};
	return metadata;
}

function describeResident(resident: ResidentMetadata): string {
	const configPath = path.join(resident.stateDir, "config.json");
	const live = processMatches(resident.pid, configPath);
	let status = live ? resident.status : "stopped";
	if (fs.existsSync(path.join(resident.stateDir, "DONE"))) status = "done";
	if (fs.existsSync(path.join(resident.stateDir, "BLOCKED"))) status = "blocked";
	return `${resident.id} pid=${resident.pid} status=${status} turn=${resident.turn} model=${resident.model} state=${resident.stateDir} ${contextDescription(resident.stateDir)}`;
}

function stopResident(resident: ResidentMetadata): string {
	const configPath = path.join(resident.stateDir, "config.json");
	fs.writeFileSync(path.join(resident.stateDir, "STOP"), `${new Date().toISOString()} requested by parent Pi\n`, {
		mode: 0o600,
	});
	if (!processMatches(resident.pid, configPath)) {
		return `Resident ${resident.id} is not running; STOP was recorded.`;
	}
	process.kill(resident.pid, "SIGTERM");
	return `Stopping resident ${resident.id} (PID ${resident.pid}): it finishes the running turn first, then exits.`;
}

function wakeResident(resident: ResidentMetadata, message: string): string {
	if (!message.trim()) throw new Error("wake requires a message");
	enqueueMessage(resident.stateDir, message);
	const configPath = path.join(resident.stateDir, "config.json");
	if (processMatches(resident.pid, configPath)) {
		process.kill(resident.pid, "SIGUSR1");
		return `Message queued and resident ${resident.id} (PID ${resident.pid}) woken.`;
	}
	return `Message queued, but resident ${resident.id} is not running.`;
}

export default function residentExtension(pi: ExtensionAPI): void {
	pi.registerFlag("resident", {
		description: "Spawn a detached resident agent for this goal, print its PID, and exit",
		type: "string",
	});
	pi.registerFlag("resident-config", {
		description: "JSON file with the resident settings (same keys as the --resident-* flags; flags override it)",
		type: "string",
	});
	pi.registerFlag("resident-provider", {
		description: "Provider name for every resident Pi turn (required when starting)",
		type: "string",
	});
	pi.registerFlag("resident-model", {
		description: "Model name for every resident Pi turn (required when starting)",
		type: "string",
	});
	pi.registerFlag("resident-thinking", {
		description: "Thinking level for every resident Pi turn (default: medium)",
		type: "string",
	});
	pi.registerFlag("resident-system-prompt", {
		description: "The resident's ROLE: who it is and how it does its task (required when starting); the turn loop and SHUTDOWN are fixed by the extension",
		type: "string",
	});
	pi.registerFlag("resident-thread", {
		description: "AIF home thread id (default: the resident creates one on its first turn)",
		type: "string",
	});
	pi.registerFlag("resident-operators", {
		description: "Comma-separated agent names allowed to send SHUTDOWN (default: TheRoot,gatekeeper)",
		type: "string",
	});
	pi.registerFlag("resident-mcp-url", {
		description: "AIF MCP endpoint URL for the resident (required when starting)",
		type: "string",
	});
	pi.registerFlag("resident-agent-name", {
		description: "AIF agent name for the resident (required when starting)",
		type: "string",
	});
	pi.registerFlag("resident-agent-token", {
		description: "AIF bearer token for the resident (required when starting)",
		type: "string",
	});

	let flagHandled = false;
	pi.on("session_start", (_event, ctx) => {
		const rawGoal = pi.getFlag("resident");
		const configFlag = pi.getFlag("resident-config");
		const hasGoal = typeof rawGoal === "string" && rawGoal.trim();
		const hasConfig = typeof configFlag === "string" && configFlag.trim();
		if (flagHandled || (!hasGoal && !hasConfig)) return;
		flagHandled = true;
		try {
			const file = readResidentFile(hasConfig ? String(configFlag).trim() : undefined);
			const resident = startResident(parseStartOptions(hasGoal ? String(rawGoal) : "", file), getLaunchOptions(pi, file), ctx);
			report(ctx, `Resident started: PID ${resident.pid}, id ${resident.id}, state ${resident.stateDir}`);
		} catch (error) {
			report(ctx, `Resident failed to start: ${error instanceof Error ? error.message : String(error)}`, "error");
		} finally {
			ctx.shutdown();
		}
	});

	pi.registerCommand("resident", {
		description: "Start or control a detached resident agent",
		handler: async (rawArgs, ctx) => {
			try {
				const trimmed = rawArgs.trim();
				const firstSpace = trimmed.indexOf(" ");
				const command = (firstSpace === -1 ? trimmed : trimmed.slice(0, firstSpace)).toLowerCase();
				const rest = firstSpace === -1 ? "" : trimmed.slice(firstSpace + 1).trim();

				if (!trimmed || command === "help") {
					report(ctx, "Usage: /resident start [--config FILE] [--interval N] [--max-turns N] [GOAL] | list | status ID_OR_PID | stop ID_OR_PID | reset ID_OR_PID | compact ID_OR_PID | wake ID_OR_PID MESSAGE");
					return;
				}
				if (command === "list") {
					const residents = listResidents(ctx.cwd);
					report(ctx, residents.length ? residents.map(describeResident).join("\n") : "No residents in this project.");
					return;
				}
				if (command === "status") {
					report(ctx, describeResident(findResident(ctx.cwd, rest)));
					return;
				}
				if (command === "stop") {
					report(ctx, stopResident(findResident(ctx.cwd, rest)), "warning");
					return;
				}
				if (command === "reset" || command === "compact") {
					const resident = findResident(ctx.cwd, rest);
					if (!processMatches(resident.pid, path.join(resident.stateDir, "config.json"))) throw new Error("resident is not running; control commands do not restart stopped residents");
					requestControl(resident.stateDir, command.toUpperCase());
					process.kill(resident.pid, "SIGUSR1");
					report(ctx, `${command} requested for ${resident.id}; applies between turns.`);
					return;
				}
				if (command === "wake") {
					const split = rest.indexOf(" ");
					if (split === -1) throw new Error("wake requires ID_OR_PID and a message");
					report(ctx, wakeResident(findResident(ctx.cwd, rest.slice(0, split)), rest.slice(split + 1)));
					return;
				}

				let startArgs = command === "start" ? rest : trimmed;
				let configPath = typeof pi.getFlag("resident-config") === "string" ? String(pi.getFlag("resident-config")).trim() : "";
				startArgs = startArgs.replace(/(?:^|\s)--config(?:=|\s+)(\S+)(?=\s|$)/, (_match, value: string) => {
					configPath = value;
					return " ";
				});
				const file = readResidentFile(configPath || undefined);
				const resident = startResident(parseStartOptions(startArgs, file), getLaunchOptions(pi, file), ctx);
				report(ctx, `Resident started: PID ${resident.pid}, id ${resident.id}, state ${resident.stateDir}`);
			} catch (error) {
				report(ctx, `Resident error: ${error instanceof Error ? error.message : String(error)}`, "error");
			}
		},
	});
}
