# Open Questions

Subjects to discuss. Nothing here is decided. See "Decided" in `README.md`; when a question is
decided, move it there with the date. Started 2026-10-31, last revised 2026-11-08.

"Leaning" is a starting position to challenge, not a decision.

## 1. Provider

Claude only for now (decided). Remaining:

1. Do the terms allow this use of the Claude API for the intended product?
2. When do other providers come in, and do they go behind the same Quill interface?

## 2. Interface

1. Which fields does each event carry, exactly, beyond `run.completed`?
2. How is the interface versioned, and how does a caller detect an incompatible version? The
   session file has a `format` field; events do not yet.
3. Is a heartbeat event needed during a long shell command, so callers can tell a busy run
   from a hung one?
4. Should the shell command timeout become a flag, or a parameter the model sets per command?

## 3. Scope

1. What does the built-in system prompt say, beyond the safety rules and precedence order?
2. When do MCP, structured output, and a no-tools mode come in?

## 4. Safety

1. Before production: separate user, local proxy, or both, to keep the API key from tools?
2. What network access do tools get? Must package installs and test services be reachable?
3. How is the workspace boundary enforced in Quill and in the container?
4. What does the adversarial test suite contain, and does it block every release?
5. Who performs independent security review?
6. What may be logged, and how long are logs retained?
7. Which actions need a hard block in every profile, for example pushing to Git?

## 5. To verify

Lookups, not discussions.

1. Pin the Anthropic Go SDK. Current docs use the `github.com/anthropics/anthropic-sdk-go/v2`
   module path; confirm on pkg.go.dev.
2. Does hitting a workspace spend limit return 402, or a 400 identified only by its message?
   If 400, map it to `billing`.
3. Check the prior-art table against official docs, especially Claude Code resume.
4. Check `quill` for name collisions; the repo went public on 2026-11-08.
5. Does the API accept consecutive user messages? Resuming after SIGTERM sends the saved
   tool results, then the prompt, as two user messages. `TestLiveResumeAfterInterrupted`
   checks this.
6. Does a high-effort step with `max_tokens` 32,000 finish within the 10-minute per-attempt
   timeout without streaming? If not, consider streaming.
7. After an empty `end_turn` or a `refusal`, does resuming the session work? Quill saves
   neither turn, so the next request may hold two consecutive user messages.
