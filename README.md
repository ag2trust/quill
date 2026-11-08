# Quill

Status: proof of concept, built 2026-11-08. Started 2026-10-31 as a handoff for the next
agent. Not ready for production; see the safety baseline.

This repository is private. Never commit API keys, tokens, customer data, or run transcripts.

Open questions are in [`DISCUSSION.md`](DISCUSSION.md). Anything not listed under "Decided"
below is a proposal, not a decision. When a question is decided, move it here with the date.

## What this is

A very basic, headless coding-agent CLI with no user interface. A program launches it as a
separate process, gives it a task, and reads the result.

Quill is a harness: an API client running a loop. It sends the model the task and the
available tools, runs the tool the model requests, returns the result, and repeats until the
model finishes or a limit is hit. One pass through that loop is a **step**. One Quill process
handling one prompt is a **run**.

## Why

- One CLI and one typed error model, using API keys instead of vendor logins.
- A simple container story: one static binary and no third-party CLIs to install or
  redistribute.
- Typed failures, so callers never have to match CLI error text.

## Decided

### Foundations (2026-10-31)

- Name: Quill. The binary is `quill`.
- Language: Go, for official SDKs, few dependencies, one static binary, and fast builds.
- Private, separate repo.
- No UI. Quill is headless only, with no terminal UI, chat, or permission prompts.
- Quill is unaware of its callers. It has no knowledge of any orchestrator or hosted product,
  and no caller-specific code, names, or concepts. Callers adapt to Quill's generic interface.
- Very basic scope, with high standards on safety. No convenience features.
- Being private is not a security control. The main threat is prompt injection from hostile
  repositories, and the binary runs beside untrusted code. Rely on containment.

### Provider (2026-11-05)

- Claude only for now, through the native Claude Messages API and the official Go SDK. The
  OpenAI-compatible endpoint is not used: it does not support prompt caching and Anthropic
  does not consider it production-ready.
- Prompt caching is on, using automatic caching. To keep the cache valid, within a session:
  - the model, effort, tools, and system prompt never change;
  - history is append-only and replayed byte-for-byte, thinking blocks included;
  - tool output is truncated deterministically.
- Transient provider errors (rate limited, overloaded, network) are retried briefly inside
  the run with the SDK's retries, honouring `retry-after`, within `--timeout`. Longer waits and
  failover belong to the caller.

### Command line (2026-11-07, limit defaults 2026-11-08)

Modelled on `codex exec`, OpenAI's headless CLI.

```
echo "$TASK" | quill exec [flags] -
```

| Flag | Meaning | Default |
|---|---|---|
| positional `PROMPT` or `-` | The prompt. `-` reads it from stdin; callers should use stdin. | required |
| `--workspace DIR` | Directory the tools work in. | current directory |
| `--model ID` | Claude model. | `claude-opus-5-5` |
| `--effort LEVEL` | Model effort. | `high` |
| `--sandbox read-only\|workspace-write` | Tool profile. `read-only` has no write, edit, or shell tools. | `workspace-write` |
| `--max-steps N`, `--max-tokens N`, `--timeout D` | Hard limits for the run. `--max-tokens` counts input, output, and cache-write tokens, not cache reads. | `100`, `2000000`, `30m` |
| `--session FILE` | Session file to create or continue. See Sessions. | none |
| `--env NAME` | Pass this environment variable to tools. Repeatable. | none |

- The API key comes only from `ANTHROPIC_API_KEY`, never from a flag.
- Flags carry options only. The prompt never goes in argv, where it would be visible in `ps`
  and limited in size.

### Tools (2026-11-07, details 2026-11-08)

- `read`, `write`, `edit` (exact text), `list`, `search`, `shell`, and `raise_blocker`.
- Each shell command has a fixed 10-minute timeout. A timed-out command is killed and
  reported to the model as a tool error; the run continues.
- Tool input is checked strictly; unknown fields are an error. The schemas are fixed in
  `internal/tools/registry.go`.
- `read` returns the whole file, or `limit` lines from `offset` (1-based). `write` creates
  parent directories and overwrites. `list` is non-recursive and sorted, directories end
  with `/`. `read`, `write`, and `edit` refuse non-regular files, and `read` and `edit`
  refuse files over 10 MB.
- `edit` replaces `old_text`, which must match exactly one place unless `replace_all` is
  set. Otherwise it is a tool error saying the text was not found or how many places
  matched.
- `search` takes a Go regular expression and an optional path. It skips `.git`, symlinks,
  binary files, and files over 1 MB, cuts lines at 500 bytes, and stops after 200 matches.
