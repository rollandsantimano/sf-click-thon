# ReadQuest

A gamified reading challenge app for child literacy, built for the
[Better Days hackathon](https://lu.ma/clickh-sie8) (San Francisco, 2026-08-28),
**Reading Crisis track**.

Students log reading sessions through a chat interface, earn XP and badges,
and get AI-powered book recommendations. Teachers ask in plain language who in
their class is falling behind — and get a ranked list backed by real analytics.

---

## The problem

Children fall behind in reading early, and teachers rarely have the visibility
to catch it in time. Existing tools require manual data entry, produce reports
no one reads, and give students no reason to care.

ReadQuest addresses two explicit hackathon asks:

1. **Help students find engaging books** — gamified challenges + Claude-powered
   personalised recommendations
2. **Help teachers identify struggling readers** — a real-time at-risk
   dashboard driven by ClickHouse analytics

---

## How it works

Everything happens through **ClickHouse Cloud's hosted LibreChat** (branded as
"ClickHouse Agent"). A student says "I read 30 pages of Hatchet today" in the
chat window. A teacher asks "who in my class needs help?" The agent calls the
right tool, gets a structured answer, and narrates it.

There is no web front end. The chat window _is_ the interface.

```
[Student / Teacher]
      │ natural language
      ▼
[ClickHouse Agent]   ← hosted LibreChat on ClickHouse Cloud
      │ MCP (Streamable HTTPS)
      │ Authorization: Bearer <key>
      ▼
[ReadQuest Go server]   ← runs locally, exposed via ngrok
      │                    │
      ▼                    ▼
[PostgreSQL]         [ClickHouse]
source of truth      analytics + events
users, books,        reading_events
badges, streaks      at-risk dashboard
```

### Why both databases?

| | PostgreSQL | ClickHouse |
|---|---|---|
| **Owns** | Students, books, sessions, badges, streaks | Reading event stream |
| **Strength** | Transactional writes, foreign keys | Aggregations over millions of rows |
| **Used for** | Everything that must agree (XP, streaks, badges) | At-risk dashboard, teacher queries, ClickHouse Cloud dashboard charts |

ClickHouse cannot join PostgreSQL. The at-risk dashboard merges them in Go:
1. Postgres supplies the class roster (who *should* be reading)
2. ClickHouse aggregates 7-day activity per student
3. Go left-joins the two — students absent from ClickHouse have *never* read,
   which is the highest-risk signal, and an inner join would silently hide them

---

## MCP tools (5)

The Go server exposes five tools over
[Streamable HTTP MCP](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http):

| Tool | Direction | What it does |
|---|---|---|
| `log_reading_session` | write | Records a session; awards XP, updates streak transactionally, unlocks badges |
| `get_student_progress` | read | XP, level, streak, earned badges, gap to next badge, recent reads |
| `get_book_list` | read | Catalogue filtered by genre or title fragment |
| `get_class_dashboard` | read | Students ranked by who most needs attention (cross-DB merge) |
| `recommend_book` | read | Claude Opus 5 suggestion tailored to the student's reading history |

All student-facing tools accept a name string rather than an integer ID —
users in a chat interface don't know database IDs. A shared resolver does
exact → fuzzy → disambiguate, and on failure returns "did you mean?" candidates
so the model can self-correct in one turn.

---

## Gamification

- **XP:** +10 per session + 1 per page read, capped at 60 per session
- **Levels:** Beginner (0–99) → Reader (100–499) → Bookworm (500–999) → Scholar (1000+)
- **Streak:** consecutive calendar days with ≥1 session — owned entirely by
  Postgres, updated in the same transaction as the session, ClickHouse is never
  consulted for it
- **Badges:** First Step, Page Turner (100p), Bookworm (500p), Week Warrior
  (7-day streak), Genre Explorer (3 known genres)

Badge awards run inside the session transaction so a badge can never be granted
for a session that subsequently rolls back.

---

## At-risk rules

A student is flagged if any of:
- `DaysSinceRead == nil` — has never logged a session
- `DaysSinceRead >= 7` — silent for a week or more
- `VelocityPerDay < 10.0` — reading fewer than 10 pages per day (7-day rolling)

Students are ranked: never-read first, then longest-silent, then slowest pace.
A teacher reading the list top-down under time pressure sees the worst cases
first.

---

## Data models

### PostgreSQL

```sql
classes        (id, name)
teachers       (id, name, class_id)
students       (id, name, class_id, xp, streak_days, last_session_date)
books          (id, title, author, genre, age_min, age_max, description)
reading_sessions (id, student_id, book_id, pages_read, minutes_spent, session_date)
badges         (id, name, description, condition_type, condition_value)
student_badges (student_id, badge_id, earned_at)  -- UNIQUE constraint prevents double-award
```

