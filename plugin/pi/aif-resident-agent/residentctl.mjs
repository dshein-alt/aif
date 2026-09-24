#!/usr/bin/env node

import * as fs from "node:fs";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

import { enqueueMessage, requestControl, contextDescription } from "./control.mjs";

const runner = path.join(path.dirname(fileURLToPath(import.meta.url)), "runner.mjs");
const residentPrefix = "pi-resident-";

function list(cwd) {
	const root = tmpdir();
	if (!fs.existsSync(root)) return [];
	return fs.readdirSync(root, { withFileTypes: true }).flatMap((entry) => {
		if (!entry.isDirectory() || !entry.name.startsWith(residentPrefix)) return [];
		try {
			const resident = JSON.parse(fs.readFileSync(path.join(root, entry.name, "resident.json"), "utf8"));
			return path.resolve(resident.cwd) === path.resolve(cwd) ? [resident] : [];
		} catch {
			return [];
		}
	});
}

function find(cwd, selector) {
	const resident = list(cwd).find((item) => item.id === selector || String(item.pid) === selector);
	if (!resident) throw new Error(`resident not found: ${selector}`);
	return resident;
}

function isLive(resident) {
	try {
		const parts = fs.readFileSync(`/proc/${resident.pid}/cmdline`, "utf8").split("\0");
		return parts.includes(runner) && parts.includes(path.join(resident.stateDir, "config.json"));
	} catch {
		return false;
	}
}

function describe(resident) {
	return `${resident.id} pid=${resident.pid} live=${isLive(resident)} status=${resident.status} turn=${resident.turn} state=${resident.stateDir} ${contextDescription(resident.stateDir)}`;
}

function main() {
	const [command = "list", selector, ...rest] = process.argv.slice(2);
	const cwd = process.cwd();
	if (command === "list") {
		for (const resident of list(cwd)) process.stdout.write(`${describe(resident)}\n`);
		return;
	}
	if (!selector) throw new Error(`usage: residentctl.mjs ${command} ID_OR_PID`);
	const resident = find(cwd, selector);
	if (command === "status") {
		process.stdout.write(`${describe(resident)}\n`);
		return;
	}
	if (command === "stop") {
		fs.writeFileSync(path.join(resident.stateDir, "STOP"), `${new Date().toISOString()} requested by residentctl\n`);
		if (isLive(resident)) process.kill(resident.pid, "SIGTERM");
		process.stdout.write(`stop requested for ${resident.id} (PID ${resident.pid})\n`);
		return;
	}
	if (command === "reset" || command === "compact") {
		if (!isLive(resident)) throw new Error("resident is not running; control commands do not restart stopped residents");
		requestControl(resident.stateDir, command.toUpperCase());
		process.kill(resident.pid, "SIGUSR1");
		process.stdout.write(`${command} requested for ${resident.id}; applies between turns.\n`);
		return;
	}
	if (command === "wake") {
		const message = rest.join(" ").trim();
		if (!message) throw new Error("wake requires a message");
		enqueueMessage(resident.stateDir, message);
		if (isLive(resident)) process.kill(resident.pid, "SIGUSR1");
		process.stdout.write(`message queued for ${resident.id} (PID ${resident.pid})\n`);
		return;
	}
	throw new Error(`unknown command: ${command}`);
}

try {
	main();
} catch (error) {
	process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
	process.exitCode = 1;
}
