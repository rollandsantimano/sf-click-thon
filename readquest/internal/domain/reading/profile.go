package reading

import (
	"context"
	"fmt"
)

// RecommendationProfile is everything a librarian would want to know before
// suggesting a book to a child.
type RecommendationProfile struct {
	StudentName string
	Level       string

	// AgeMin/AgeMax is an inferred reading band, not a stored fact — see
	// inferAgeBand for why.
	AgeMin int
	AgeMax int

	Recent      []RecentSession
	AlreadyRead []string
}

// Default band when a student has no usable history to infer from. Wide on
// purpose: a recommendation that is slightly off is recoverable, one that
// assumes a narrow level and gets it wrong is discouraging.
const (
	fallbackAgeMin = 6
	fallbackAgeMax = 12

	recommendationHistoryLimit = 10
)

// GetRecommendationProfile gathers a student's reading history and infers the
// age band to pitch a recommendation at.
func (s *Store) GetRecommendationProfile(ctx context.Context, studentName string) (*RecommendationProfile, error) {
	student, err := resolveStudent(ctx, s.pg.Pool, studentName)
	if err != nil {
		return nil, err
	}

	rows, err := s.pg.Pool.Query(ctx, `
		SELECT b.title, b.genre, rs.pages_read, rs.minutes_spent, rs.session_date,
		       b.age_min, b.age_max
		FROM reading_sessions rs
		JOIN books b ON b.id = rs.book_id
		WHERE rs.student_id = $1
		ORDER BY rs.session_date DESC, rs.id DESC
		LIMIT $2`, student.ID, recommendationHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("reading history for recommendation: %w", err)
	}
	defer rows.Close()

	profile := &RecommendationProfile{
		StudentName: student.Name,
		Level:       LevelFor(student.XP),
	}

	var ageMins, ageMaxes []int
	seen := map[string]bool{}
	for rows.Next() {
		var r RecentSession
		var ageMin, ageMax *int
		if err := rows.Scan(&r.Title, &r.Genre, &r.PagesRead, &r.MinutesSpent,
			&r.SessionDate, &ageMin, &ageMax); err != nil {
			return nil, err
		}

		profile.Recent = append(profile.Recent, r)
		if !seen[r.Title] {
			seen[r.Title] = true
			profile.AlreadyRead = append(profile.AlreadyRead, r.Title)
		}

		// Auto-created books carry genre 'Unknown' and null ages; including
		// them would drag the inferred band toward whatever the fallback is
		// rather than toward what the child actually reads.
		if r.Genre != "Unknown" && ageMin != nil && ageMax != nil {
			ageMins = append(ageMins, *ageMin)
			ageMaxes = append(ageMaxes, *ageMax)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	profile.AgeMin, profile.AgeMax = s.inferAgeBand(ctx, ageMins, ageMaxes)
	return profile, nil
}

// inferAgeBand derives a reading level from the books a student has actually
// finished.
//
// No age or grade is stored on a student, and adding one would mean asking a
// child their age in a chat interface — worse for privacy and worse for the
// demo. Averaging the bands of books they have read is a better signal anyway:
// it reflects what they can actually handle rather than their birthday.
func (s *Store) inferAgeBand(ctx context.Context, mins, maxes []int) (int, int) {
	if len(mins) > 0 && len(maxes) > 0 {
		return average(mins), average(maxes)
	}

	// No usable personal history — fall back to what this catalogue is aimed
	// at overall before falling back to a hardcoded band.
	var classMin, classMax *float64
	err := s.pg.Pool.QueryRow(ctx, `
		SELECT avg(age_min), avg(age_max) FROM books WHERE genre <> 'Unknown'`,
	).Scan(&classMin, &classMax)
	if err == nil && classMin != nil && classMax != nil {
		return int(*classMin), int(*classMax)
	}
	return fallbackAgeMin, fallbackAgeMax
}

func average(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return sum / len(xs)
}
