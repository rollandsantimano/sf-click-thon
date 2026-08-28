package reading

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"readquest/internal/db/clickhouse"
	"readquest/internal/db/postgres"
)

// These are integration tests against the live ClickHouse Cloud databases.
//
// They exist because the parts of LogSession most likely to be wrong are not
// Go at all — they are the streak CASE expression, an ON CONFLICT against an
// expression index, and the ClickHouse driver's type mapping for a Date
// column. None of those can be exercised without a real server, and all three
// are load-bearing for every later phase.
//
// They skip rather than fail when credentials are absent, so `go test ./...`
// stays useful offline.

func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()

	// Tests run in the package directory, so .env at the module root is four
	// levels up and godotenv's default lookup would miss it.
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".env"))
	if err == nil {
		_ = godotenv.Load(root)
	}

	pgDSN, chDSN := os.Getenv("POSTGRES_DSN"), os.Getenv("CLICKHOUSE_DSN")
	if pgDSN == "" || chDSN == "" {
		t.Skip("POSTGRES_DSN / CLICKHOUSE_DSN not set — skipping integration test")
	}

	ctx := context.Background()
	pg, err := postgres.Connect(ctx, pgDSN)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pg.Close)

	ch, err := clickhouse.Connect(ctx, chDSN)
	if err != nil {
		t.Fatalf("connecting to clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	return NewStore(pg, ch), ctx
}

func TestLogSession_AwardsXPAndMirrorsToClickHouse(t *testing.T) {
	s, ctx := testStore(t)

	const student = "Maya Chen"
	before := readXP(t, ctx, s, student)
	eventsBefore := countEvents(t, ctx, s, student)

	res, err := s.LogSession(ctx, student, "Matilda", 24, 30)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}

	if want := 10 + 24; res.XPAwarded != want {
		t.Errorf("XPAwarded = %d, want %d", res.XPAwarded, want)
	}
	if got, want := res.TotalXP, before+res.XPAwarded; got != want {
		t.Errorf("TotalXP = %d, want %d", got, want)
	}
	if res.BookWasCreated {
		t.Error("Matilda is seeded; it should not have been auto-created")
	}
	if res.Book.Genre != "Fantasy" {
		t.Errorf("Book.Genre = %q, want %q", res.Book.Genre, "Fantasy")
	}
	if res.StreakDays < 1 {
		t.Errorf("StreakDays = %d, want at least 1", res.StreakDays)
	}

	// The cross-DB mirror is the part with no transaction protecting it.
	if got := countEvents(t, ctx, s, student); got != eventsBefore+1 {
		t.Errorf("clickhouse events = %d, want %d — the mirror did not land", got, eventsBefore+1)
	}
}

// TestLogSession_SameDayDoesNotAdvanceStreak guards the exact bug the plan
// review caught: a streak that double-counts two sessions on one day.
func TestLogSession_SameDayDoesNotAdvanceStreak(t *testing.T) {
	s, ctx := testStore(t)

	first, err := s.LogSession(ctx, "Diego Ramirez", "Holes", 15, 20)
	if err != nil {
		t.Fatalf("first LogSession: %v", err)
	}
	second, err := s.LogSession(ctx, "Diego Ramirez", "Hatchet", 18, 25)
	if err != nil {
		t.Fatalf("second LogSession: %v", err)
	}

	if first.StreakDays != second.StreakDays {
		t.Errorf("streak advanced within one day: %d then %d", first.StreakDays, second.StreakDays)
	}
	// XP must still accrue for both — only the streak is once-per-day.
	if second.TotalXP <= first.TotalXP {
		t.Errorf("second session awarded no XP: %d then %d", first.TotalXP, second.TotalXP)
	}
}

func TestLogSession_XPIsCapped(t *testing.T) {
	s, ctx := testStore(t)

	res, err := s.LogSession(ctx, "Amara Okafor", "Wonder", 500, 120)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}
	if res.XPAwarded != xpSessionCap {
		t.Errorf("XPAwarded = %d, want cap %d", res.XPAwarded, xpSessionCap)
	}
}

// TestResolveBook_AutoCreates exercises ON CONFLICT against the expression
// index on lower(title), which is the fiddliest SQL in the package.
func TestResolveBook_AutoCreates(t *testing.T) {
	s, ctx := testStore(t)

	title := "The Test Book " + time.Now().Format("20060102150405.000")
	res, err := s.LogSession(ctx, "Maya Chen", title, 10, 10)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}

	if !res.BookWasCreated {
		t.Error("BookWasCreated = false, want true for a novel title")
	}
	if res.Book.Genre != "Unknown" {
		t.Errorf("Book.Genre = %q, want %q so Genre Explorer excludes it", res.Book.Genre, "Unknown")
	}

	// Logging the same title again must reuse the row, not create a second.
	again, err := s.LogSession(ctx, "Maya Chen", title, 5, 5)
	if err != nil {
		t.Fatalf("second LogSession: %v", err)
	}
	if again.BookWasCreated {
		t.Error("second log re-created the book instead of matching it")
	}
	if again.Book.ID != res.Book.ID {
		t.Errorf("book id changed: %d then %d", res.Book.ID, again.Book.ID)
	}
}

func TestResolveStudent_UnknownNameListsCandidates(t *testing.T) {
	s, ctx := testStore(t)

	_, err := s.LogSession(ctx, "Nobody McNotreal", "Matilda", 10, 10)
	if !errors.Is(err, ErrStudentNotFound) {
		t.Fatalf("error = %v, want ErrStudentNotFound", err)
	}

	var resErr *ResolutionError
	if !errors.As(err, &resErr) {
		t.Fatalf("error is not a *ResolutionError: %v", err)
	}
	if len(resErr.Candidates) == 0 {
		t.Error("no candidates offered — the chat layer cannot suggest a correction")
	}
}

