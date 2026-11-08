#!/bin/sh
# preflight.sh — mechanical author gate before pushing or opening a PR.
#
# Gates (in order, fail-fast):
#   1. branch base — the branch's own commits sit on origin/main, not on another
#                    feature branch
#   2. gofmt       — no unformatted files
#   3. go vet      — ./...
#   4. go test     — ./... -count=1
#   5. build       — CGO_ENABLED=0 for linux/amd64, linux/arm64, darwin/arm64
#
# Usage:
#   ./preflight.sh                     # all gates
#   ./preflight.sh --quick             # gates 1+2 (what the pre-push hook runs)
#   ./preflight.sh --ci                # gates 2-5 (CI has no branch context)
#   ./preflight.sh --quick --format-only        # gate 2 only (tag pushes)
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
