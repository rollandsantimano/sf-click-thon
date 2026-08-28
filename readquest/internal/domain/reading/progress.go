package reading

import (
	"context"
	"fmt"
	"time"
)

// Progress is a student's full standing — what the chat layer reads back when
// a child asks how they are doing.
type Progress struct {
	Student Student
	Level   string

	SessionCount   int
	TotalPages     int
	TotalMinutes   int
	DistinctGenres int

	Earned []Badge
	// Locked carries the remaining distance to each unearned badge, so the
	// answer can end on "38 more pages" rather than a list of things missing.
	Locked []BadgeProgress

	Recent []RecentSession
}

type RecentSession struct {
	Title        string
	Genre        string
	PagesRead    int
	MinutesSpent int
	SessionDate  time.Time
}

const recentSessionLimit = 5

// GetProgress reads a student's standing by name.
func (s *Store) GetProgress(ctx context.Context, studentName string) (*Progress, error) {
	student, err := resolveStudent(ctx, s.pg.Pool, studentName)
	if err != nil {
		return nil, err
	}

	p := &Progress{
		Student: *student,
		Level:   LevelFor(student.XP),
	}

	// Totals come from Postgres rather than ClickHouse deliberately: this is a
	// per-student point lookup on the source of truth, not an analytical scan.
	// ClickHouse earns its place on the class-wide dashboard, not here.
	err = s.pg.Pool.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(sum(rs.pages_read), 0),
		       coalesce(sum(rs.minutes_spent), 0),
		       count(DISTINCT b.genre) FILTER (WHERE b.genre <> 'Unknown')
		FROM reading_sessions rs
		JOIN books b ON b.id = rs.book_id
		WHERE rs.student_id = $1`,
		student.ID,
	).Scan(&p.SessionCount, &p.TotalPages, &p.TotalMinutes, &p.DistinctGenres)
	if err != nil {
		return nil, fmt.Errorf("reading totals: %w", err)
	}

	if p.Earned, p.Locked, err = s.badgeStanding(ctx, student.ID, p); err != nil {
		return nil, err
	}
	if p.Recent, err = s.recentSessions(ctx, student.ID); err != nil {
		return nil, err
	}
	return p, nil
}

// badgeStanding splits every badge into earned and locked, computing the gap
// for the locked ones from the totals already gathered.
func (s *Store) badgeStanding(ctx context.Context, studentID int, p *Progress) ([]Badge, []BadgeProgress, error) {
	rows, err := s.pg.Pool.Query(ctx, `
		SELECT b.name, b.description, b.condition_type, b.condition_value,
		       (sb.student_id IS NOT NULL) AS earned
		FROM badges b
		LEFT JOIN student_badges sb ON sb.badge_id = b.id AND sb.student_id = $1
		ORDER BY b.id`, studentID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading badges: %w", err)
	}
	defer rows.Close()

	var earned []Badge
	var locked []BadgeProgress
	for rows.Next() {
		var b Badge
		var condType string
		var condValue int
		var isEarned bool
		if err := rows.Scan(&b.Name, &b.Description, &condType, &condValue, &isEarned); err != nil {
			return nil, nil, err
		}

		if isEarned {
			earned = append(earned, b)
			continue
		}
		locked = append(locked, BadgeProgress{
			Badge: b,
			Have:  currentValueFor(condType, p),
			Need:  condValue,
		})
	}
	return earned, locked, rows.Err()
}

// currentValueFor maps a badge's condition type onto the matching total, so
// the gap shown to a student is in the same units as the requirement.
func currentValueFor(condType string, p *Progress) int {
	switch condType {
	case "first_session":
		return p.SessionCount
	case "pages_total":
		return p.TotalPages
	case "genres":
		return p.DistinctGenres
	case "streak":
		return p.Student.StreakDays
	default:
		return 0
	}
}

func (s *Store) recentSessions(ctx context.Context, studentID int) ([]RecentSession, error) {
	rows, err := s.pg.Pool.Query(ctx, `
		SELECT b.title, b.genre, rs.pages_read, rs.minutes_spent, rs.session_date
		FROM reading_sessions rs
		JOIN books b ON b.id = rs.book_id
		WHERE rs.student_id = $1
		ORDER BY rs.session_date DESC, rs.id DESC
		LIMIT $2`, studentID, recentSessionLimit)
	if err != nil {
		return nil, fmt.Errorf("reading recent sessions: %w", err)
	}
	defer rows.Close()

	var out []RecentSession
	for rows.Next() {
		var r RecentSession
		if err := rows.Scan(&r.Title, &r.Genre, &r.PagesRead, &r.MinutesSpent, &r.SessionDate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
