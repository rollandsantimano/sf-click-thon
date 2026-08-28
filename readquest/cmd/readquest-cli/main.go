// Command readquest-cli drives the ReadQuest domain from a terminal.
//
// Two purposes. During the build it is how each phase gets exercised before
// the MCP server exists. On demo day it is the fallback: if ngrok or the
// hosted chat UI misbehaves, every feature is still demonstrable here, because
// this drives the same stores the server does.
//
// Usage:
//
//	readquest-cli students
//	readquest-cli books [filter]
//	readquest-cli log <student> <book> <pages> <minutes>
//	readquest-cli reset
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"readquest/internal/app"
	"readquest/internal/domain/reading"
)

func main() {
	// The CLI's output is the point, so keep library logging out of it unless
	// something is actually wrong.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	if err := dispatch(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(cmd string, args []string) error {
	ctx := context.Background()

	a, err := app.Open(ctx, false)
	if err != nil {
		return err
	}
	defer a.Close()

	switch cmd {
	case "students":
		return cmdStudents(ctx, a)
	case "books":
		var filter string
		if len(args) > 0 {
			filter = args[0]
		}
		return cmdBooks(ctx, a, filter)
	case "events":
		return cmdEvents(ctx, a)
	case "progress":
		return cmdProgress(ctx, a, args)
	case "suspicious":
		var class string
		if len(args) > 0 {
			class = args[0]
		}
		return cmdSuspicious(ctx, a, class)
	case "recommend":
		return cmdRecommend(ctx, a, args)
	case "dashboard":
		var class string
		if len(args) > 0 {
			class = args[0]
		}
		return cmdDashboard(ctx, a, class)
	case "log":
		return cmdLog(ctx, a, args)
	case "reset":
		return cmdReset(ctx, a)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `readquest-cli — drive the ReadQuest domain from a terminal

  students                                  list students with XP, level, streak
  books [filter]                            list the catalogue
  progress <student>                        one student's badges, totals and recent reads
  suspicious [class]                        sessions that may indicate reward-hacking
  recommend <student>                       ask Claude what the student should read next
  dashboard [class]                         teacher view: students ranked by who needs attention
  events                                    per-student rollup from ClickHouse analytics
  log <student> <book> <pages> <minutes>    log a reading session
  reset                                     clear all activity back to a fresh demo state

Names are matched loosely, so "log maya matilda 20 25" works.
`)
}

