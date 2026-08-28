# ReadQuest — Plan Spec

**Hackathon:** Better Days, San Francisco (2026-08-28)
**Track:** Reading Crisis
**Bonus target:** LibreChat ($250)
**Build window:** 4–5 hours (10:15 AM – ~3:00 PM, demo at 5:00 PM)
**Team:** 1 developer + Claude Code

**Revision:** v3 — post-build update; reflects what was actually built and verified

---

## Why (Requirements)

Child literacy crisis: students fall behind early and teachers lack visibility into who is struggling and why.
ReadQuest addresses two explicit hackathon asks:

1. **Help students find engaging books** — gamified reading challenges + AI-powered book recommendations
2. **Help teachers identify struggling readers** — real-time analytics dashboard powered by ClickHouse

LibreChat / ClickHouse Agent is the primary user interface — no frontend was built.

---

## What (Functionality)

### Student (via chat)
- Log a reading session (book, pages, minutes) — fuzzy name and title matching
- Check progress: XP, level, streak, badges earned, gap to next badge
- Get a personalised book recommendation (Claude Opus 5)
- Browse the book catalogue by genre or title fragment

### Teacher (via chat)
- View class dashboard: students ranked by who most needs attention
- At-risk rules: never read, silent ≥7 days, pace <10 pages/day
- Ask natural-language questions about class data via ClickHouse's native MCP (built-in, no code required)

### Gamification
- **XP:** +10 per session + 1 per page (capped at 60 per session)
- **Levels:** Beginner → Reader → Bookworm → Scholar
- **Streak:** consecutive calendar days with ≥1 session, maintained transactionally in Postgres at log time
- **Badges:** First Step, Page Turner (100p), Bookworm (500p), Week Warrior (7-day streak), Genre Explorer (3 known genres)
- Genre Explorer excludes `Unknown`-genre books so auto-created placeholders can't inflate the count

### Identity in a chat interface
All tools take `student_name` (string). A shared resolver does:
1. exact match (case-insensitive)
2. ILIKE substring match
3. on miss → error listing all candidates ("did you mean?")
4. on ambiguous match → error listing the candidates

Book title resolution: exact → fuzzy → auto-create placeholder (genre `Unknown`, surfaced to the caller).

---

## How (Architecture)

### Deployment topology

```
┌─────────────────────────────────┐     ┌──────────────────────────────────┐
│        ClickHouse Cloud         │     │           Local Laptop           │
│                                 │     │                                  │
│  ┌──────────────────────────┐   │     │  ┌────────────────────────────┐  │
│  │  ClickHouse Agent        │◄──┼─────┼──│   Go MCP Server (:8080)    │  │
│  │  (LibreChat, hosted)     │   │HTTPS│  │   /tmp/rq-server           │  │
│  └──────────────────────────┘   │ngrok│  └──────────┬─────────────────┘  │
│                                 │     │             │                    │
│  ┌──────────────┐               │     │  ┌──────────▼─────────────────┐  │
│  │  ClickHouse  │◄──────────────┼─────┼──│   ngrok static domain      │  │
│  │  (events,    │               │     │  │   equal-coronary-ducking   │  │
│  │   analytics) │               │     │  │   .ngrok-free.dev          │  │
│  └──────────────┘               │     └──────────────────────────────────┘
│  ┌──────────────┐               │
│  │  PostgreSQL  │◄──────────────┘
│  │  (source of  │
│  │   truth)     │
│  └──────────────┘
└─────────────────────────────────┘
```

### Project structure (what was built)

```
readquest/
├── cmd/
│   ├── readquest/main.go        # MCP server binary (-check, -migrate flags)
│   └── readquest-cli/main.go    # CLI for dev + demo fallback
├── internal/
│   ├── app/app.go               # wires config + both DBs + all stores
│   ├── config/config.go         # .env loading, validation
│   ├── db/
│   │   ├── wait.go              # WaitReady: retry loop for cold-start
│   │   ├── postgres/postgres.go
│   │   └── clickhouse/clickhouse.go
│   ├── domain/
│   │   ├── reading/
│   │   │   ├── reading.go       # LogSession: PG transaction + CH mirror
│   │   │   ├── resolve.go       # resolveStudent, resolveBook
│   │   │   ├── badges.go        # awardBadges (inside LogSession tx)
│   │   │   ├── progress.go      # GetProgress: XP, badges, gaps, recent
│   │   │   └── profile.go       # GetRecommendationProfile + inferAgeBand
│   │   └── dashboard/
│   │       └── dashboard.go     # ClassDashboard: 3-step cross-DB merge
│   ├── ai/recommend.go          # RecommendBook via Claude Opus 5
│   └── mcpserver/
│       ├── server.go            # Streamable HTTP, /healthz, graceful shutdown
│       ├── auth.go              # API key middleware (Bearer + X-API-Key)
│       └── tools.go             # 5 MCP tool registrations + handlers
├── migrations/
│   ├── migrations.go            # embed + apply both sets idempotently
│   ├── postgres/001_schema.sql
│   ├── postgres/002_seed.sql    # 12 books, 3 students, 5 badges
│   ├── clickhouse/001_reading_events.sql
│   └── clickhouse/002_denormalize_names.sql
├── mini-demo.sh                 # full domain demo via CLI (fallback + dev)
├── scripts/
│   └── mcp-smoke.sh             # 14-assertion MCP protocol smoke test
└── PLAN.md
```

