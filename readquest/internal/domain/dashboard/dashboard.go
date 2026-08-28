// Package dashboard answers the teacher's question: who in my class needs
// attention?
//
// This is the one place ReadQuest genuinely needs both databases at once, and
// they cannot be joined. ClickHouse holds the reading events but knows nothing
// about names or class rosters; Postgres holds the roster but is not where
// analytical scans belong. So the join happens here, in Go:
//
//	1. Postgres  — the class roster (who SHOULD be reading)
//	2. ClickHouse — reading activity aggregates (who ACTUALLY read)
//	3. Go        — left-join roster onto activity
//
// Step 3 is a LEFT join for a reason that is easy to get backwards: a student
// with no ClickHouse rows at all is not missing data to be skipped, they are
// the single most at-risk student in the class. An inner join would silently
// hide exactly the children the tool exists to find.
package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"readquest/internal/db/clickhouse"
	"readquest/internal/db/postgres"
	"readquest/internal/domain/reading"
)

const (
	// A student is flagged if they have not read within this window...
	staleAfterDays = 7
	// ...or if they are reading below this pace over that window.
	minPagesPerDay = 10.0
)

type Store struct {
	pg *postgres.DB
	ch *clickhouse.DB
}

func NewStore(pg *postgres.DB, ch *clickhouse.DB) *Store {
	return &Store{pg: pg, ch: ch}
}

type ClassDashboard struct {
	ClassName   string
	GeneratedAt time.Time
	Students    []StudentStanding
	AtRiskCount int
}

// AttentionSummary is the one-line headline for the class.
//
// It lives here rather than in either caller so the CLI and the MCP tool
// cannot drift apart — they previously disagreed on whether one student
// "need" or "needs" attention.
func (d *ClassDashboard) AttentionSummary() string {
	total := len(d.Students)
	switch {
	case total == 0:
		return "no students enrolled"
	case d.AtRiskCount == 0:
		return fmt.Sprintf("all %d students are reading steadily", total)
	case d.AtRiskCount == 1:
		return fmt.Sprintf("1 of %d students needs attention", total)
	default:
		return fmt.Sprintf("%d of %d students need attention", d.AtRiskCount, total)
	}
}

type StudentStanding struct {
	// From Postgres.
	StudentID  int
	Name       string
	XP         int
	Level      string
	StreakDays int

	// From ClickHouse. Zero values are meaningful here: they mean the student
	// produced no events, not that the lookup failed.
	SessionsLast7d int
	PagesLast7d    int
	VelocityPerDay float64

	// LastReadOn is nil when the student has never logged a session at all,
	// which is different from having read a long time ago — a teacher needs
	// to tell those two apart.
	LastReadOn    *time.Time
	DaysSinceRead *int

	AtRisk bool
	Reason string
}

// activity is the ClickHouse half of the merge, keyed by student id.
type activity struct {
	sessions7d int
	pages7d    int
	lastRead   time.Time
}

// ClassDashboard builds the ranked standing for one class.
//
// className may be empty when the deployment has exactly one class, which is
// the demo case — a teacher in chat should not have to name it.
func (s *Store) ClassDashboard(ctx context.Context, className string) (*ClassDashboard, error) {
	classID, resolvedName, err := s.resolveClass(ctx, className)
	if err != nil {
		return nil, err
	}

	roster, err := s.loadRoster(ctx, classID)
	if err != nil {
		return nil, err
	}
	acts, err := s.loadActivity(ctx, classID)
	if err != nil {
		return nil, err
	}

	board := &ClassDashboard{
		ClassName:   resolvedName,
		GeneratedAt: time.Now(),
		Students:    merge(roster, acts),
	}
	for _, st := range board.Students {
		if st.AtRisk {
			board.AtRiskCount++
		}
	}

	slog.Info("class dashboard built",
		"class", resolvedName, "students", len(board.Students), "at_risk", board.AtRiskCount)
	return board, nil
}

func (s *Store) resolveClass(ctx context.Context, name string) (int, string, error) {
	if name == "" {
		var id int
		var resolved string
		err := s.pg.Pool.QueryRow(ctx, `SELECT id, name FROM classes ORDER BY id LIMIT 1`).
			Scan(&id, &resolved)
		if err != nil {
			return 0, "", fmt.Errorf("no classes exist: %w", err)
		}
		return id, resolved, nil
	}

	var id int
	var resolved string
	err := s.pg.Pool.QueryRow(ctx,
		`SELECT id, name FROM classes
		 WHERE lower(name) = lower($1) OR name ILIKE '%' || $1 || '%'
		 ORDER BY id LIMIT 1`, name).Scan(&id, &resolved)
	if err != nil {
		return 0, "", fmt.Errorf("no class matching %q: %w", name, err)
	}
	return id, resolved, nil
}

