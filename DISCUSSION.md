# Open Questions

Subjects to discuss. Nothing here is decided. See "Decided" in `README.md`; when a question is
decided, move it there with the date. Started 2026-10-31, last revised 2026-11-10.

"Leaning" is a starting position to challenge, not a decision.

## 1. Provider

Claude only for now (decided). Remaining:

1. Do the terms allow this use of the Claude API for the intended product?
2. When do other providers come in, and do they go behind the same Quill interface?

## 2. Interface

1. Which fields does each event carry, exactly, beyond `run.completed`?
2. How should a caller react to an event `format` it does not know?
3. Is a heartbeat event needed during a long shell command, so callers can tell a busy run
   from a hung one?
4. Should the shell command timeout become a flag, or a parameter the model sets per command?

## 3. Scope

1. What does the built-in system prompt say, beyond the safety rules and precedence order?
2. When do MCP, structured output, and a no-tools mode come in?

## 4. Safety

1. What network access do tools get? Must package installs and test services be reachable?
2. How is the workspace boundary enforced in Quill and in the container?
3. What does the adversarial test suite contain, and does it block every release?
4. Who performs independent security review?
5. What may be logged, and how long are logs retained?
6. Which actions need a hard block in every profile, for example pushing to Git?

## 5. To verify

Lookups, not discussions.

1. Pin the Anthropic Go SDK. Current docs use the `github.com/anthropics/anthropic-sdk-go/v2`
   module path; confirm on pkg.go.dev.
2. Does hitting a workspace spend limit return 402, or a 400 identified only by its message?
   If 400, map it to `billing`.
3. Check the prior-art table against official docs, especially Claude Code resume.
4. Check `quill` for name collisions; the repo went public on 2026-11-08.
5. Verified 2026-11-08 (live, Haiku 5.5): the API accepts consecutive user messages.
   `TestLiveResumeAfterInterrupted` cancelled a run mid-`shell`, resumed it, and completed.
6. Verified 2026-11-09 (live, one run): a high-effort `claude-opus-5-5` step that hit
   `max_tokens` 32,000 finished in about 5.3 minutes without streaming, about half the
   10-minute per-attempt timeout (about 100 output tokens per second across the run). The
   cut-off tool call was not run, and the model retried in smaller steps. Output speed
   varies with load, so streaming stays the fix if timeouts appear. The SDK estimates 15
   minutes for 32,000 tokens and refuses non-streaming requests over 10 minutes unless a
   request timeout is set, as Quill does.
7. After an empty `end_turn` or a `refusal`, does resuming the session work? Quill saves
   neither turn, so the next request may hold two consecutive user messages. Item 5 shows
   the API accepts that shape; neither stop has been triggered live.
8. Verified 2026-11-08 (live): automatic prompt caching produces cache reads (about 9,800
   tokens read on the second step of `TestLiveCacheReads`).
9. Verified 2026-11-10 (live, one run, Haiku 5.5, high effort, trigger lowered to the
   50,000 minimum): `TestLiveCompaction` compacted twice in a 15-step run and completed.
   Threshold compaction worked on non-streaming requests with client tools, and the full
   history, thinking blocks after the compaction block included, was accepted on every
   later request without the thinking-binding beta. The compaction iteration reports its
   own cache reads and writes as well as input and output. Compaction is a beta API and
   this is one run on one model. The run used the API's default summary prompt; Quill's
   own `instructions` were added afterwards and have not been run live.
10. Compaction, open and unverified:
    - Usage: the docs say only that top-level `input_tokens` and `output_tokens` leave out
      the compaction iteration. Quill assumes the same of the two cache fields and adds all
      four from the compaction iteration. `TestLiveCompaction` now checks that each
      top-level field equals the sum of the `message` iterations; not yet run with that
      check. If the assumption is wrong, cache tokens of a compaction are counted twice.
    - A compaction block with `content: null` (no summary, documented when tools are
      present) has not been seen live. Quill keeps and replays it; the docs say the API
      ignores it and nothing is compacted, so context keeps growing until a later
      compaction succeeds.
    - Compaction on a resumed session has not been run live.
    - The session file and the request body are never trimmed, so both grow without bound
      in a long session, and the request will eventually hit the API's request size limit.
