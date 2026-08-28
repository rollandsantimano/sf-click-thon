#!/usr/bin/env bash
#
# ReadQuest MCP smoke test — exercises the protocol path LibreChat will use.
#
# Runs against either target, which is the point: if localhost passes and the
# tunnel fails, the fault is in the tunnel, not the server. Isolate that before
# adding LibreChat as a third variable.
#
#     ./mcp-smoke.sh                                   # local  (default)
#     ./mcp-smoke.sh https://your-domain.ngrok-free.dev # tunnel
#
# Reads READQUEST_API_KEY from the environment or .env.

set -uo pipefail
cd "$(dirname "$0")"

BASE=${1:-http://localhost:8080}
MCP="$BASE/mcp"

# Prefer an exported key; fall back to .env so this matches what the server read.
KEY=${READQUEST_API_KEY:-$(grep -E '^READQUEST_API_KEY=' .env 2>/dev/null | sed -E 's#^READQUEST_API_KEY=##')}
if [[ -z "${KEY:-}" ]]; then
  echo "READQUEST_API_KEY is not set (export it, or put it in .env)" >&2
  exit 1
fi

pass=0; fail=0
ok()   { printf '  \033[0;32mPASS\033[0m  %s\n' "$1"; pass=$((pass+1)); }
bad()  { printf '  \033[0;31mFAIL\033[0m  %s\n' "$1"; fail=$((fail+1)); }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

rpc() {
  curl -s -m 30 -X POST "$MCP" \
    -H "X-API-Key: $KEY" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d "$1"
}

code() { # method-agnostic status probe
  curl -s -o /dev/null -w '%{http_code}' -m 30 "$@"
}

printf '\033[1mReadQuest MCP smoke test\033[0m — %s\n' "$BASE"

# --- reachability -----------------------------------------------------------
step "1. Reachability"
health=$(curl -s -m 20 "$BASE/healthz" 2>/dev/null)
if [[ "$health" == "ok" ]]; then
  ok "/healthz reachable (tunnel + port are correct)"
else
  bad "/healthz did not return ok — got: ${health:0:120}"
  echo
  echo "  If this says ERR_NGROK_8012, ngrok is pointed at the wrong port."
  echo "  Restart it with: ngrok http 8080"
  exit 1
fi

# --- auth -------------------------------------------------------------------
step "2. Authentication"
[[ $(code -X POST "$MCP" -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}') == 401 ]] \
  && ok "no key rejected with 401" || bad "missing key was NOT rejected"

[[ $(code -X POST "$MCP" -H 'X-API-Key: wrong-key' -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}') == 401 ]] \
  && ok "wrong key rejected with 401" || bad "wrong key was NOT rejected"

# Both header spellings must work: LibreChat's API Key field does not document
# which one it sends.
[[ $(code -X POST "$MCP" -H "X-API-Key: $KEY" -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}') == 200 ]] \
  && ok "X-API-Key accepted" || bad "X-API-Key rejected"

[[ $(code -X POST "$MCP" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}') == 200 ]] \
  && ok "Authorization: Bearer accepted" || bad "Authorization: Bearer rejected"

# --- protocol ---------------------------------------------------------------
step "3. MCP protocol"
init=$(rpc '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}')
name=$(jq -r '.result.serverInfo.name // empty' <<<"$init")
[[ "$name" == "readquest" ]] \
  && ok "initialize handshake ($(jq -r '.result.protocolVersion' <<<"$init"))" \
  || bad "initialize failed — got: ${init:0:160}"

tools=$(rpc '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq -r '.result.tools[].name' | sort)
count=$(wc -l <<<"$tools" | tr -d ' ')
[[ "$count" == 5 ]] && ok "5 tools advertised" || bad "expected 5 tools, got $count"
sed 's/^/        /' <<<"$tools"

# A read-only tool marked destructive makes some clients demand confirmation
# before every harmless lookup, which is a bad demo.
destructive=$(rpc '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | jq -r '[.result.tools[] | select(.annotations.destructiveHint == true)] | length')
[[ "$destructive" == 0 ]] \
  && ok "no tool falsely marked destructive" \
  || bad "$destructive tool(s) marked destructive — clients may prompt on every call"

# --- tools ------------------------------------------------------------------
step "4. Tool calls"
call() { rpc "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2}}"; }
text() { jq -r '.result.content[0].text // empty'; }

out=$(call get_class_dashboard '{}' | text)
grep -q 'Students, ranked' <<<"$out" && ok "get_class_dashboard" || bad "get_class_dashboard — ${out:0:120}"

out=$(call get_book_list '{"filter":"fantasy"}' | text)
grep -q 'Matilda' <<<"$out" && ok "get_book_list" || bad "get_book_list — ${out:0:120}"

out=$(call get_student_progress '{"student_name":"maya"}' | text)
grep -q 'Maya Chen' <<<"$out" && ok "get_student_progress (fuzzy name)" || bad "get_student_progress — ${out:0:120}"

out=$(call log_reading_session '{"student_name":"maya","book_title":"hatchet","pages_read":12,"minutes_spent":10}' | text)
grep -q 'Session logged' <<<"$out" && ok "log_reading_session (writes both DBs)" || bad "log_reading_session — ${out:0:120}"

# Recommendations need ANTHROPIC_API_KEY; report the state rather than failing,
# since every other tool works without it.
out=$(call recommend_book '{"student_name":"maya"}' | text)
if grep -q 'not available' <<<"$out"; then
  printf '  \033[0;33mSKIP\033[0m  recommend_book — no ANTHROPIC_API_KEY set (degrades cleanly)\n'
elif [[ -n "$out" ]]; then
  ok "recommend_book returned a suggestion"
  sed 's/^/        /' <<<"${out:0:300}"
else
  bad "recommend_book returned nothing"
fi

# --- error handling ---------------------------------------------------------
step "5. Error recovery (the model must be able to self-correct)"
out=$(call get_student_progress '{"student_name":"Bobby Tables"}' | jq -r '.result.content[0].text // .error.message')
grep -q 'did you mean' <<<"$out" && ok "unknown student returns candidates" || bad "no candidates offered — ${out:0:120}"

out=$(call log_reading_session '{"student_name":"maya","book_title":"matilda","pages_read":0,"minutes_spent":10}' | jq -r '.result.content[0].text // .error.message')
grep -q 'greater than zero' <<<"$out" && ok "invalid input rejected with a reason" || bad "bad input not rejected — ${out:0:120}"

# --- summary ----------------------------------------------------------------
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
if [[ $fail -gt 0 ]]; then
  echo
  echo "If localhost passes but the tunnel fails, the fault is between ngrok and"
  echo "the server — check the ngrok inspector at http://localhost:4040"
  exit 1
fi
echo