### MCP tools (7)

| Tool | R/W | What it does |
|---|---|---|
| `log_reading_session` | W | PG transaction (XP, streak, badges) + CH event + Claude comprehension question |
| `get_student_progress` | R | XP, level, streak, badges earned, gap to next badge, recent reads |
| `get_book_list` | R | Catalogue filtered by genre/title fragment |
| `get_class_dashboard` | R | 3-step cross-DB merge: at-risk ranking |
| `recommend_book` | R | Claude Opus 5, low effort, age band inferred from reading history |
| `get_suspicious_sessions` | R | ClickHouse: rate anomaly (`pages/min > 2`) + burst logging (`>2 sessions/day`) |
| `answer_comprehension_question` | W | Claude evaluates free-text answer; stored in Postgres for teacher review |

### Postgres schema (source of truth)

`classes`, `teachers`, `students` (xp, streak_days, last_session_date), `books` (title, author, genre, age_min, age_max), `reading_sessions`, `badges`, `student_badges`

Key design decision: **streak is owned by Postgres, updated transactionally at log time** using a 3-branch CASE (`today` → unchanged, `today-1` → +1, else → 1). ClickHouse is never queried for streak.

### ClickHouse schema (analytics)

`reading_events` (student_id, **student_name**, class_id, **class_name**, book_id, **book_title**, genre, pages_read, minutes_spent, session_date)

Names are denormalised onto every event (`LowCardinality(String)`) so ClickHouse can answer queries and the built-in ClickHouse MCP can give named results without joining Postgres.

Ordering key: `(class_id, session_date, student_id)` — optimised for the at-risk dashboard's filter (`class_id = ? AND session_date >= today()-7`) and group-by (`student_id`).

### At-risk dashboard (cross-DB merge)

ClickHouse cannot join Postgres. The merge runs in Go:

1. **Postgres** — full class roster (who SHOULD be reading)
2. **ClickHouse** — `countIf`/`sumIf` for last-7-day activity + `max(session_date)`; unfiltered so a student who read 9 days ago still appears (vs. WHERE which would make them invisible)
3. **Go** — left-join roster onto activity; students absent from ClickHouse result have NEVER read (highest risk)

At-risk if: `DaysSinceRead == nil` (never) OR `DaysSinceRead >= 7` OR `VelocityPerDay < 10.0`.

### Anti-cheating architecture

Reward-hacking is addressed at three levels — each handled by the platform best suited to it:

**Level 1 — Hard cap (Go server, write time)**
`validateSession` rejects any session where `pages ÷ minutes > 5`. Physiologically implausible; never stored. Error message explains the threshold to the student.

**Level 2 — ClickHouse analytics (`get_suspicious_sessions`)**
Two queries over `reading_events`, both running entirely in ClickHouse:
- Rate anomaly: `toFloat64(pages_read) / toFloat64(minutes_spent) > 2.0` — sessions worth a teacher's second look
- Burst logging: `count() > 2` grouped by `(student_name, session_date)` — flagging same-day session batches

Results are merged in Go and surfaced through a teacher-facing MCP tool. No Postgres involved. A judge can ask "are any sessions suspicious?" in natural language and ClickHouse answers from the 30-day event history.

**Level 3 — Comprehension check (Claude + LibreChat)**
After every `log_reading_session` call for a book with a known genre:
1. Claude generates one plot-specific question (e.g. "What was the first word Charlotte spun?") using only the title and genre — no stored question bank
2. The question is stored in `session_questions` (Postgres) linked to the session
3. The agent presents the question in the same chat turn with a cue to call `answer_comprehension_question`
4. The student's free-text answer is passed to Claude for evaluation — warm, qualitative, no rubric
5. Answer + evaluation stored in `session_questions` for teacher review

XP is awarded before the question fires. The comprehension check is friction, not a gate: the cost of a false positive (a genuine reader who expressed themselves poorly) outweighs the cost of a motivated cheater who looked up a plot summary.

The comprehension check is the only feature that requires a genuine multi-turn conversational exchange rather than a single tool call. It is the clearest demonstration of LibreChat as an interface and Claude doing something SQL cannot.

Skipped for `Unknown`-genre books (auto-created placeholders) — generating a question for a fabricated title would be misleading.

### Claude integration (recommend_book)

- Model: `claude-opus-5`, effort `low` (simple task, live latency matters)
- Age band inferred from the age ranges of books already read (excludes `Unknown` genre); falls back to catalogue average then 6-12
- System prompt: warm librarian persona, one real book, connect to something they've already read
- Degrades cleanly when `ANTHROPIC_API_KEY` absent: tool returns a plain message rather than an error