func TestLogSession_RejectsImplausibleInput(t *testing.T) {
	s, ctx := testStore(t)

	for _, tc := range []struct {
		name           string
		pages, minutes int
	}{
		{"zero pages", 0, 10},
		{"zero minutes", 10, 0},
		{"negative pages", -5, 10},
		{"absurd pages", maxPagesPerSession + 1, 10},
		{"absurd minutes", 10, maxMinutesPerSession + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.LogSession(ctx, "Maya Chen", "Matilda", tc.pages, tc.minutes); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestBadges_AwardedOnceAndGenreExplorerIgnoresUnknown covers the two ways
// badge awarding can go wrong: granting the same badge twice, and letting an
// auto-created 'Unknown' book count toward Genre Explorer.
func TestBadges_AwardedOnceAndGenreExplorerIgnoresUnknown(t *testing.T) {
	s, ctx := testStore(t)
	resetStudent(t, ctx, s, "Amara Okafor")

	// First Step must arrive with the very first session.
	first, err := s.LogSession(ctx, "Amara Okafor", "Matilda", 10, 15)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}
	if !hasBadge(first.NewBadges, "First Step") {
		t.Errorf("First Step not awarded on first session, got %v", names(first.NewBadges))
	}

	// ...and must not arrive again on the second.
	second, err := s.LogSession(ctx, "Amara Okafor", "Holes", 10, 15)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}
	if hasBadge(second.NewBadges, "First Step") {
		t.Error("First Step awarded twice")
	}

	// Two known genres so far. An auto-created book adds a third *session*
	// but its genre is 'Unknown', so Genre Explorer must stay locked.
	unknownBook := "Nonexistent Title " + time.Now().Format("150405.000")
	third, err := s.LogSession(ctx, "Amara Okafor", unknownBook, 10, 15)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}
	if hasBadge(third.NewBadges, "Genre Explorer") {
		t.Error("Genre Explorer counted an 'Unknown' genre — a typo can now earn it")
	}

	// A third genuine genre must unlock it.
	fourth, err := s.LogSession(ctx, "Amara Okafor", "The Wild Robot", 10, 15)
	if err != nil {
		t.Fatalf("LogSession: %v", err)
	}
	if !hasBadge(fourth.NewBadges, "Genre Explorer") {
		t.Errorf("Genre Explorer not awarded at 3 known genres, got %v", names(fourth.NewBadges))
	}
}

func TestGetProgress_ReportsGapToLockedBadges(t *testing.T) {
	s, ctx := testStore(t)
	resetStudent(t, ctx, s, "Diego Ramirez")

	if _, err := s.LogSession(ctx, "Diego Ramirez", "Hatchet", 30, 20); err != nil {
		t.Fatalf("LogSession: %v", err)
	}

	p, err := s.GetProgress(ctx, "Diego Ramirez")
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}

	if p.TotalPages != 30 {
		t.Errorf("TotalPages = %d, want 30", p.TotalPages)
	}
	if len(p.Earned) == 0 {
		t.Error("no badges earned after a session — First Step should be present")
	}

	var pageTurner *BadgeProgress
	for i := range p.Locked {
		if p.Locked[i].Name == "Page Turner" {
			pageTurner = &p.Locked[i]
		}
	}
	if pageTurner == nil {
		t.Fatal("Page Turner should still be locked at 30 pages")
	}
	if pageTurner.Have != 30 || pageTurner.Need != 100 {
		t.Errorf("Page Turner gap = %d/%d, want 30/100", pageTurner.Have, pageTurner.Need)
	}
}

// resetStudent clears one student's activity so badge tests start from a known
// state regardless of what earlier runs or the demo script left behind.
func resetStudent(t *testing.T, ctx context.Context, s *Store, name string) {
	t.Helper()
	var id int
	if err := s.pg.Pool.QueryRow(ctx, `SELECT id FROM students WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("finding %s: %v", name, err)
	}
	for _, q := range []string{
		`DELETE FROM student_badges WHERE student_id = $1`,
		`DELETE FROM reading_sessions WHERE student_id = $1`,
		`UPDATE students SET xp = 0, streak_days = 0, last_session_date = NULL WHERE id = $1`,
	} {
		if _, err := s.pg.Pool.Exec(ctx, q, id); err != nil {
			t.Fatalf("resetting %s: %v", name, err)
		}
	}
}

func hasBadge(badges []Badge, name string) bool {
	for _, b := range badges {
		if b.Name == name {
			return true
		}
	}
	return false
}

func names(badges []Badge) []string {
	out := make([]string, len(badges))
	for i, b := range badges {
		out[i] = b.Name
	}
	return out
}

func readXP(t *testing.T, ctx context.Context, s *Store, name string) int {
	t.Helper()
	var xp int
	err := s.pg.Pool.QueryRow(ctx, `SELECT xp FROM students WHERE name = $1`, name).Scan(&xp)
	if err != nil {
		t.Fatalf("reading xp for %s: %v", name, err)
	}
	return xp
}

func countEvents(t *testing.T, ctx context.Context, s *Store, name string) uint64 {
	t.Helper()
	var studentID int
	if err := s.pg.Pool.QueryRow(ctx, `SELECT id FROM students WHERE name = $1`, name).Scan(&studentID); err != nil {
		t.Fatalf("reading id for %s: %v", name, err)
	}

	var n uint64
	err := s.ch.Conn.QueryRow(ctx,
		`SELECT count() FROM reading_events WHERE student_id = ?`, int32(studentID)).Scan(&n)
	if err != nil {
		t.Fatalf("counting clickhouse events: %v", err)
	}
	return n
}