`last_session_date` exists so the streak update can run without reading
ClickHouse: `CASE WHEN last_session_date = CURRENT_DATE THEN streak_days WHEN ... THEN streak_days + 1 ELSE 1 END`.

### ClickHouse

```sql
reading_events (
    event_id UUID,
    student_id    Int32,
    student_name  LowCardinality(String),   -- denormalised — CH can't join PG
    class_id      Int32,
    class_name    LowCardinality(String),
    book_id       Int32,
    book_title    LowCardinality(String),
    genre         String,
    pages_read    Int32,
    minutes_spent Int32,
    session_date  Date,
    created_at    DateTime
)
ENGINE = MergeTree()
ORDER BY (class_id, session_date, student_id)
```

Names are denormalised onto every event so the built-in ClickHouse MCP and
Cloud SQL console dashboards can answer queries in plain English without knowing
Postgres IDs. `LowCardinality` keeps the redundancy cost near zero.

Ordering key `(class_id, session_date, student_id)` is chosen for the
dashboard's dominant query: filter on `class_id` + `session_date >= today()-7`,
then group by `student_id`.

---

## Code structure

```
readquest/
├── cmd/
│   ├── readquest/main.go        # MCP server binary
│   │                            #   flags: -check (connectivity only), -migrate (schema + exit)
│   └── readquest-cli/main.go    # Dev + fallback CLI
│                                #   commands: students, books, progress, recommend,
│                                #             events, dashboard, log, reset
├── internal/
│   ├── app/app.go               # Wires config + both DB clients + all domain stores
│   ├── config/config.go         # Loads .env then environment; validates at boot
│   ├── db/
│   │   ├── wait.go              # WaitReady: retry loop for ClickHouse Cloud cold-start
│   │   ├── postgres/postgres.go # pgx/v5 pool
│   │   └── clickhouse/          # clickhouse-go/v2 client (HTTPS on 8443)
│   ├── domain/
│   │   ├── reading/
│   │   │   ├── reading.go       # LogSession: PG transaction + ClickHouse mirror
│   │   │   ├── resolve.go       # resolveStudent, resolveBook (exact → fuzzy → create)
│   │   │   ├── badges.go        # awardBadges: one INSERT...SELECT inside the session tx
│   │   │   ├── progress.go      # GetProgress: totals, badge standing, recent sessions
│   │   │   └── profile.go       # GetRecommendationProfile: history + inferred age band
│   │   └── dashboard/
│   │       └── dashboard.go     # ClassDashboard: 3-step cross-DB merge + risk ranking
│   ├── ai/
│   │   └── recommend.go         # RecommendBook via Claude Opus 5 (anthropic-sdk-go)
│   └── mcpserver/
│       ├── server.go            # StreamableHTTP server, /healthz, graceful shutdown
│       ├── auth.go              # API key middleware; accepts X-API-Key + Authorization: Bearer
│       │                        # rewrites r.Host to bypass mcp-go DNS-rebinding guard
│       └── tools.go             # 5 tool registrations + handlers
├── migrations/
│   ├── migrations.go            # go:embed + idempotent apply (no schema_migrations table)
│   ├── postgres/001_schema.sql
│   ├── postgres/002_seed.sql    # 12 books, 3 students, 5 badges
│   ├── clickhouse/001_reading_events.sql
│   └── clickhouse/002_denormalize_names.sql  # ADD COLUMN IF NOT EXISTS for existing tables
├── mini-demo.sh    # Full domain smoke test via CLI; doubles as fallback demo
├── scripts/
│   └── mcp-smoke.sh    # 14-assertion MCP protocol test (localhost or tunnel URL)
├── PLAN.md         # Architecture decisions, design rationale, build log
└── README.md       # This file
```

### Key dependencies

| Package | Purpose |
|---|---|
| `github.com/mark3labs/mcp-go` | MCP server (Streamable HTTP transport) |
| `github.com/jackc/pgx/v5` | PostgreSQL client |
| `github.com/ClickHouse/clickhouse-go/v2` | ClickHouse client (native + HTTP) |
| `github.com/anthropics/anthropic-sdk-go` | Claude API (book recommendations) |
| `github.com/joho/godotenv` | `.env` loading |

---

## Setup

### Prerequisites

- Go 1.27+
- A ClickHouse Cloud account with:
  - One ClickHouse service
  - One managed PostgreSQL instance
  - ClickHouse Agents (LibreChat) enabled
- An Anthropic API key (for `recommend_book`)
- ngrok account with a static domain (free tier)

### 1. Clone and configure

```bash
git clone <repo>
cd readquest
cp .env.example .env
```

Fill in `.env`:

```bash
# ClickHouse Cloud → your Postgres instance → Connect
POSTGRES_DSN=postgres://USER:PASSWORD@HOST:5432/postgres?sslmode=require

# ClickHouse Cloud → your CH service → Connect
# HTTPS on 8443 is preferred over native on 9440 — 9440 is often blocked on
# conference/corporate networks. secure=true is required regardless of scheme.
CLICKHOUSE_DSN=https://USER:PASSWORD@HOST:8443/default?secure=true

# Anthropic API key — required only for recommend_book; all other tools work without it
ANTHROPIC_API_KEY=sk-ant-...

# Shared secret for MCP auth. LibreChat will send this as Authorization: Bearer.
# Generate with: openssl rand -hex 24
READQUEST_API_KEY=<random hex>

# MCP server listen address (ngrok tunnels to this port)
LISTEN_ADDR=:8080
```

### 2. Apply schemas and seed data

```bash
go run ./cmd/readquest -migrate
```

This creates both schemas and seeds:
- 1 class, 1 teacher, 3 students
- 12 books across 7 genres (ages 3–14)
- 5 badges

Migrations are idempotent — safe to re-run at any time.

### 3. Start the MCP server

```bash
go run ./cmd/readquest
```

Or build first for faster startup:

```bash
go build -o /tmp/rq-server ./cmd/readquest
/tmp/rq-server
```

Check it's up:
```bash
curl http://localhost:8080/healthz   # → ok
```

### 4. Start the ngrok tunnel

```bash
ngrok http 8080 --url=<your-static-domain>.ngrok-free.dev
```

Verify end-to-end:
```bash
./scripts/mcp-smoke.sh https://<your-static-domain>.ngrok-free.dev
# All 14 checks should pass
```

### 5. Register in ClickHouse Agent

In ClickHouse Cloud → ClickHouse Agent → MCP Servers → **+**:

| Field | Value |
|---|---|
| Name | ReadQuest |
| MCP Server URL | `https://<your-domain>/mcp` |
| Transport | Streamable HTTPS |
| Authentication | API Key |
| Key | value of `READQUEST_API_KEY` |

Start a **new chat** after registering (tool lists bind at conversation start).

---

## Running tests

```bash
# All tests (integration tests require live DB credentials in .env)
go test ./...

# Domain unit tests only (no DB needed)
go test ./internal/domain/dashboard/...

# MCP protocol smoke test
./scripts/mcp-smoke.sh                                          # localhost
./scripts/mcp-smoke.sh https://<your-domain>.ngrok-free.dev    # through tunnel
```

---

## CLI reference

The `readquest-cli` binary drives the same domain logic as the MCP server.
Use it for development, inspection, and as a fallback demo if the chat
interface is unavailable.

```bash
go run ./cmd/readquest-cli <command>

  students                                  list all students (XP, level, streak, badges)
  books [filter]                            list the book catalogue
  progress <student>                        one student's full standing
  recommend <student>                       ask Claude what to read next
  events                                    ClickHouse rollup merged with Postgres roster
  dashboard [class]                         teacher view: at-risk ranking
  log <student> <book> <pages> <minutes>    log a reading session
  reset                                     clear all sessions, badges, XP back to zero
```

Names are matched fuzzily — `log maya matilda 30 20` works.

---

## Demo day checklist

```bash
# 1. Clean slate
./mini-demo.sh

# 2. Start tunnel
ngrok http 8080 --url=<your-domain>.ngrok-free.dev

# 3. Start server (from readquest/ directory)
go build -o /tmp/rq-server ./cmd/readquest && /tmp/rq-server

# 4. Verify everything
./scripts/mcp-smoke.sh https://<your-domain>.ngrok-free.dev

# 5. Open ClickHouse Agent — start a NEW chat
```

**Fallback:** if the chat interface fails at any point during the demo,
`./mini-demo.sh` exercises every feature via the CLI and is fully scripted.

---

## Hackathon track alignment

**Reading Crisis** — tools to identify struggling readers and help students
find engaging books.

| Requirement | How ReadQuest addresses it |
|---|---|
| Identify struggling readers | `get_class_dashboard`: cross-DB merge ranks students by days-silent and reading velocity |
| Help students find books | `get_book_list` + `recommend_book`: Claude Opus 5 personalised suggestions |
| ClickHouse | Event stream, at-risk aggregations, ClickHouse Cloud dashboards, native MCP for teacher ad-hoc queries |
| PostgreSQL | Source of truth for all state; transactional XP + streak + badge awards |
| LibreChat | ClickHouse Agent is the only UI — students and teachers interact entirely through chat |
| AI/LLM | Claude Opus 5 for book recommendations; age band inferred from reading history |

---

## Architecture decisions and design log

See [PLAN.md](PLAN.md) for:
- Why streak lives in Postgres (not ClickHouse)
- Why names are denormalised onto ClickHouse events
- The cross-DB left-join pattern and why an inner join hides the most at-risk students
- The Host-header tunnel fix (mcp-go DNS-rebinding guard)
- Bugs that only surfaced through the live demo harness
