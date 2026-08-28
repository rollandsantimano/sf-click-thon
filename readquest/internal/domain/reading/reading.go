// Package reading implements logging a reading session — the one write path
// that every other feature depends on.
//
// Postgres is the source of truth and is updated in a single transaction.
// ClickHouse receives a mirrored event afterwards on a best-effort basis: it
// backs analytics only, so a failed event insert is logged and swallowed
// rather than failing a student's session.
package reading

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"readquest/internal/db/clickhouse"
	"readquest/internal/db/postgres"
)

// Bounds on a single session. Input arrives from a chat model relaying what a
// child typed, so it can be arbitrary — these reject nonsense before it
// reaches the XP and streak logic.
const (
	maxPagesPerSession   = 2000
	maxMinutesPerSession = 1440 // a full day
)

// XP awarded per session: a flat participation grant plus one point per page,
// capped so a single implausible entry cannot dominate a class leaderboard.
const (
	xpPerSession = 10
	xpPerPage    = 1
	xpSessionCap = 60
)

type Store struct {
	pg *postgres.DB
	ch *clickhouse.DB
}

func NewStore(pg *postgres.DB, ch *clickhouse.DB) *Store {
	return &Store{pg: pg, ch: ch}
}

type Student struct {
	ID      int
	Name    string
	ClassID int
	// ClassName is carried so it can be denormalised onto ClickHouse events,
	// which have no way to resolve a class id afterwards.
	ClassName       string
	XP              int
	StreakDays      int
	LastSessionDate *time.Time
}

type Book struct {
	ID     int
	Title  string
	Author string
	Genre  string
	AgeMin *int
	AgeMax *int
}

// SessionResult is what the chat layer narrates back to the student.
type SessionResult struct {
	Student      Student
	Book         Book
	PagesRead    int
	MinutesSpent int
	SessionDate  time.Time

	XPAwarded  int
	TotalXP    int
	Level      string
	StreakDays int

	// NewBadges holds only badges earned by THIS session, not every badge the
	// student holds — the chat layer celebrates these, and repeating old ones
	// every time would make the new one impossible to spot.
	NewBadges []Badge

	// BookWasCreated reports that the title was not in the catalogue and a
	// placeholder row was added. The chat layer can mention this so a
	// misspelling is visible rather than silently becoming a new book.
	BookWasCreated bool
}

