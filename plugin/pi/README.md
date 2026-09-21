# Pi resident agent

This Pi package starts a detached supervisor which repeatedly runs bounded Pi turns against one
persistent session. Provider, model, thinking level, and AIF MCP identity are explicit launch
arguments; each child loads the required `pi-mcp-adapter` dependency with an isolated in-memory MCP
configuration. The start operation returns the supervisor PID immediately.

## Run it

One-shot launch (Pi exits after spawning the resident):

```bash
pi -e ./plugin/pi \
  --resident-provider freetoken \
  --resident-model qwen3.8-flash-next \
  --resident-thinking medium \
  --resident-system-prompt 'Work autonomously. When you receive SHUTDOWN, finish the current task and terminate.' \
  --resident-mcp-url http://aif.example:18080/mcp \
  --resident-agent-name mybot \
  --resident-agent-token 'aif_...' \
  --resident "Implement the migration, test it, and document the result"
```

Provider, model, system prompt, MCP URL, agent name, and agent token are required.
`--resident-thinking` defaults to `medium`. No environment variables or pre-existing MCP config are
used for these values.

Or load/install the extension with the same `--resident-*` flags and launch from an existing Pi
session:

```text
/resident start --interval 300 --max-turns 24 Implement the migration and stop when tests pass
```

The command reports both a PID and a short resident id. `--max-turns 0` explicitly opts into an
unlimited number of turns; the bounded default is 24. The minimum interval is 10 seconds.

Install it as a local package if desired:

```bash
pi install /absolute/path/to/plugin/pi
```

## Control it

Inside Pi:

```text
/resident list
/resident status ID_OR_PID
/resident wake ID_OR_PID Check the newly-added review comments
/resident stop ID_OR_PID
```

After the parent Pi has exited, use the standalone control script from the project directory:

```bash
node /path/to/plugin/pi/residentctl.mjs list
node /path/to/plugin/pi/residentctl.mjs status PID
node /path/to/plugin/pi/residentctl.mjs wake PID "New instruction"
node /path/to/plugin/pi/residentctl.mjs stop PID
```

Plain `kill PID` also works. `residentctl` and the extension validate `/proc/PID/cmdline` before
signalling, so a stale PID cannot kill an unrelated process.

## Durable state and lifecycle

Each resident owns `${TMPDIR:-/tmp}/pi-resident-ID/` in the system temporary directory:

- `config.json` and `resident.json`: immutable non-secret launch config and current status
- `mcp.json`: private mode-0600 AIF URL and bearer token configuration
- `GOAL.md`, `INBOX.md`, `journal.md`: goal, messages, and durable working memory
- `sessions/`: the resident's Pi session
- `turn-NNNN.jsonl`: complete Pi JSON event logs per turn
- `supervisor.log`: process-level lifecycle log
- `STOP`, `DONE`, `BLOCKED`: lifecycle markers

The repository receives no resident runtime files. The operating system may reclaim all resident
state according to its normal temporary-file policy.

The first turn starts immediately. Later turns run on the configured interval or immediately after
`wake`. The resident exits on `STOP`, `DONE`, `BLOCKED`, the turn limit, SIGTERM, or SIGINT. `DONE`
and `BLOCKED` are checked immediately after every turn, before the next sleep.

The supplied system prompt defines the resident's behavior and may define a semantic termination
command such as `SHUTDOWN`. Deliver that command with `/resident wake ID SHUTDOWN` (or through a
channel the prompt tells the resident to poll). The resident finishes its current unit of work,
writes `DONE`, ends the turn, and the supervisor terminates without scheduling another turn.

## Explicit child configuration

Every child Pi invocation receives the requested provider, model, and thinking level as Pi CLI
arguments. The supplied system prompt is stored in the private state directory and passed with
Pi's `--append-system-prompt`. The child starts with extension discovery disabled and explicitly loads the package's
`resident-child.ts`, and that extension creates `pi-mcp-adapter` from the resident's private MCP
config. Ambient global/project MCP files and globally installed Pi extensions are not used.

The AIF agent name is added to the resident system prompt. The token is written only to the private
temporary MCP file and is never copied into the goal, journal template, status metadata, child
command line, or supervisor log. The initial parent command line necessarily contains the explicit
`--resident-agent-token` argument, so launch it only in an environment where process arguments are
appropriately protected.

The parent conversation is intentionally not copied. A resident receives a clean persistent session
and the explicit goal; silently cloning an interactive conversation would leak unrelated context and
make the resident's behavior hard to audit. The requested provider/model must be available to the
child Pi installation.

## Why a supervisor is necessary

A detached `pi --mode rpc` process is only a server waiting for input; it does not wake itself. A
single `pi -p` process also exits after one agent run. The supervisor is the small missing layer: it
owns scheduling and durable control while every actual reasoning turn remains a normal Pi process.

Autonomous agents can spend money and modify the repository without another confirmation. Start
with a finite turn limit, inspect `journal.md` and the turn logs, and use `stop` when the goal changes.
