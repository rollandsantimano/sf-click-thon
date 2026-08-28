package dashboard

import (
	"context"
	"fmt"
	"time"
)

// suspiciousRatePagesPerMinute is the analytics threshold — below the hard cap
// that rejects at write time (5 pages/min), but worth surfacing to a teacher.
// A genuine fast reader who reads 2.5 pages/minute is possible; consistently
// logging at this rate is worth a second look.
const suspiciousRatePagesPerMinute = 2.0

// SuspiciousSession describes one session that triggered a flag.
type SuspiciousSession struct {
	StudentName    string
	BookTitle      string
	Genre          string
	PagesRead      int
	MinutesSpent   int
	PagesPerMinute float64
	SessionDate    time.Time
	Reason         string
}

// SuspiciousSessions returns sessions from the last 30 days that match one of
// two patterns:
//
//  1. Rate anomaly — pages/minute above the suspicious threshold. These are not
//     blocked at write time (only clearly impossible rates are), so they arrive
//     here for teacher review.
//
//  2. Burst logging — more than 2 sessions logged by the same student on the
//     same calendar day. Genuine multi-session days happen; 3+ in one day is
//     unusual enough to surface.
//
// ClickHouse is the right place for both queries: they aggregate over the full
// event history without touching Postgres, and both benefit from the ordering
// key's pruning on class_id.
func (s *Store) SuspiciousSessions(ctx context.Context, className string) ([]SuspiciousSession, error) {
	classID, resolvedName, err := s.resolveClass(ctx, className)
	if err != nil {
		return nil, err
	}
	_ = resolvedName

	rate, err := s.rateAnomalies(ctx, classID)
	if err != nil {
		return nil, err
	}

	bursts, err := s.burstLogging(ctx, classID)
	if err != nil {
		return nil, err
	}

	return append(rate, bursts...), nil
}

// rateAnomalies flags sessions where pages/minutes exceeds the threshold.
func (s *Store) rateAnomalies(ctx context.Context, classID int) ([]SuspiciousSession, error) {
	rows, err := s.ch.Conn.Query(ctx, `
		SELECT student_name,
		       book_title,
		       genre,
		       pages_read,
		       minutes_spent,
		       toFloat64(pages_read) / toFloat64(minutes_spent) AS rate,
		       session_date
		FROM reading_events
		WHERE class_id = ?
		  AND session_date >= today() - 30
		  AND toFloat64(pages_read) / toFloat64(minutes_spent) > ?
		ORDER BY rate DESC`,
		int32(classID), suspiciousRatePagesPerMinute)
	if err != nil {
		return nil, fmt.Errorf("querying rate anomalies: %w", err)
	}
	defer rows.Close()

	var out []SuspiciousSession
	for rows.Next() {
		var ss SuspiciousSession
		if err := rows.Scan(
			&ss.StudentName, &ss.BookTitle, &ss.Genre,
			&ss.PagesRead, &ss.MinutesSpent, &ss.PagesPerMinute, &ss.SessionDate,
		); err != nil {
			return nil, err
		}
		ss.Reason = fmt.Sprintf("reading rate %.1f pages/min (threshold %.0f)", ss.PagesPerMinute, suspiciousRatePagesPerMinute)
		out = append(out, ss)
	}
	return out, rows.Err()
}

// burstLogging flags students who logged more than 2 sessions on the same day.
//
// The query returns one row per (student, day) pair that exceeded the limit —
// not the individual sessions — because what matters for teacher review is the
// pattern, not each individual session.
func (s *Store) burstLogging(ctx context.Context, classID int) ([]SuspiciousSession, error) {
	rows, err := s.ch.Conn.Query(ctx, `
		SELECT student_name,
		       session_date,
		       count()          AS n,
		       sum(pages_read)  AS total_pages,
		       sum(minutes_spent) AS total_minutes
		FROM reading_events
		WHERE class_id = ?
		  AND session_date >= today() - 30
		GROUP BY student_name, session_date
		HAVING n > 2
		ORDER BY n DESC, session_date DESC`,
		int32(classID))
	if err != nil {
		return nil, fmt.Errorf("querying burst logging: %w", err)
	}
	defer rows.Close()

	var out []SuspiciousSession
	for rows.Next() {
		var name string
		var date time.Time
		var n uint64
		var totalPages, totalMinutes int64 // sum() over Int32 widens to Int64
		if err := rows.Scan(&name, &date, &n, &totalPages, &totalMinutes); err != nil {
			return nil, err
		}
		out = append(out, SuspiciousSession{
			StudentName: name,
			SessionDate: date,
			Reason: fmt.Sprintf("%d sessions logged on the same day (%d pages, %d min total)",
				n, totalPages, totalMinutes),
		})
	}
	return out, rows.Err()
}