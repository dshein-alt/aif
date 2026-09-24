import assert from "node:assert/strict";
import test from "node:test";
import { residentContract } from "../aif-resident-agent/index.ts";

// index.ts keeps only type-only imports from pi, so the contract is reachable without the host loader.
function contract(overrides = {}) {
	return residentContract({
		provider: "local",
		model: "test-model",
		thinkingLevel: "low",
		systemPrompt: "ROLE TEXT",
		mcpUrl: "http://127.0.0.1:1/mcp",
		agentName: "Tess",
		agentToken: "aif_test",
		thread: 3,
		operators: ["TheRoot", "gatekeeper"],
		...overrides,
	});
}

// Steps are the numbered turn loop; the prose after them is not ordered instructions.
function steps(text) {
	const body = text.split("Every turn, in this order:\n")[1];
	assert.ok(body, "contract must contain a numbered turn loop");
	const out = new Map();
	for (const line of body.split("\n")) {
		const m = line.match(/^(\d+)\. /);
		if (m) out.set(Number(m[1]), line);
	}
	return out;
}

test("turn loop is numbered from 1 with no gaps or duplicates", () => {
	const numbered = steps(contract());
	assert.deepEqual(
		[...numbered.keys()],
		[1, 2, 3, 4, 5, 6, 7, 8, 9],
		`turn steps must be contiguous, got ${[...numbered.keys()].join(",")}`,
	);
});

test("identity is confirmed before any inbox work", () => {
	const numbered = steps(contract());
	assert.match(numbered.get(1), /^1\. Call whoami first\./);
	assert.match(numbered.get(1), /claim_required/);
});

test("the inbox is read without advancing the cursor", () => {
	const read = steps(contract()).get(2);
	assert.match(read, /"tool":"unread","args":\{"advance":0\}/);
	assert.ok(!read.includes('"tool":"seen"'), "the read step must not also clear the inbox");
	assert.match(read, /gone forever/);
	assert.match(read, /highest message id/);
});

test("clearing happens only after replies, and never before them", () => {
	const numbered = steps(contract());
	const read = numbered.get(2);
	const replies = numbered.get(4);
	const clear = numbered.get(6);
	// The reply step must precede the clearing step, and must not itself clear.
	assert.ok(!read.includes('"tool":"seen"') && !replies.includes('"tool":"seen"'),
		"no step before the clearing step may move the read cursor");
	assert.match(clear, /"tool":"seen","args":\{"seq":<highest message id handled>\}/);
	// A reply that could not be posted has to leave the inbox unread so it is redelivered.
	assert.match(replies, /If a reply cannot be posted, stop before step 6/);
	assert.match(replies, /before this turn ends/);
	assert.match(clear, /If a reply is still owed, clear only up to the id just below the oldest owed message/);
});

test("clearing is always bounded: seq 0 is banned and every seq is an id, never a literal", () => {
	const text = contract();
	assert.match(text, /Never pass seq 0 \(it clears the whole forum\)/);
	let seenCalls = 0;
	for (const line of steps(text).values()) {
		for (const m of line.matchAll(/"tool":"seen","args":\{"seq":([^}]*)\}/g)) {
			seenCalls++;
			assert.match(m[1], /^</, `a clearing step must name a bounded id, got ${m[1]}`);
		}
	}
	assert.equal(seenCalls, 2, "expected exactly the RESET clear and the end-of-turn clear");
});

test("a RESET request clears its own message before resetting", () => {
	const reset = steps(contract()).get(5);
	const clearAt = reset.indexOf('"tool":"seen"');
	const resetAt = reset.indexOf("resident_memory action reset");
	assert.ok(clearAt >= 0, "the RESET step must clear the cursor it just read up to");
	assert.ok(resetAt > clearAt, "RESET must clear first so the fresh session does not replay the request");
	assert.match(reset, /Do not create DONE/);
});

test("SHUTDOWN still outranks replying and clearing", () => {
	const numbered = steps(contract());
	assert.ok(numbered.get(3).includes("SHUTDOWN"), "step 3 stays the SHUTDOWN check");
	assert.match(numbered.get(3), /post "Goodbye from Tess: SHUTDOWN received\."|Goodbye from Tess/);
	assert.match(numbered.get(3), /Do nothing else/);
});

test("a reply exists only as a post that returned an id", () => {
	assert.match(contract(), /A reply exists only when you called mcp__aif with tool "post" and got back an id/);
});

test("unread names seen as the clearing tool and the home thread is honoured", () => {
	const text = contract();
	assert.match(text, /mcp__aif\(\{"tool":"seen","args":\{"seq":<id>\}\}\)/);
	assert.match(text, /Your home thread is 3\./);
	assert.match(text, /ROLE \(supplied by the operator\):\nROLE TEXT$/);
});
