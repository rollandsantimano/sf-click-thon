#!/usr/bin/env bash
#
# ReadQuest mini-demo — exercises every feature built so far.
#
# Two jobs: a smoke test after each build phase, and the fallback demo if
# ngrok or the hosted chat UI misbehaves on the day. Grows one section per
# phase.
#
# Run from the readquest directory (the CLI reads .env from the working dir):
#     ./mini-demo.sh

set -uo pipefail
cd "$(dirname "$0")"

# Build once. Eight `go run` invocations recompile eight times, which is
# noticeable dead air in front of an audience.
BIN=$(mktemp -d)/readquest-cli
echo "building..."
go build -o "$BIN" ./cmd/readquest-cli || exit 1
trap 'rm -rf "$(dirname "$BIN")"' EXIT

# stderr is NOT suppressed. The CLI already filters its own logs to WARN, so
# anything on stderr is either a real problem or — for the expect_failure
# cases — the error message that is the whole point of the step.
rq() { "$BIN" "$@"; }

step() { printf '\n\033[1m=== %s ===\033[0m\n' "$*"; }

# expect_failure runs a command that SHOULD fail, so an intentional error
# never looks like a broken demo. The error text is shown, because the quality
# of that message is what is being demonstrated.
expect_failure() {
  local label=$1; shift
  printf '\n\033[1m=== %s \033[0;33m(expected to fail)\033[0m\033[1m ===\033[0m\n' "$label"
  if "$BIN" "$@"; then
    printf '\033[0;31mUNEXPECTED: that was supposed to fail\033[0m\n'
    return 1
  fi
  return 0
}

# ---------------------------------------------------------------- phase 0-2
step "Clean slate"
rq reset

step "Seeded roster"
rq students

step "Catalogue — fantasy titles"
rq books fantasy

# ------------------------------------------------------------------ phase 3
step "Log a session (fuzzy match: 'maya' -> Maya Chen, 'matilda' -> Matilda)"
rq log maya matilda 45 30

step "Second session same day — streak must stay at 1"
rq log maya holes 30 25

step "Crossing 100 XP — level becomes Reader"
rq log maya "the wild robot" 40 35

step "Book outside the catalogue — auto-created as 'Unknown'"
rq log diego "Dog Man Unleashed" 60 40

expect_failure "Unknown student — error should list candidates" \
  log "Bobby Tables" matilda 20 20

expect_failure "Implausible input — zero pages rejected" \
  log maya matilda 0 20

# ------------------------------------------------------------------ phase 4
step "Badges — Maya has 115 pages across 3 genres, so two should unlock"
rq progress maya

step "A locked badge shows the remaining gap, not just a closed door"
rq progress diego

step "Genre Explorer needs 3 KNOWN genres — Diego will reach 3 books but only 2 count"
rq log diego "Frog and Toad Are Friends" 20 15
rq log diego "Bridge to Terabithia" 25 20

step "Postgres state — source of truth"
rq students

step "ClickHouse rollup — analytics mirror, merged with the Postgres roster"
rq events

# ------------------------------------------------------------------ phase 5
step "Teacher dashboard — the cross-database merge, ranked by who needs attention"
rq dashboard

printf '\n\033[1mDone.\033[0m Amara Okafor is deliberately left with no activity —\n'
printf 'that is the at-risk case the Phase 5 dashboard has to surface.\n\n'
