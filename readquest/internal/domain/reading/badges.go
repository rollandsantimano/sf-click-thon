package reading

import (
	"context"
	"fmt"
)

type Badge struct {
	Name        string
	Description string
}

// BadgeProgress describes a badge not yet earned, and how close the student is.
//
// Showing the gap is the point: "38 more pages" is a nudge, where a bare
// locked badge is only a reminder of failure.
type BadgeProgress struct {
	Badge
	Have int
	Need int
}

// awardBadges grants every badge whose condition the student now meets.
//
// It runs inside LogSession's transaction, so a badge can never be granted for
// a session that subsequently rolls back.
//
// The whole evaluation is one statement rather than a read-then-write in Go.
// That matters because the conditions are data — rows in the badges table — so
// adding a badge is a seed change, not a code change, and because ON CONFLICT
// makes re-awarding impossible without a separate "already earned?" query.
//
// streak is passed in rather than re-read: LogSession has just computed it in
// the same transaction, and re-reading would risk disagreeing with it.
func awardBadges(ctx context.Context, q queryer, studentID, streak int) ([]Badge, error) {
	rows, err := q.Query(ctx, `
		WITH stats AS (
		    SELECT count(*)                        AS sessions,
		           coalesce(sum(rs.pages_read), 0) AS pages_total,
		           -- 'Unknown' is the placeholder genre given to auto-created
		           -- books, so excluding it stops a typo from counting toward
		           -- Genre Explorer.
		           count(DISTINCT b.genre) FILTER (WHERE b.genre <> 'Unknown') AS genres
		    FROM reading_sessions rs
		    JOIN books b ON b.id = rs.book_id
		    WHERE rs.student_id = $1
		),
		earned AS (
		    INSERT INTO student_badges (student_id, badge_id)
		    SELECT $1, bd.id
		    FROM badges bd, stats st
		    WHERE CASE bd.condition_type
		        WHEN 'first_session' THEN st.sessions    >= bd.condition_value
		        WHEN 'pages_total'   THEN st.pages_total >= bd.condition_value
		        WHEN 'genres'        THEN st.genres      >= bd.condition_value
		        WHEN 'streak'        THEN $2             >= bd.condition_value
		        ELSE false
		    END
		    ON CONFLICT (student_id, badge_id) DO NOTHING
		    RETURNING badge_id
		)
		SELECT b.name, b.description
		FROM badges b JOIN earned e ON e.badge_id = b.id
		ORDER BY b.id`,
		studentID, streak,
	)
	if err != nil {
		return nil, fmt.Errorf("awarding badges: %w", err)
	}
	defer rows.Close()

	var earned []Badge
	for rows.Next() {
		var b Badge
		if err := rows.Scan(&b.Name, &b.Description); err != nil {
			return nil, fmt.Errorf("reading awarded badge: %w", err)
		}
		earned = append(earned, b)
	}
	return earned, rows.Err()
}