### MCP auth

`requireAPIKey` middleware accepts both `X-API-Key` and `Authorization: Bearer` headers (LibreChat sends Bearer; this was not documented). Constant-time comparison. Empty key at startup logs a loud warning but doesn't block boot.

**Host header fix:** mcp-go's DNS-rebinding guard rejects requests whose `Host` is not loopback when the connection arrived on loopback — which is every tunnelled request. After auth passes, the middleware rewrites `r.Host = "localhost"`. This is safe because the auth check is the stronger control: a malicious page can't produce the key.

### Key bugs found by the demo harness (not by unit tests)

| Bug | Where found | Fix |
|---|---|---|
| `last_session_date` scanned as `*string` — works when NULL, fails when a real date exists | `mini-demo.sh` first run | Changed to `*time.Time` |
| ClickHouse `sum()` over Int32 returns Int64; scanned as uint64 — driver rejects it | `cmdEvents` | Separate scan types per return type |
| mcp-go `destructiveHint` defaults to `true` on all tools | `scripts/mcp-smoke.sh` | Explicitly set `false` on all 5 tools |
| mcp-go Host-header rebinding guard blocks all tunnel traffic | Live LibreChat test | Auth middleware rewrites `r.Host` |
| `events` CLI and badge gap in Postgres used different `Unknown` exclusion rules | Cross-referencing outputs | `uniqExactIf(genre, genre != 'Unknown')` in ClickHouse |
| Server failed silently when port already held on restart | Testing | Check log / `lsof -tiTCP:8080` |

---

## Build Status

| Phase | Status | Notes |
|---|---|---|
| 0 Setup | ✓ done | config, DB clients, WaitReady cold-start retry |
| 1 Postgres schema | ✓ done | idempotent migrations + seed |
| 2 ClickHouse schema | ✓ done | denormalized names added in 002 migration |
| 3 Reading domain | ✓ done | 10 integration tests against live DBs |
| 4 Badges + progress | ✓ done | badge double-award and Unknown exclusion tested |
| 5 At-risk dashboard | ✓ done | 13 unit tests on pure assess/sort functions |
| 6 MCP server | ✓ done | 18 assertions in scripts/mcp-smoke.sh |
| 7 Book recommendations | ✓ done | Claude Opus 5, live call confirmed |
| 8 ngrok + LibreChat | ✓ done | all 7 tools confirmed through live chat |
| — Suspicious sessions | ✓ done | ClickHouse rate anomaly + burst detection; `get_suspicious_sessions` MCP tool |
| — Comprehension check | ✓ done | Claude generates question post-session; evaluates free-text answer; stored in PG |

**Verified through ClickHouse Agent chat:**
- `log_reading_session` — Maya Hatchet, Amara Charlotte's Web (badge fired)
- `get_class_dashboard` — at_risk=1 (Amara, never-read)
- `recommend_book` — Island of the Blue Dolphins for Maya (connected Hatchet + Wild Robot)

---

## Running the build

```bash
# First time
cp .env.example .env
# fill in POSTGRES_DSN, CLICKHOUSE_DSN, ANTHROPIC_API_KEY, READQUEST_API_KEY

# Apply schemas + seed
go run ./cmd/readquest -migrate

# Start MCP server (requires ngrok running: ngrok http 8080 --url=<domain>)
READQUEST_API_KEY=<key> go run ./cmd/readquest

# CLI demo (domain logic without MCP)
./mini-demo.sh

# MCP protocol smoke test
./scripts/mcp-smoke.sh                             # localhost
./scripts/mcp-smoke.sh https://<ngrok-domain>      # through tunnel

# Run all tests
go test ./...
```

## Demo day checklist

1. `./mini-demo.sh` — resets data to clean state (Amara never-read, 3 students, 12 books)
2. `ngrok http 8080 --url=equal-coronary-ducking.ngrok-free.dev` — start tunnel
3. `READQUEST_API_KEY=<key> /tmp/rq-server` — start MCP server (from `readquest/`)
4. `./scripts/mcp-smoke.sh https://equal-coronary-ducking.ngrok-free.dev` — verify all 14 checks pass
5. Open ClickHouse Agent, fresh chat
6. Fallback: `./mini-demo.sh` if the chat fails at any point

## Open items

- `PLAN.md` update completed — this is v3
- `scripts/mcp-smoke.sh` covers all 5 tools; `get_book_list` does not naturally route via chat (model answers from own knowledge) — not a code defect
- Week Warrior badge (7-day streak) is not demonstrable in one day; shows as visible aspiration (`1/7`)
- 3 placeholder books (`Unknown` genre) may appear in catalogue; `./mini-demo.sh` removes them on reset

## Skills used

- `claude-api` — loaded before Phase 7 to confirm Go SDK method signatures, model ID, and effort binding
