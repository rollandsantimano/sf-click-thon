package reading

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrStudentNotFound and ErrAmbiguous carry a candidate list because the
	// caller is a chat model: an error it can read aloud ("did you mean Maya
	// or Marcus?") recovers in one turn, where a bare failure does not.
	ErrStudentNotFound = errors.New("student not found")
	ErrAmbiguous       = errors.New("ambiguous name")
)

// ResolutionError adds the candidates that were considered to a resolution
// failure.
type ResolutionError struct {
	Err        error
	Query      string
	Candidates []string
}

func (e *ResolutionError) Error() string {
	if len(e.Candidates) == 0 {
		return fmt.Sprintf("%v: %q", e.Err, e.Query)
	}
	return fmt.Sprintf("%v: %q (did you mean: %s?)",
		e.Err, e.Query, strings.Join(e.Candidates, ", "))
}

func (e *ResolutionError) Unwrap() error { return e.Err }

// resolveStudent maps a spoken name to a student row.
//
// Chat users never know their integer id, so every student-facing tool takes a
// name and funnels through here. Exact match is tried first so that a student
// whose full name is a prefix of another's still resolves unambiguously.
func resolveStudent(ctx context.Context, q queryer, name string) (*Student, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: a student name is required", ErrInvalidInput)
	}

	// Columns must be table-qualified: classes also has a "name".
	students, err := queryStudents(ctx, q, `lower(s.name) = lower($1)`, name)
	if err != nil {
		return nil, err
	}

	// Only widen to a substring search when the exact name found nothing —
	// otherwise "Maya Chen" would also drag in "Maya Chenoweth".
	if len(students) == 0 {
		students, err = queryStudents(ctx, q, `s.name ILIKE '%' || $1 || '%'`, name)
		if err != nil {
			return nil, err
		}
	}

	switch len(students) {
	case 1:
		return &students[0], nil
	case 0:
		known, _ := allStudentNames(ctx, q)
		return nil, &ResolutionError{Err: ErrStudentNotFound, Query: name, Candidates: known}
	default:
		return nil, &ResolutionError{Err: ErrAmbiguous, Query: name, Candidates: namesOf(students)}
	}
}

func queryStudents(ctx context.Context, q queryer, where string, arg any) ([]Student, error) {
	// The class join exists to feed the denormalised class_name on ClickHouse
	// events — the analytics side has no way to look it up later.
	rows, err := q.Query(ctx, `
		SELECT s.id, s.name, s.class_id, coalesce(c.name, ''), s.xp, s.streak_days, s.last_session_date
		FROM students s
		LEFT JOIN classes c ON c.id = s.class_id
		WHERE `+where+` ORDER BY s.name`, arg)
	if err != nil {
		return nil, fmt.Errorf("looking up student: %w", err)
	}
	defer rows.Close()

	var out []Student
	for rows.Next() {
		var s Student
		if err := rows.Scan(&s.ID, &s.Name, &s.ClassID, &s.ClassName, &s.XP, &s.StreakDays, &s.LastSessionDate); err != nil {
			return nil, fmt.Errorf("reading student row: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func allStudentNames(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT name FROM students ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func namesOf(students []Student) []string {
	names := make([]string, len(students))
	for i, s := range students {
		names[i] = s.Name
	}
	return names
}

// resolveBook maps a free-text title to a book row, creating a placeholder if
// the catalogue has no match.
//
// Auto-creating is a deliberate trade: a child reading something outside a
// 12-book seed must still be able to log it, and a failed lookup mid-demo is
// worse than an imperfect catalogue row. The cost is that a typo becomes a new
// book — hence the created flag, so the chat layer can surface it.
//
// Placeholder rows carry genre 'Unknown', which the Genre Explorer badge
// excludes, so auto-creation cannot inflate a distinct-genre count.
func resolveBook(ctx context.Context, q queryer, title string) (book *Book, created bool, err error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, false, fmt.Errorf("%w: a book title is required", ErrInvalidInput)
	}

	book, err = findBook(ctx, q, `lower(title) = lower($1)`, title)
	if err != nil {
		return nil, false, err
	}
	if book != nil {
		return book, false, nil
	}

	book, err = findBook(ctx, q, `title ILIKE '%' || $1 || '%'`, title)
	if err != nil {
		return nil, false, err
	}
	if book != nil {
		slog.Info("book matched by substring", "input", title, "matched", book.Title)
		return book, false, nil
	}

	// ON CONFLICT guards the unique index on lower(title): a concurrent insert
	// of the same title must return the existing row, not fail the session.
	var b Book
	err = q.QueryRow(ctx, `
		INSERT INTO books (title) VALUES ($1)
		ON CONFLICT (lower(title)) DO UPDATE SET title = books.title
		RETURNING id, title, author, genre, age_min, age_max`,
		title,
	).Scan(&b.ID, &b.Title, &b.Author, &b.Genre, &b.AgeMin, &b.AgeMax)
	if err != nil {
		return nil, false, fmt.Errorf("adding book %q to catalogue: %w", title, err)
	}

	slog.Info("book not in catalogue, placeholder created", "title", title, "book_id", b.ID)
	return &b, true, nil
}

// findBook returns nil (not an error) when nothing matches, so callers can
// distinguish "no match, try something wider" from a real query failure.
func findBook(ctx context.Context, q queryer, where string, arg any) (*Book, error) {
	var b Book
	err := q.QueryRow(ctx, `
		SELECT id, title, author, genre, age_min, age_max
		FROM books WHERE `+where+` ORDER BY id LIMIT 1`, arg,
	).Scan(&b.ID, &b.Title, &b.Author, &b.Genre, &b.AgeMin, &b.AgeMax)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up book: %w", err)
	}
	return &b, nil
}