func cmdStudents(ctx context.Context, a *app.App) error {
	rows, err := a.PG.Pool.Query(ctx, `
		SELECT s.name, c.name, s.xp, s.streak_days, s.last_session_date,
		       (SELECT count(*) FROM reading_sessions rs WHERE rs.student_id = s.id),
		       (SELECT count(*) FROM student_badges sb WHERE sb.student_id = s.id)
		FROM students s JOIN classes c ON c.id = s.class_id
		ORDER BY s.xp DESC, s.name`)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := table("STUDENT\tCLASS\tXP\tLEVEL\tSTREAK\tSESSIONS\tBADGES\tLAST READ")
	for rows.Next() {
		var name, class string
		var xp, streak, sessions, badges int
		// DATE arrives as time.Time over the binary protocol, not a string —
		// scanning into *string only appears to work while every row is NULL.
		var last *time.Time
		if err := rows.Scan(&name, &class, &xp, &streak, &last, &sessions, &badges); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\n",
			name, class, xp, reading.LevelFor(xp), streak, sessions, badges, dateOr(last, "never"))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func cmdBooks(ctx context.Context, a *app.App, filter string) error {
	rows, err := a.PG.Pool.Query(ctx, `
		SELECT b.title, b.author, b.genre, b.age_min, b.age_max,
		       (SELECT count(*) FROM reading_sessions rs WHERE rs.book_id = b.id)
		FROM books b
		WHERE $1 = '' OR b.title ILIKE '%' || $1 || '%' OR b.genre ILIKE '%' || $1 || '%'
		ORDER BY b.genre, b.title`, filter)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := table("TITLE\tAUTHOR\tGENRE\tAGES\tTIMES READ")
	for rows.Next() {
		var title, author, genre string
		var ageMin, ageMax *int
		var timesRead int
		if err := rows.Scan(&title, &author, &genre, &ageMin, &ageMax, &timesRead); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", title, author, genre, ageRange(ageMin, ageMax), timesRead)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return w.Flush()
}

func cmdSuspicious(ctx context.Context, a *app.App, class string) error {
	sessions, err := a.Dashboard.SuspiciousSessions(ctx, class)
	if err != nil {
		return err
	}

	if len(sessions) == 0 {
		fmt.Println("\nNo suspicious sessions detected in the last 30 days.")
		fmt.Println()
		return nil
	}

	fmt.Printf("\n%d suspicious session(s) detected (last 30 days)\n", len(sessions))
	w := table("\n  STUDENT\tDATE\tPAGES\tMIN\tRATE\tFLAG")
	for _, ss := range sessions {
		rate := "-"
		if ss.PagesPerMinute > 0 {
			rate = fmt.Sprintf("%.1f/min", ss.PagesPerMinute)
		}
		fmt.Fprintf(w, "  %s\t%s\t%d\t%d\t%s\t%s\n",
			ss.StudentName, ss.SessionDate.Format("2006-01-02"),
			ss.PagesRead, ss.MinutesSpent, rate, ss.Reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\n  Flagged for teacher review — not automatically penalised.")
	fmt.Println()
	return nil
}

func cmdRecommend(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: recommend <student>")
	}

	profile, err := a.Reading.GetRecommendationProfile(ctx, args[0])
	if err != nil {
		return err
	}

	fmt.Printf("\nAsking Claude for a recommendation for %s (ages %d-%d, %d books read)...\n",
		profile.StudentName, profile.AgeMin, profile.AgeMax, len(profile.AlreadyRead))

	suggestion, err := a.Recommender.RecommendBook(ctx, profile)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s\n\n", suggestion)
	return nil
}

func cmdDashboard(ctx context.Context, a *app.App, class string) error {
	board, err := a.Dashboard.ClassDashboard(ctx, class)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s — %s\n", board.ClassName, board.AttentionSummary())

	w := table("\n   STUDENT\tLEVEL\tXP\tSTREAK\t7d PAGES\tPACE\tLAST READ\tSTATUS")
	for _, st := range board.Students {
		// The marker matters more than the columns: a teacher scanning under
		// time pressure reads the left edge first.
		marker := "  "
		if st.AtRisk {
			marker = "->"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%d\t%d\t%d\t%.1f/day\t%s\t%s\n",
			marker, st.Name, st.Level, st.XP, st.StreakDays,
			st.PagesLast7d, st.VelocityPerDay, lastRead(st.DaysSinceRead), st.Reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println()
	return nil
}

func lastRead(daysSince *int) string {
	switch {
	case daysSince == nil:
		return "never"
	case *daysSince == 0:
		return "today"
	case *daysSince == 1:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", *daysSince)
	}
}

// cmdEvents rolls up the ClickHouse event stream per student.
//
// Since names are denormalised onto the events, this query stands entirely on
// its own — no Postgres lookup is needed to label a row. Postgres is still
// consulted for one thing only: students who produced NO events, who by
// definition cannot appear in a ClickHouse result and are exactly the ones a
// teacher most needs to see.
func cmdEvents(ctx context.Context, a *app.App) error {
	chRows, err := a.CH.Conn.Query(ctx, `
		SELECT student_name,
		       count()            AS sessions,
		       sum(pages_read)    AS pages,
		       sum(minutes_spent) AS minutes,
		       -- Must match the Genre Explorer rule in badges.go, which
		       -- excludes the 'Unknown' placeholder. Counting it here would
		       -- report 3 genres beside a badge that says 2/3.
		       uniqExactIf(genre, genre != 'Unknown') AS genres,
		       max(session_date)  AS last_read
		FROM reading_events
		GROUP BY student_name
		ORDER BY pages DESC`)
	if err != nil {
		return fmt.Errorf("querying clickhouse: %w", err)
	}
	defer chRows.Close()

	w := table("STUDENT\tSESSIONS\tPAGES\tMINUTES\tGENRES\tLAST READ")
	seen := map[string]bool{}
	for chRows.Next() {
		var name string
		// count() and uniqExact() are UInt64, but sum() over an Int32 column
		// widens to Int64 — the driver rejects a mismatched scan target, so
		// these cannot all share one type.
		var sessions, genres uint64
		var pages, minutes int64
		var lastRead time.Time
		if err := chRows.Scan(&name, &sessions, &pages, &minutes, &genres, &lastRead); err != nil {
			return err
		}
		seen[name] = true

		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%s\n",
			name, sessions, pages, minutes, genres, lastRead.Format("2006-01-02"))
	}
	if err := chRows.Err(); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(seen) == 0 {
		fmt.Println("\n(no analytics events yet — log a session first)")
	}

	return listStudentsWithNoEvents(ctx, a, seen)
}

// listStudentsWithNoEvents names the roster entries ClickHouse never saw.
//
// This is the one thing denormalisation cannot fix: absence of data is not
// stored anywhere, so it can only be found by starting from the Postgres
// roster. It is the same reason the Phase 5 dashboard needs a left join.
func listStudentsWithNoEvents(ctx context.Context, a *app.App, seen map[string]bool) error {
	rows, err := a.PG.Pool.Query(ctx, `SELECT name FROM students ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		fmt.Printf("\n  no reading activity at all: %s\n", strings.Join(missing, ", "))
	}
	fmt.Println()
	return nil
}

func cmdLog(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: log <student> <book> <pages> <minutes>")
	}
	pages, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("pages must be a number, got %q", args[2])
	}
	minutes, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("minutes must be a number, got %q", args[3])
	}

	res, err := a.Reading.LogSession(ctx, args[0], args[1], pages, minutes)
	if err != nil {
		return err
	}

	fmt.Println("\nSession logged")
	w := table("")
	fmt.Fprintf(w, "  Student\t%s\n", res.Student.Name)
	fmt.Fprintf(w, "  Book\t%s (%s)\n", res.Book.Title, res.Book.Genre)
	fmt.Fprintf(w, "  Read\t%d pages in %d min\n", res.PagesRead, res.MinutesSpent)
	fmt.Fprintf(w, "  XP\t+%d  (total %d)\n", res.XPAwarded, res.TotalXP)
	fmt.Fprintf(w, "  Level\t%s\n", res.Level)
	fmt.Fprintf(w, "  Streak\t%s\n", plural(res.StreakDays, "day", "days"))
	if err := w.Flush(); err != nil {
		return err
	}

	for _, b := range res.NewBadges {
		fmt.Printf("\n  BADGE UNLOCKED  %s — %s\n", b.Name, b.Description)
	}

	if res.BookWasCreated {
		fmt.Printf("\n  note: %q was not in the catalogue, so it was added with genre 'Unknown'.\n"+
			"        If that was a typo, run `reset` to clear it.\n", res.Book.Title)
	}
	fmt.Println()
	return nil
}

func cmdProgress(ctx context.Context, a *app.App, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: progress <student>")
	}

	p, err := a.Reading.GetProgress(ctx, args[0])
	if err != nil {
		return err
	}

	fmt.Printf("\n%s — %s (%d XP)\n", p.Student.Name, p.Level, p.Student.XP)

	w := table("")
	fmt.Fprintf(w, "  Sessions\t%d\n", p.SessionCount)
	fmt.Fprintf(w, "  Pages read\t%d\n", p.TotalPages)
	fmt.Fprintf(w, "  Time reading\t%d min\n", p.TotalMinutes)
	fmt.Fprintf(w, "  Genres explored\t%d\n", p.DistinctGenres)
	fmt.Fprintf(w, "  Streak\t%s\n", plural(p.Student.StreakDays, "day", "days"))
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n  Badges earned (%d)\n", len(p.Earned))
	if len(p.Earned) == 0 {
		fmt.Println("    none yet")
	}
	for _, b := range p.Earned {
		fmt.Printf("    [x] %-16s %s\n", b.Name, b.Description)
	}

	if len(p.Locked) > 0 {
		fmt.Printf("\n  Still to earn (%d)\n", len(p.Locked))
		for _, b := range p.Locked {
			fmt.Printf("    [ ] %-16s %d/%d\n", b.Name, b.Have, b.Need)
		}
	}

	if len(p.Recent) > 0 {
		rw := table("\n  RECENT\tGENRE\tPAGES\tMIN\tDATE")
		for _, r := range p.Recent {
			fmt.Fprintf(rw, "  %s\t%s\t%d\t%d\t%s\n",
				r.Title, r.Genre, r.PagesRead, r.MinutesSpent, r.SessionDate.Format("2006-01-02"))
		}
		if err := rw.Flush(); err != nil {
			return err
		}
	}

	fmt.Println()
	return nil
}

// cmdReset returns the demo to a clean slate.
//
// The seed migrations are idempotent, which means re-running them does NOT
// undo accumulated XP — so without this there is no way back to a fresh state
// short of dropping tables. Order follows the foreign keys.
func cmdReset(ctx context.Context, a *app.App) error {
	tx, err := a.PG.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, stmt := range []string{
		`DELETE FROM student_badges`,
		`DELETE FROM reading_sessions`,
		// Only placeholder rows: the seeded catalogue must survive a reset.
		`DELETE FROM books WHERE genre = 'Unknown'`,
		`UPDATE students SET xp = 0, streak_days = 0, last_session_date = NULL`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("resetting (%s): %w", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// ClickHouse has no transaction to join; it is analytics only, so a
	// failure here leaves stale events rather than corrupt state.
	if err := a.CH.Conn.Exec(ctx, `TRUNCATE TABLE reading_events`); err != nil {
		return fmt.Errorf("truncating clickhouse events: %w", err)
	}

	fmt.Println("\nReset complete — sessions, badges, XP, streaks and analytics events cleared.")
	fmt.Println("Seeded students, classes, badges and the 12-book catalogue are intact.")
	fmt.Println()
	return nil
}

func table(header string) *tabwriter.Writer {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if header != "" {
		fmt.Fprintf(w, "\n%s\n", header)
	}
	return w
}

func dateOr(t *time.Time, fallback string) string {
	if t == nil {
		return fallback
	}
	return t.Format("2006-01-02")
}

func ageRange(lo, hi *int) string {
	if lo == nil || hi == nil {
		return "-"
	}
	return fmt.Sprintf("%d-%d", *lo, *hi)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