- Tool output over 30,000 bytes keeps the first and last 15,000 bytes around a marker
  giving the number of bytes left out. The same output is always cut the same way, so the
  cache stays valid.
- The model may request several tools in one response. Quill runs them one at a time, in
  order.

### Sessions (2026-11-07)

Quill is stateless. One process handles one run and then exits, whatever the outcome. To
continue, the caller starts a new run with the same session file.

- `--session FILE`: if the file exists, Quill continues from it and appends the new turn. If
  it does not exist, Quill creates it. Without `--session`, the run is one-off and nothing is
  written.
- Quill refuses a session path inside the workspace, so the file tools cannot edit it. The
  shell runs as the same user, so containment is the container's job (see the safety
  baseline).
- Format: JSON lines. The first line holds settings fixed for the session. Each later line is
  one message in Claude API format, exactly as sent or received.

  ```jsonl
  {"type":"session","format":1,"model":"claude-opus-5-5","effort":"high","sandbox":"workspace-write","system":"<full system prompt>","tools":[...]}
  {"type":"message","message":{"role":"user","content":[{"type":"text","text":"Fix the failing test"}]}}
  ```

- Continuing with a different `--model`, `--effort`, or `--sandbox` set explicitly is bad
  usage (exit 2); unset flags take the session's values.
- Quill appends after each completed step, so a crash leaves a valid file. It locks the file
  so two runs cannot share it, and creates it with mode `0600`. The file holds prompts and
  file contents; callers treat it as sensitive.
- The file does not contain the API key, usage, or events.
- An assistant reply with no content, and a refused turn, are not saved: the API would reject
  them in the next request.

### Blockers (2026-11-07)

When the task conflicts with repository guidance, or is unclear and a guess could cause
damage, the agent stops instead of guessing.

- The model calls `raise_blocker` with `reason` (`conflict`, `unclear_task`,
  `missing_access`, or `unsafe`), `summary`, `details` (quoting both sides of a conflict), and
  `question`.
- Quill runs no further tools, saves the session, reports status `blocked`, and exits 5.
- The caller resolves it and continues with the same session file, sending the answer as the
  prompt. Quill delivers the answer as the result of the `raise_blocker` call. Other tool
  calls from that same response get the result "not run: paused".
- Quill does not decide what counts as a conflict, and does not notify or wait for anyone.

### Repository instructions (2026-11-07)

- Quill reads `AGENTS.md` and `CLAUDE.md` from the workspace root, if present, capped at
  32 KB each. They go into the first user message, marked as repository content, never into
  the system prompt. When `CLAUDE.md` is the same file as `AGENTS.md`, it is included once.
- The built-in system prompt holds Quill's safety rules and sets this precedence:
  1. Quill's safety rules always win.
  2. The task decides what to do.
  3. Repository guidance decides how: conventions, commands, invariants.
  4. If the task requires breaking a stated invariant, the agent calls `raise_blocker`.

### Environment for tools (2026-11-07)

- Tools start from a clean environment: `PATH`, `HOME`, `USER`, `LANG`, `TMPDIR`, plus each
  variable named with `--env`. Quill copies the value from its own environment.
- `--env ANTHROPIC_API_KEY` is refused.
- `run.started` lists the names passed, so a missing variable is easy to spot. Unset or
  empty variables are not passed and are missing from `env_passed`.
- This stops accidental leaks only. A same-user tool can still read the key from Quill's
  process. See the safety baseline.

### Cleanup and stopping (2026-11-07)

- Each shell command runs in its own process group, tracked by Quill.
- On SIGTERM, SIGINT, SIGHUP, or run timeout, Quill stops every command group (SIGTERM, then
  SIGKILL after a few seconds), writes `run.completed` with status `cancelled` or
  `limit_reached`, and exits.
- A run stopped during tool calls saves its tool results: tools that finished get their real
  results, the call in progress gets "interrupted: it may have partly run", and the rest get
  "not run: cancelled". If Quill crashed instead, continuing the session gives every call
  left without a result "interrupted: it may have partly run".
- Quill removes everything it created: processes, temporary files, and temporary directories.
  The agent's changes to the workspace are the result of the task and stay.
- Callers stop Quill with SIGTERM, and SIGKILL only if it has not exited after about 10
  seconds. SIGKILL cannot be caught; the container is the last backstop.

### Output (2026-11-07)

- stdout is always JSON lines. stderr is for human diagnostics only.
- Events: `run.started`, `item.started`, `item.completed` (assistant messages and tool calls
  with name and input), and `run.completed`.
