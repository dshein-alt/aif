#!/usr/bin/env node

import { spawn } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import { pathToFileURL } from "node:url";

export function writeJsonAtomic(file, value) {
	const temporary = `${file}.${process.pid}.tmp`;
	fs.writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
	fs.renameSync(temporary, file);
}

export function buildPiArgs(config, turn) {
	const args = [
		...config.piInvocation.prefixArgs,
		"--no-extensions",
		"--extension",
		config.childExtensionPath,
		"--resident-mcp-config",
		config.mcpConfigPath,
		"--mode",
		"json",
		"--session-dir",
		path.join(config.stateDir, "sessions"),
		"--session-id",
		config.id,
		"--name",
		`resident:${config.id}`,
		"--provider",
		config.provider,
		"--model",
		config.model,
		"--thinking",
		config.thinkingLevel,
		"--append-system-prompt",
		path.join(config.stateDir, "SYSTEM.md"),
		config.trustedProject ? "--approve" : "--no-approve",
	];
	args.push(
		"-p",
		[
			`Resident turn ${turn}.`,
			`Your durable control directory is ${config.stateDir}.`,
			"Read GOAL.md, INBOX.md, and journal.md there, inspect the current repository state, and advance the goal by one bounded step.",
			"Record what you verified and the next step in journal.md. Create DONE or BLOCKED as described by the resident system prompt when appropriate.",
		].join(" "),
	);
	return args;
}

function appendSupervisorLog(config, message) {
	fs.appendFileSync(
		path.join(config.stateDir, "supervisor.log"),
		`${new Date().toISOString()} ${message}\n`,
		{ mode: 0o600 },
	);
}

function updateMetadata(config, patch) {
	const file = path.join(config.stateDir, "resident.json");
	let current = {};
	try {
		current = JSON.parse(fs.readFileSync(file, "utf8"));
	} catch {
		// The extension may still be finishing the initial metadata write.
	}
	writeJsonAtomic(file, { ...current, ...patch, updatedAt: new Date().toISOString() });
}

function runTurn(config, turn) {
	return new Promise((resolve) => {
		const turnLog = path.join(config.stateDir, `turn-${String(turn).padStart(4, "0")}.jsonl`);
		const outputFd = fs.openSync(turnLog, "a", 0o600);
		const child = spawn(config.piInvocation.command, buildPiArgs(config, turn), {
			cwd: config.cwd,
			env: {
				...process.env,
				PI_RESIDENT_ID: config.id,
				PI_RESIDENT_STATE_DIR: config.stateDir,
			},
			stdio: ["ignore", outputFd, outputFd],
		});
		let settled = false;
		const finish = (code) => {
			if (settled) return;
			settled = true;
			fs.closeSync(outputFd);
			resolve(code);
		};
		child.once("error", (error) => {
			appendSupervisorLog(config, `turn=${turn} spawn-error=${JSON.stringify(error.message)}`);
			finish(127);
		});
		child.once("close", (code, signal) => {
			appendSupervisorLog(config, `turn=${turn} exit=${code ?? "null"} signal=${signal ?? "none"}`);
			finish(code ?? 1);
		});
	});
}

function makeWakeableDelay(milliseconds, state) {
	return new Promise((resolve) => {
		if (state.stopping || state.wakeRequested) {
			state.wakeRequested = false;
			resolve();
			return;
		}
		const timer = setTimeout(() => {
			state.resolveDelay = undefined;
			resolve();
		}, milliseconds);
		state.resolveDelay = () => {
			clearTimeout(timer);
			state.resolveDelay = undefined;
			state.wakeRequested = false;
			resolve();
		};
	});
}

export async function runResident(configPath) {
	const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
	if (config.version !== 2) throw new Error(`unsupported resident config version: ${config.version}`);
	if (path.resolve(config.stateDir, "config.json") !== path.resolve(configPath)) {
		throw new Error("config path does not belong to its state directory");
	}
	if (path.resolve(config.stateDir, "mcp.json") !== path.resolve(config.mcpConfigPath)) {
		throw new Error("MCP config path does not belong to the resident state directory");
	}
	if (!path.isAbsolute(config.childExtensionPath)) {
		throw new Error("resident child extension path must be absolute");
	}

	const state = { stopping: false, wakeRequested: false, resolveDelay: undefined };
	const requestStop = () => {
		state.stopping = true;
		state.resolveDelay?.();
	};
	process.on("SIGTERM", requestStop);
	process.on("SIGINT", requestStop);
	process.on("SIGHUP", () => undefined);
	process.on("SIGUSR1", () => {
		state.wakeRequested = true;
		state.resolveDelay?.();
	});

	appendSupervisorLog(config, `started pid=${process.pid} model=${config.provider}/${config.model} thinking=${config.thinkingLevel} agent=${JSON.stringify(config.agentName)} interval=${config.intervalSeconds}s maxTurns=${config.maxTurns || "unlimited"}`);
	updateMetadata(config, { pid: process.pid, status: "running", turn: 0 });

	let turn = 0;
	let finalStatus = "stopped";
	try {
		while (!state.stopping) {
			if (fs.existsSync(path.join(config.stateDir, "STOP"))) break;
			if (fs.existsSync(path.join(config.stateDir, "DONE"))) {
				finalStatus = "done";
				break;
			}
			if (fs.existsSync(path.join(config.stateDir, "BLOCKED"))) {
				finalStatus = "blocked";
				break;
			}
			if (config.maxTurns > 0 && turn >= config.maxTurns) {
				finalStatus = "turn-limit";
				break;
			}

			turn += 1;
			updateMetadata(config, { status: "working", turn });
			const exitCode = await runTurn(config, turn);
			updateMetadata(config, { status: exitCode === 0 ? "sleeping" : "retrying", turn, lastExitCode: exitCode });
			if (state.stopping) break;
			if (fs.existsSync(path.join(config.stateDir, "DONE"))) {
				finalStatus = "done";
				break;
			}
			if (fs.existsSync(path.join(config.stateDir, "BLOCKED"))) {
				finalStatus = "blocked";
				break;
			}
			if (config.maxTurns > 0 && turn >= config.maxTurns) {
				finalStatus = "turn-limit";
				break;
			}
			await makeWakeableDelay(config.intervalSeconds * 1000, state);
		}
	} finally {
		appendSupervisorLog(config, `exiting pid=${process.pid} status=${finalStatus} turn=${turn}`);
		updateMetadata(config, { status: finalStatus, turn, stoppedAt: new Date().toISOString() });
	}
	return { status: finalStatus, turn };
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href;
if (isMain) {
	const configPath = process.argv[2];
	if (!configPath) {
		process.stderr.write("usage: runner.mjs CONFIG_PATH\n");
		process.exitCode = 2;
	} else {
		runResident(path.resolve(configPath)).catch((error) => {
			process.stderr.write(`resident supervisor failed: ${error instanceof Error ? error.stack : String(error)}\n`);
			process.exitCode = 1;
		});
	}
}
