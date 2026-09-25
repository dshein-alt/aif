#!/usr/bin/env node

import { spawn } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import { pathToFileURL } from "node:url";

import { applyReset, boundJournal, contextDescription } from "./control.mjs";

export function writeJsonAtomic(file, value) {
	const temporary = `${file}.${process.pid}.tmp`;
	fs.writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
	fs.renameSync(temporary, file);
}

const WAKE_REASON = {
	start: "first turn",
	inbox: "AIF reports unread messages for you",
	wake: "a local message was queued with wake",
};
const POLL_WAIT_CAP_SECONDS = 60; // the server's ceiling for one long-poll

export function buildPiArgs(config, turn, reason = "start") {
	const args = [
		...config.piInvocation.prefixArgs,
		"--no-extensions",
		"--no-context-files",
		"--no-skills",
		"--no-prompt-templates",
		"--extension",
		config.childExtensionPath,
		"--resident-mcp-config",
		config.mcpConfigPath,
		"--mode",
		"json",
		"--session-dir",
		path.join(config.stateDir, "sessions"),
		"--session-id",
		config.sessionId ?? config.id,
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
			`Resident turn ${turn}, woken because ${WAKE_REASON[reason]}.`,
			`Your durable control directory is ${config.stateDir}.`,
			"Run the resident contract's turn loop first (whoami, unread with advance 0, SHUTDOWN check, replies, then clear the inbox with seen), then use resident_inbox to read and acknowledge local messages. Recover working state from GOAL.md and journal.md as needed and advance the goal by one bounded step.",
			"When state changes, replace the bounded working summary using resident_memory action journal. Create DONE or BLOCKED as described by the resident system prompt when appropriate.",
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

function runTurn(config, turn, reason) {
	return new Promise((resolve) => {
		const turnLog = path.join(config.stateDir, `turn-${String(turn).padStart(4, "0")}.jsonl`);
		const outputFd = fs.openSync(turnLog, "a", 0o600);
		const child = spawn(config.piInvocation.command, buildPiArgs(config, turn, reason), {
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

// A sleep that a wake or stop signal cuts short (state.resolveDelay is the signal handlers' hook).
function pause(milliseconds, state) {
	return new Promise((resolve) => {
		const timer = setTimeout(resolve, milliseconds);
		state.resolveDelay = () => {
			clearTimeout(timer);
			resolve();
		};
	}).finally(() => { state.resolveDelay = undefined; });
}

// The supervisor, not the model, watches AIF: every intervalSeconds it long-polls GET /api/poll
// with the resident's own token, which never advances the cursor. A turn starts only when the inbox
// has grown (n>0 and a newer forum seq than the last inbox-triggered turn, so a message the model
// leaves unread cannot spin the loop) or when a local wake arrives. An idle resident costs one HTTP
// request per interval and no model tokens; there is no timed heartbeat.
async function waitForWork(config, state, aif) {
	while (!state.stopping && !state.wakeRequested) {
		const controller = new AbortController();
		state.resolveDelay = () => controller.abort();
		const started = Date.now();
		try {
			const wait = Math.min(config.intervalSeconds, POLL_WAIT_CAP_SECONDS);
			const response = await fetch(new URL(`../api/poll?wait=${wait}`, aif.url), {
				headers: { authorization: `Bearer ${aif.bearerToken}` },
				signal: controller.signal,
			});
			const body = await response.json();
			if (!response.ok) throw new Error(body.error ?? `HTTP ${response.status}`);
			state.pollError = undefined;
			if (body.n > 0 && body.seq !== state.pollSeq) {
				state.pollSeq = body.seq;
				return "inbox";
			}
		} catch (error) {
			if (!controller.signal.aborted && error.message !== state.pollError) {
				state.pollError = error.message;
				appendSupervisorLog(config, `poll error=${JSON.stringify(error.message)}`);
			}
		} finally {
			state.resolveDelay = undefined;
		}
		// The interval is the check cadence: an early answer (error, a cap below the interval, or
		// unread that is not news) waits out the rest of it before asking again.
		const rest = config.intervalSeconds * 1000 - (Date.now() - started);
		if (rest > 0 && !state.stopping && !state.wakeRequested) await pause(rest, state);
	}
	state.wakeRequested = false;
	return state.stopping ? "stop" : "wake";
}

function readAifServer(config) {
	const aif = JSON.parse(fs.readFileSync(config.mcpConfigPath, "utf8")).mcpServers?.aif;
	if (typeof aif?.url !== "string" || typeof aif?.bearerToken !== "string") {
		throw new Error(`no aif server in ${config.mcpConfigPath}`);
	}
	return aif;
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

	try { config.sessionId = JSON.parse(fs.readFileSync(path.join(config.stateDir, "session.json"), "utf8")).id; }
	catch (error) { if (error.code !== "ENOENT") throw error; }
	// Complete a reset interrupted by a supervisor crash before accepting more work.
	if (fs.existsSync(path.join(config.stateDir, "RESET.processing")) && !fs.existsSync(path.join(config.stateDir, "RESET"))) {
		fs.renameSync(path.join(config.stateDir, "RESET.processing"), path.join(config.stateDir, "RESET"));
	}
	const reset = () => {
		if (!applyReset(config)) return false;
		appendSupervisorLog(config, `reset session=${config.sessionId}`);
		updateMetadata(config, { sessionId: config.sessionId, resetAt: new Date().toISOString() });
		return true;
	};
	const aif = readAifServer(config);
	const state = { stopping: false, wakeRequested: false, resolveDelay: undefined, pollSeq: undefined, pollError: undefined };
	const requestStop = () => {
		state.stopping = true;
		state.resolveDelay?.();
	};
	process.on("SIGTERM", requestStop);
	process.on("SIGINT", requestStop);
	const ignoreHangup = () => undefined;
	const requestWake = () => {
		state.wakeRequested = true;
		state.resolveDelay?.();
	};
	process.on("SIGHUP", ignoreHangup);
	process.on("SIGUSR1", requestWake);

	appendSupervisorLog(config, `started pid=${process.pid} model=${config.provider}/${config.model} thinking=${config.thinkingLevel} agent=${JSON.stringify(config.agentName)} interval=${config.intervalSeconds}s maxTurns=${config.maxTurns || "unlimited"}`);
	updateMetadata(config, { pid: process.pid, status: "running", turn: 0 });

	let turn = 0;
	let reason = "start";
	let finalStatus = "stopped";
	try {
		while (!state.stopping) {
			if (fs.existsSync(path.join(config.stateDir, "STOP"))) break;
			reset();
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
			updateMetadata(config, { status: "working", turn, reason });
			const exitCode = await runTurn(config, turn, reason);
			updateMetadata(config, { status: exitCode === 0 ? "sleeping" : "retrying", turn, lastExitCode: exitCode });
			if (boundJournal(config.stateDir)) appendSupervisorLog(config, "journal exceeded 16 KiB; retained bounded recent notes");
			appendSupervisorLog(config, contextDescription(config.stateDir));
			if (state.stopping || fs.existsSync(path.join(config.stateDir, "STOP"))) break;
			const wasReset = reset();
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
			if (wasReset) {
				reason = "start";
				continue;
			}
			reason = await waitForWork(config, state, aif);
			if (reason === "stop") break;
		}
	} finally {
		process.off("SIGTERM", requestStop);
		process.off("SIGINT", requestStop);
		process.off("SIGHUP", ignoreHangup);
		process.off("SIGUSR1", requestWake);
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
