# Quill agent guide

Quill is a headless, Claude-only coding-agent CLI. `README.md` holds the decided spec;
`DISCUSSION.md` holds open questions. Quill knows nothing about its callers: no
caller-specific code, names, or concepts.

## Rules

- This repository is public. Never commit API keys, tokens, customer data, or run
  transcripts.
- The API key comes only from `ANTHROPIC_API_KEY` and must never reach tools.
- Live API tests (`go test -tags live ./tests/live/`) cost money. Run them only with the
  owner's approval.
- Keep scope minimal. No convenience features; do not over-claim in docs, PRs, or commits.

## Git and delivery workflow

- Never edit the shared `~/dev/quill` checkout. Create a worktree from `origin/main`:

  ```bash
  git worktree add -b <branch> ~/dev/quill-wt/<branch> origin/main
  ```

- Enable the hooks once per clone: `git config core.hooksPath .githooks`.
- Open PRs against `main`: `gh pr create --base main`. Nothing lands on `main` without a PR.
- Never branch from another feature branch. Do not rebase solely to become current.
- Never force-push or delete `main`.
- Commit, push, and open PRs with the default `ag2trust-dev` identity. Never override the
  token or author as `brevitize`.
- Use conventional commit subjects (`feat:`, `fix:`, `docs:`, `chore:`, `test:`) and end
  commits with the working session's `Co-Authored-By:` trailer.
- Review without checking the PR branch out in the shared checkout. Use `gh pr diff`,
  `git show`, or a throwaway worktree.

### Required verification before a PR

Run the full gate before pushing a PR:

```bash
./preflight.sh
```

It checks the branch base, `gofmt`, `go vet`, `go test`, and static builds for linux/amd64,
linux/arm64, and darwin/arm64. The pre-push hook runs the quick subset (branch base and
`gofmt`); CI runs the rest on Linux and macOS. Never bypass the pre-push hook. Doc-only
changes still run the gate.
