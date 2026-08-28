package reading

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrNoPendingQuestion = errors.New("no pending comprehension question")

// PendingQuestion is the unanswered question waiting for a student's response.
type PendingQuestion struct {
	ID        int
	BookTitle string
	Question  string
	CreatedAt time.Time
}

// StoreQuestion persists the comprehension question generated after a session.
// Returns the question's ID so the caller can reference it later.
func (s *Store) StoreQuestion(ctx context.Context, sessionID, studentID int, bookTitle, question string) (int, error) {
	var id int
	err := s.pg.Pool.QueryRow(ctx, `
		INSERT INTO session_questions (session_id, student_id, book_title, question)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		sessionID, studentID, bookTitle, question,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("storing comprehension question: %w", err)
	}
	return id, nil
}

// GetPendingQuestion returns the most recent unanswered question for a student,
// resolved by name using the same fuzzy resolver all other tools use.
//
// "Most recent unanswered" rather than "most recent overall" matters: a student
// who logs two sessions before answering the first question still gets one
// question at a time, in order.
func (s *Store) GetPendingQuestion(ctx context.Context, studentName string) (*PendingQuestion, error) {
	student, err := resolveStudent(ctx, s.pg.Pool, studentName)
	if err != nil {
		return nil, err
	}

	var q PendingQuestion
	err = s.pg.Pool.QueryRow(ctx, `
		SELECT id, book_title, question, created_at
		FROM session_questions
		WHERE student_id = $1
		  AND answered_at IS NULL
		ORDER BY created_at ASC
		LIMIT 1`,
		student.ID,
	).Scan(&q.ID, &q.BookTitle, &q.Question, &q.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w for %s", ErrNoPendingQuestion, student.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("fetching pending question: %w", err)
	}
	return &q, nil
}

// RecordAnswer stores the student's answer and Claude's evaluation against the
// question row. The evaluation is deliberately stored even if it is empty —
// a blank evaluation signals "we tried but couldn't evaluate" rather than
// "this question was never answered."
func (s *Store) RecordAnswer(ctx context.Context, questionID int, answer, evaluation string) error {
	_, err := s.pg.Pool.Exec(ctx, `
		UPDATE session_questions
		SET answer      = $2,
		    evaluation  = $3,
		    answered_at = now()
		WHERE id = $1`,
		questionID, answer, evaluation,
	)
	if err != nil {
		return fmt.Errorf("recording answer: %w", err)
	}
	return nil
}
