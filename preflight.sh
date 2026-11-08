#!/bin/sh
# preflight.sh — mechanical author gate before pushing or opening a PR.
#
# Gates (in order, fail-fast):
#   1. branch base — the branch's own commits sit on origin/main, not on another
#                    feature branch
#   2. secrets     — no credential-shaped strings in the tree or unpublished commits
#                    (this repo is public)
#   3. gofmt       — no unformatted files
#   4. go vet      — ./...
#   5. go test     — ./... -count=1
#   6. build       — CGO_ENABLED=0 for linux/amd64, linux/arm64, darwin/arm64
#
# Usage:
#   ./preflight.sh                     # all gates
#   ./preflight.sh --quick             # gates 1-3 (what the pre-push hook runs)
#   ./preflight.sh --ci                # gates 2-6 (CI has no branch context)
#   ./preflight.sh --quick --format-only        # gates 2+3 (tag pushes)
#   ./preflight.sh --quick --tip=SHA --target=BRANCH   # pre-push hook form
#
# Do not bypass the pre-push hook with --no-verify; fix the cause instead.

set -u

QUICK=0
CI=0
FORMAT_ONLY=0
TIP=HEAD
TARGET=
for arg in "$@"; do
  case "$arg" in
    --quick) QUICK=1 ;;
    --ci) CI=1 ;;
    --format-only) FORMAT_ONLY=1 ;;
    --tip=*) TIP=${arg#--tip=} ;;
    --target=*) TARGET=${arg#--target=} ;;
    *) printf 'preflight.sh: unknown argument: %s\n' "$arg" >&2; exit 2 ;;
  esac
done

fail() {
  printf 'preflight: FAIL: %s\n' "$1" >&2
  exit 1
}

REPO_ROOT=$(git rev-parse --show-toplevel) || exit 1
cd "$REPO_ROOT" || exit 1

branch_base() {
  printf '=== preflight: branch base ===\n'
  [ -n "$TARGET" ] || TARGET=$(git rev-parse --abbrev-ref HEAD)
  git cat-file -e "$TIP^{commit}" 2>/dev/null || fail "tip $TIP is not a commit"
  git fetch origin --quiet || fail "git fetch origin"
  if ! git rev-parse --verify --quiet origin/main >/dev/null; then
    [ "$TARGET" = main ] || fail "origin/main not found; push main first"
    printf 'first push of main — nothing to compare\n'
    return
  fi
  OWN=$(git rev-list --count "$TIP" --not origin/main)
  printf '%s commit(s) not on origin/main\n' "$OWN"
  [ "$OWN" -gt 0 ] || return
  # A branch cut from another feature branch carries that branch's commits.
  # Compare against every other remote branch; the push target is excluded
  # because it holds this branch's own published history.
  for ref in $(git for-each-ref --format='%(refname:short)' refs/remotes/origin); do
    case "$ref" in
      origin/main | origin/HEAD | origin | "origin/$TARGET") continue ;;
    esac
    NOT_SHARED=$(git rev-list --count "$TIP" --not origin/main "$ref")
    [ "$NOT_SHARED" -eq "$OWN" ] \
      || fail "branch shares commits with $ref; cut it from origin/main"
  done
  printf 'ok\n'
}

# Credential shapes: Anthropic, GitHub, AWS, Slack, private keys.
SECRET_RE='sk-ant-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{40,}|AKIA[0-9A-Z]{16}|xox[abposr]-[A-Za-z0-9-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----'

secrets_gate() {
  printf '=== preflight: secrets ===\n'
  # The tree being pushed, plus every unpublished commit's patch and message:
  # a secret added and later deleted is still published with the history.
  if git grep -I -q -E "$SECRET_RE" "$TIP" -- . 2>/dev/null; then
    git grep -I -l -E "$SECRET_RE" "$TIP" -- . >&2
    fail "credential-shaped string in the tree (files above)"
  fi
  if git rev-parse --verify --quiet origin/main >/dev/null; then
    git log -p --format='%H%n%B' "$TIP" --not origin/main | grep -q -E "$SECRET_RE" \
      && fail "credential-shaped string in an unpublished commit; rewrite it before pushing"
  fi
  printf 'ok\n'
}

gofmt_gate() {
  printf '=== preflight: gofmt ===\n'
  UNFORMATTED=$(gofmt -l .) || fail "gofmt"
  [ -z "$UNFORMATTED" ] || fail "unformatted files (run gofmt -w):
$UNFORMATTED"
  printf 'ok\n'
}

if [ "$FORMAT_ONLY" -eq 0 ] && [ "$CI" -eq 0 ]; then
  branch_base
fi
secrets_gate
gofmt_gate
[ "$QUICK" -eq 1 ] && exit 0

printf '=== preflight: go vet ===\n'
go vet ./... || fail "go vet"

printf '=== preflight: go test ===\n'
go test -count=1 ./... || fail "go test"

printf '=== preflight: build ===\n'
for target in linux/amd64 linux/arm64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -o /dev/null ./cmd/quill \
    || fail "build $target"
  printf '%s ok\n' "$target"
done

printf 'preflight: all gates passed\n'