- Exactly one `run.completed`, always the last line, except after SIGKILL. No `run.completed`
  means Quill crashed, except exit 2, which reports on stderr only. A Go runtime crash also
  exits 2, but normally after `run.started`, so stdout is non-empty and has no
  `run.completed`. A SIGTERM, SIGINT, or SIGHUP before the prompt is read ends Quill with
  no output.

```json
{"type":"run.completed","status":"completed","final_text":"Fixed the flaky test by ...",
 "usage":{"input":12000,"output":3400,"cache_read":410000,"cache_write":15000},
 "steps":23,"duration_ms":184000}
```

- `usage` covers the whole run. `final_text` is the agent's last message, even when the run
  did not succeed. A failed run adds `error`; a blocked run adds `blocker`.
- `status` is authoritative. The exit code summarises it for scripts:

| Exit | `status` | Meaning |
|---|---|---|
| 0 | `completed` | Task done |
| 1 | `internal_error` | Bug in Quill |
| 2 | (none, before the run starts) | Bad usage |
| 3 | `limit_reached` | Steps, tokens, or time ran out |
| 4 | `provider_error` | See `error.kind` |
| 5 | `blocked` | Waiting for input. See `blocker` |
| 143 | `cancelled` | Stopped by SIGTERM, SIGINT or SIGHUP |

- Provider errors are classified from HTTP status and API error type only, never from text.
  Each `error` also carries `http_status`, the API's `type`, `message`, and `request_id`.

| `error.kind` | From |
|---|---|
| `auth` | 401, 403 |
| `billing` | 402 |
| `rate_limited` | 429, with `retry_after` |
| `overloaded` | 500, 529, other 5xx |
| `network` | Connection failures and timeouts |
| `model_unavailable` | 404 on the model |
| `bad_request` | 400, 413 |
| `refused` | Stop reason `refusal` |

### First version scope (2026-11-07)

- One autonomous coding task per run, in a git workspace, using the tools above.
- Not in the first version: MCP client, skills, a no-tools mode, structured JSON output,
  context compaction, cost in USD, and other providers.
- Out of scope entirely: plugins, subagents, web search, image input, memory between
  sessions, and orchestration. Orchestration belongs to the caller.

## Proposed safety baseline

- Deny by default: anything outside the selected tool profile is refused.
- Tool output and repository files are untrusted. They never gain authority.
- Reject paths that resolve outside the workspace, including through symlinks.
- The container is the sandbox: non-root, read-only image, CPU, memory, and process limits,
  and restricted network.
- Run Quill under an init in containers (`docker run --init` or tini) so orphaned processes
  are reaped.
- API keys must not reach tools. The proof of concept only keeps the key out of the tool
  environment. That is not enough, because a same-user process can inspect Quill. Before
  production, run tools as a separate user, or send model calls through a small proxy.
- A shell cannot be made read-only by rules, so the read-only profile has no shell.
- Fail closed on unknown tools, malformed responses, and policy errors.
- No prompts, file contents, or secrets in logs. The stdout event stream is the caller's
  data, not a log.
- Pinned dependencies, few of them, and `govulncheck`.
- A permanent adversarial suite: prompt injection, path escape, and secret theft.

## Prior art: headless CLI interfaces

There is no shared convention in use.

| | Claude Code | Codex | Grok Build |
|---|---|---|---|
| Headless command | `claude -p` | `codex exec` | `grok -p` |
| Events | `--output-format stream-json --verbose` | `--json` | `--output-format streaming-json` |
| Prompt input | `--input-format stream-json` on stdin | Argument | `-p` argument |
| Session ID | Caller generates it, `--session-id` | Issued by CLI in `thread.started` | Issued by CLI in terminal `end` event |
| Resume | Not recorded | `exec resume <id>` | `--resume <id>` |
| Tool limits | `--permission-mode`, `--allowedTools` | `-s <sandbox>` | `--permission-mode`, `--sandbox` |
| MCP | `--mcp-config --strict-mcp-config` | `-c mcp_servers.<name>=...` | Config file |
| Final outcome | `result` event, `subtype` | `turn.completed` or `turn.failed` | `end` event, `stopReason` |

The common shape is one process per run, JSON lines on stdout, an exit status, and a
resumable session. The details all differ. None of them has a "blocked, waiting for input"
outcome.

A shared standard does exist: the Agent Client Protocol (ACP), JSON-RPC over stdio. It is
aimed at editors. Sources found so far are secondary and say Claude Code and Codex need
adapters. Codex also has a JSON-RPC app-server, but OpenAI marks it experimental and not for
production workloads.

## Planning context

Adoption planning for each caller lives with that caller, not here. Nothing in Quill may
depend on it.