// queryer is satisfied by both *pgxpool.Pool and pgx.Tx, so the resolvers can
// run either standalone or inside LogSession's transaction.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LogSession records one reading session and returns the student's updated
// standing.
//
// Everything that must agree — the session row, the XP total, the streak — is
// written in one transaction. The ClickHouse mirror happens after commit,
// because a session that Postgres accepted must not be rolled back by an
// analytics failure.
func (s *Store) LogSession(ctx context.Context, studentName, bookTitle string, pages, minutes int) (*SessionResult, error) {
	if err := validateSession(pages, minutes); err != nil {
		return nil, err
	}

	tx, err := s.pg.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe
	// unconditionally and covers every early return below.
	defer func() { _ = tx.Rollback(ctx) }()

	student, err := resolveStudent(ctx, tx, studentName)
	if err != nil {
		return nil, err
	}

	book, created, err := resolveBook(ctx, tx, bookTitle)
	if err != nil {
		return nil, err
	}

	var sessionDate time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO reading_sessions (student_id, book_id, pages_read, minutes_spent)
		VALUES ($1, $2, $3, $4)
		RETURNING session_date`,
		student.ID, book.ID, pages, minutes,
	).Scan(&sessionDate)
	if err != nil {
		return nil, fmt.Errorf("recording session: %w", err)
	}

	xpAwarded := awardXP(pages)

	// XP and streak advance in one statement so they cannot disagree.
	//
	// The streak arithmetic is deliberately SQL rather than Go: it compares
	// against CURRENT_DATE, and computing "today" in Go instead would compare
	// the client's timezone against the server's stored dates.
	var totalXP, streak int
	err = tx.QueryRow(ctx, `
		UPDATE students
		SET xp = xp + $2,
		    streak_days = CASE
		        WHEN last_session_date = CURRENT_DATE     THEN streak_days
		        WHEN last_session_date = CURRENT_DATE - 1 THEN streak_days + 1
		        ELSE 1
		    END,
		    last_session_date = CURRENT_DATE
		WHERE id = $1
		RETURNING xp, streak_days`,
		student.ID, xpAwarded,
	).Scan(&totalXP, &streak)
	if err != nil {
		return nil, fmt.Errorf("updating student standing: %w", err)
	}

	// Badges are evaluated before the commit, against the session and streak
	// this transaction just wrote, so an award can never outlive a rollback.
	newBadges, err := awardBadges(ctx, tx, student.ID, streak)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing session: %w", err)
	}

	student.XP = totalXP
	student.StreakDays = streak

	result := &SessionResult{
		Student:        *student,
		Book:           *book,
		PagesRead:      pages,
		MinutesSpent:   minutes,
		SessionDate:    sessionDate,
		XPAwarded:      xpAwarded,
		TotalXP:        totalXP,
		Level:          LevelFor(totalXP),
		StreakDays:     streak,
		NewBadges:      newBadges,
		BookWasCreated: created,
	}

	slog.Info("reading session logged",
		"student", student.Name, "book", book.Title,
		"pages", pages, "minutes", minutes,
		"xp_awarded", xpAwarded, "total_xp", totalXP, "streak", streak,
		"new_badges", len(newBadges))

	s.mirrorToClickHouse(ctx, result)
	return result, nil
}

// mirrorToClickHouse writes the analytics event. It never returns an error:
// ClickHouse is not the source of truth, and the session is already committed
// in Postgres by the time this runs.
//
// The cost of swallowing a failure is a gap in the at-risk dashboard, which is
// strictly better than telling a child their reading did not count.
func (s *Store) mirrorToClickHouse(ctx context.Context, r *SessionResult) {
	// Names travel with their ids so ClickHouse can answer on its own — in the
	// SQL console, in a dashboard, and to the built-in ClickHouse MCP, none of
	// which can reach Postgres to resolve an id.
	err := s.ch.Conn.Exec(ctx, `
		INSERT INTO reading_events
			(student_id, student_name, class_id, class_name, book_id, book_title,
			 genre, pages_read, minutes_spent, session_date)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		int32(r.Student.ID), r.Student.Name,
		int32(r.Student.ClassID), r.Student.ClassName,
		int32(r.Book.ID), r.Book.Title,
		r.Book.Genre, int32(r.PagesRead), int32(r.MinutesSpent), r.SessionDate,
	)
	if err != nil {
		slog.Error("clickhouse event mirror failed — session is committed in postgres, analytics will under-count",
			"student_id", r.Student.ID, "error", err)
		return
	}
	slog.Debug("clickhouse event mirrored", "student_id", r.Student.ID)
}

func validateSession(pages, minutes int) error {
	switch {
	case pages <= 0:
		return fmt.Errorf("%w: pages read must be greater than zero", ErrInvalidInput)
	case minutes <= 0:
		return fmt.Errorf("%w: minutes spent must be greater than zero", ErrInvalidInput)
	case pages > maxPagesPerSession:
		return fmt.Errorf("%w: %d pages in one session is not plausible (max %d)",
			ErrInvalidInput, pages, maxPagesPerSession)
	case minutes > maxMinutesPerSession:
		return fmt.Errorf("%w: %d minutes exceeds a single day (max %d)",
			ErrInvalidInput, minutes, maxMinutesPerSession)
	}
	return nil
}

func awardXP(pages int) int {
	xp := xpPerSession + pages*xpPerPage
	if xp > xpSessionCap {
		return xpSessionCap
	}
	return xp
}

// LevelFor names the band a total XP score falls into. Bands are wide and
// early ones are short, so a child sees a level change within their first few
// sessions rather than after weeks.
func LevelFor(totalXP int) string {
	switch {
	case totalXP >= 1000:
		return "Scholar"
	case totalXP >= 500:
		return "Bookworm"
	case totalXP >= 100:
		return "Reader"
	default:
		return "Beginner"
	}
}

var ErrInvalidInput = errors.New("invalid input")
