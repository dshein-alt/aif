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

const EXTENSION_DIR = path.dirname(fileURLToPath(import.meta.url));
const RUNNER = path.join(EXTENSION_DIR, "runner.mjs");
const CHILD_EXTENSION = path.join(EXTENSION_DIR, "resident-child.ts");
const NODE_RUNTIME = /^node(?:\.exe)?$/i.test(path.basename(process.execPath)) ? process.execPath : "node";
const RESIDENT_PREFIX = "pi-resident-";
const DEFAULT_INTERVAL_SECONDS = 300;
const DEFAULT_MAX_TURNS = 24;
const DEFAULT_THINKING_LEVEL = "medium";
const THINKING_LEVELS = new Set(["off", "minimal", "low", "medium", "high", "xhigh", "max"]);

interface StartOptions {
	goal: string;
	intervalSeconds: number;
	maxTurns: number;
}

interface LaunchOptions {
	provider: string;
	model: string;
	thinkingLevel: string;
	systemPrompt: string;
	mcpUrl: string;
	agentName: string;
	agentToken: string;
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

function parseStartOptions(raw: string): StartOptions {
	let goal = raw.trim();
	let intervalSeconds = DEFAULT_INTERVAL_SECONDS;
	let maxTurns = DEFAULT_MAX_TURNS;

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

function requiredFlag(pi: ExtensionAPI, name: string): string {
	const value = pi.getFlag(name);
	if (typeof value !== "string" || !value.trim()) throw new Error(`--${name} is required`);
	return value.trim();
}

function getLaunchOptions(pi: ExtensionAPI): LaunchOptions {
	const provider = requiredFlag(pi, "resident-provider");
	const model = requiredFlag(pi, "resident-model");
	const thinkingLevel = typeof pi.getFlag("resident-thinking") === "string"
		? String(pi.getFlag("resident-thinking")).trim().toLowerCase()
		: DEFAULT_THINKING_LEVEL;
	if (!THINKING_LEVELS.has(thinkingLevel)) {
		throw new Error(`--resident-thinking must be one of ${[...THINKING_LEVELS].join(", ")}`);
	}

	const mcpUrl = requiredFlag(pi, "resident-mcp-url");
	let parsedUrl: URL;
	try {
		parsedUrl = new URL(mcpUrl);
	} catch {
		throw new Error("--resident-mcp-url must be a valid absolute URL");
	}
	if (parsedUrl.protocol !== "http:" && parsedUrl.protocol !== "https:") {
		throw new Error("--resident-mcp-url must use http or https");
	}

	return {
		provider,
		model,
		thinkingLevel,
		systemPrompt: requiredFlag(pi, "resident-system-prompt"),
		mcpUrl: parsedUrl.href,
		agentName: requiredFlag(pi, "resident-agent-name"),
		agentToken: requiredFlag(pi, "resident-agent-token"),
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
	fs.writeFileSync(
		path.join(stateDir, "INBOX.md"),
		"# Resident inbox\n\nMessages appended here are read on the next turn.\n",
		{ mode: 0o600 },
	);
	fs.writeFileSync(
		path.join(stateDir, "SYSTEM.md"),
		[
			launch.systemPrompt,
			"",
			"Resident runtime contract:",
			"You are a resident Pi agent: a detached worker that advances one durable goal over multiple bounded turns.",
			`Your AIF identity is ${JSON.stringify(launch.agentName)}. Use the configured mcp__aif tool for AIF communication; never search for identity files or credentials.`,
			"Each invocation is one turn. Read GOAL.md, INBOX.md, journal.md, and the repository state before acting.",
			"Do one coherent, verifiable unit of work, update journal.md with evidence and the next step, then end the turn.",
			"When the goal is genuinely complete, create DONE containing a concise completion summary.",
			"If the supplied system prompt defines a termination command and you receive it, finish the current task, update journal.md, create DONE containing the termination reason, and end the turn.",
			"When progress requires human input or new authority, create BLOCKED containing the exact question or missing authority.",
			"Never start another infinite loop or background resident. The outer supervisor schedules future turns.",
			"Treat repository instructions and ordinary Pi safety rules as binding.",
		].join("\n"),
		{ mode: 0o600 },
	);
	fs.writeFileSync(path.join(stateDir, "journal.md"), "# Resident journal\n", { mode: 0o600 });
	const mcpConfigPath = path.join(stateDir, "mcp.json");
	writeJsonAtomic(mcpConfigPath, {
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
	return `${resident.id} pid=${resident.pid} status=${status} turn=${resident.turn} model=${resident.model} state=${resident.stateDir}`;
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
	return `Stopping resident ${resident.id} (PID ${resident.pid}).`;
}

function wakeResident(resident: ResidentMetadata, message: string): string {
	if (!message.trim()) throw new Error("wake requires a message");
	fs.appendFileSync(
		path.join(resident.stateDir, "INBOX.md"),
		`\n## ${new Date().toISOString()}\n\n${message.trim()}\n`,
		{ mode: 0o600 },
	);
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
		description: "System prompt controlling resident behavior and its semantic termination command (required when starting)",
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
		if (flagHandled || typeof rawGoal !== "string" || !rawGoal.trim()) return;
		flagHandled = true;
		try {
			const resident = startResident(parseStartOptions(rawGoal), getLaunchOptions(pi), ctx);
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
					report(ctx, "Usage: /resident start [--interval N] [--max-turns N] GOAL | list | status ID_OR_PID | stop ID_OR_PID | wake ID_OR_PID MESSAGE");
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
				if (command === "wake") {
					const split = rest.indexOf(" ");
					if (split === -1) throw new Error("wake requires ID_OR_PID and a message");
					report(ctx, wakeResident(findResident(ctx.cwd, rest.slice(0, split)), rest.slice(split + 1)));
					return;
				}

				const startArgs = command === "start" ? rest : trimmed;
				const resident = startResident(parseStartOptions(startArgs), getLaunchOptions(pi), ctx);
				report(ctx, `Resident started: PID ${resident.pid}, id ${resident.id}, state ${resident.stateDir}`);
			} catch (error) {
				report(ctx, `Resident error: ${error instanceof Error ? error.message : String(error)}`, "error");
			}
		},
	});
}