// loadRoster is step 1: everyone who SHOULD be reading.
func (s *Store) loadRoster(ctx context.Context, classID int) ([]StudentStanding, error) {
	rows, err := s.pg.Pool.Query(ctx, `
		SELECT id, name, xp, streak_days
		FROM students WHERE class_id = $1 ORDER BY name`, classID)
	if err != nil {
		return nil, fmt.Errorf("loading roster: %w", err)
	}
	defer rows.Close()

	var out []StudentStanding
	for rows.Next() {
		var st StudentStanding
		if err := rows.Scan(&st.StudentID, &st.Name, &st.XP, &st.StreakDays); err != nil {
			return nil, err
		}
		st.Level = reading.LevelFor(st.XP)
		out = append(out, st)
	}
	return out, rows.Err()
}

// loadActivity is step 2: who ACTUALLY read, aggregated in ClickHouse.
//
// The window filter is inside the aggregates rather than in a WHERE clause.
// A WHERE would drop students whose last session predates the window, making
// "read nine days ago" indistinguishable from "never read at all" — and those
// call for very different conversations with a child.
func (s *Store) loadActivity(ctx context.Context, classID int) (map[int]activity, error) {
	rows, err := s.ch.Conn.Query(ctx, `
		SELECT student_id,
		       countIf(session_date >= today() - ?)              AS sessions_7d,
		       sumIf(pages_read, session_date >= today() - ?)    AS pages_7d,
		       max(session_date)                                 AS last_read
		FROM reading_events
		WHERE class_id = ?
		GROUP BY student_id`,
		staleAfterDays-1, staleAfterDays-1, int32(classID))
	if err != nil {
		return nil, fmt.Errorf("loading activity from clickhouse: %w", err)
	}
	defer rows.Close()

	out := map[int]activity{}
	for rows.Next() {
		var id int32
		var sessions uint64
		var pages int64 // sum() over Int32 widens to Int64, unlike count()
		var lastRead time.Time
		if err := rows.Scan(&id, &sessions, &pages, &lastRead); err != nil {
			return nil, err
		}
		out[int(id)] = activity{
			sessions7d: int(sessions),
			pages7d:    int(pages),
			lastRead:   lastRead,
		}
	}
	return out, rows.Err()
}

// merge is step 3: the left join, plus ranking.
func merge(roster []StudentStanding, acts map[int]activity) []StudentStanding {
	today := time.Now().Truncate(24 * time.Hour)

	for i := range roster {
		st := &roster[i]

		// Absent from the ClickHouse result means no events at all — keep the
		// zero values and leave LastReadOn nil, which assess() reads as never.
		if a, ok := acts[st.StudentID]; ok {
			st.SessionsLast7d = a.sessions7d
			st.PagesLast7d = a.pages7d
			st.VelocityPerDay = float64(a.pages7d) / float64(staleAfterDays)

			last := a.lastRead
			st.LastReadOn = &last
			days := int(today.Sub(last.Truncate(24*time.Hour)).Hours() / 24)
			if days < 0 {
				days = 0
			}
			st.DaysSinceRead = &days
		}
		st.AtRisk, st.Reason = assess(*st)
	}

	sortByRisk(roster)
	return roster
}

// assess applies the risk rules. It is a pure function of one standing so the
// thresholds can be tested without a database.
func assess(st StudentStanding) (bool, string) {
	if st.DaysSinceRead == nil {
		return true, "has never logged a reading session"
	}
	if days := *st.DaysSinceRead; days >= staleAfterDays {
		return true, fmt.Sprintf("last read %d days ago", days)
	}
	if st.VelocityPerDay < minPagesPerDay {
		return true, fmt.Sprintf("averaging %.1f pages/day over the last %d days",
			st.VelocityPerDay, staleAfterDays)
	}
	return false, fmt.Sprintf("reading %.1f pages/day", st.VelocityPerDay)
}

// sortByRisk puts the students needing attention first: never-read at the top,
// then longest-silent, then slowest. A teacher reads top-down and stops when
// time runs out, so the ordering is the product.
func sortByRisk(students []StudentStanding) {
	sort.SliceStable(students, func(i, j int) bool {
		a, b := students[i], students[j]

		if a.AtRisk != b.AtRisk {
			return a.AtRisk
		}
		// Never-read outranks any finite silence.
		if (a.DaysSinceRead == nil) != (b.DaysSinceRead == nil) {
			return a.DaysSinceRead == nil
		}
		if a.DaysSinceRead != nil && *a.DaysSinceRead != *b.DaysSinceRead {
			return *a.DaysSinceRead > *b.DaysSinceRead
		}
		if a.VelocityPerDay != b.VelocityPerDay {
			return a.VelocityPerDay < b.VelocityPerDay
		}
		return a.Name < b.Name
	})
}
