package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"readquest/internal/ai"
	"readquest/internal/domain/reading"
)

// toolNames exists so startup can report how many tools were registered
// without reaching into the MCP server's internals.
var toolNames = []string{
	"log_reading_session",
	"get_student_progress",
	"get_book_list",
	"get_class_dashboard",
	"recommend_book",
	"get_suspicious_sessions",
	"answer_comprehension_question",
}

// registerTools wires the domain onto the MCP surface.
//
// Tool descriptions are load-bearing, not documentation: they are the only
// thing the model reads when deciding which tool a sentence like "I read
// Matilda for half an hour" calls for. They are written to say when to reach
// for the tool, not merely what it does.
func (s *Server) registerTools() {
	s.mcp.AddTool(mcp.NewTool("log_reading_session",
		mcp.WithDescription(
			"Record that a student has finished a reading session. Use this whenever a "+
				"student says they read something — for example 'I read 30 pages of Matilda "+
				"today' or 'Maya finished two chapters in 20 minutes'. Awards XP, updates "+
				"their daily streak, and unlocks any badges they have earned. If the book is "+
				"not in the catalogue it is added automatically."),
		// mcp-go defaults destructiveHint to true. Logging a session only
		// appends — nothing is overwritten or deleted — and leaving the
		// default would invite a client to demand confirmation before a child
		// can record that they read a book.
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("student_name", mcp.Required(),
			mcp.Description("The student's name. A first name or partial name is fine.")),
		mcp.WithString("book_title", mcp.Required(),
			mcp.Description("Title of the book. Approximate titles are matched to the catalogue.")),
		mcp.WithNumber("pages_read", mcp.Required(),
			mcp.Description("How many pages the student read in this session.")),
		mcp.WithNumber("minutes_spent", mcp.Required(),
			mcp.Description("How many minutes the student spent reading.")),
	), s.handleLogSession)

	s.mcp.AddTool(mcp.NewTool("get_student_progress",
		mcp.WithDescription(
			"Look up one student's reading standing: XP, level, streak, badges earned, how "+
				"far they are from the next badge, and their recent books. Use this when a "+
				"student asks how they are doing, what badges they have, or what to aim for next."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("student_name", mcp.Required(),
			mcp.Description("The student's name. A first name or partial name is fine.")),
	), s.handleGetProgress)

	s.mcp.AddTool(mcp.NewTool("get_book_list",
		mcp.WithDescription(
			"List books in the ReadQuest catalogue, optionally filtered by genre or a word "+
				"in the title. Use this when a student asks what they could read, or wants to "+
				"browse a particular kind of book."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("filter",
			mcp.Description("Optional genre or title fragment, e.g. 'fantasy' or 'robot'. "+
				"Omit to list the whole catalogue.")),
	), s.handleGetBooks)

	s.mcp.AddTool(mcp.NewTool("get_class_dashboard",
		mcp.WithDescription(
			"For teachers: list every student in a class ranked by who most needs attention. "+
				"Flags students who have never read, who have gone quiet for a week or more, "+
				"or whose reading pace has dropped. Use this for questions like 'who is falling "+
				"behind?', 'how is my class doing?' or 'who should I check on today?'."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("class_name",
			mcp.Description("Optional class name. Omit when there is only one class.")),
	), s.handleDashboard)

	s.mcp.AddTool(mcp.NewTool("answer_comprehension_question",
		mcp.WithDescription(
			"Submit a student's answer to their pending comprehension question. Use this "+
				"immediately after the student responds to the question you asked following "+
				"their reading session. Returns Claude's gentle evaluation of whether the "+
				"answer suggests the student actually read the book. XP is not affected — "+
				"the evaluation is feedback, not a gate."),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true), // calls Claude to evaluate
		mcp.WithString("student_name", mcp.Required(),
			mcp.Description("The student's name. A first name or partial name is fine.")),
		mcp.WithString("answer", mcp.Required(),
			mcp.Description("The student's verbatim answer to the comprehension question.")),
	), s.handleAnswerQuestion)

	s.mcp.AddTool(mcp.NewTool("get_suspicious_sessions",
		mcp.WithDescription(
			"For teachers: identify reading sessions that may indicate reward-hacking or "+
				"cheating. Flags two patterns: sessions with an implausibly high reading rate "+
				"(more than 2 pages per minute) and students who logged multiple sessions on "+
				"the same day. Use this when a teacher wants to verify session integrity, or "+
				"when a student's XP suddenly jumps in a way that seems inconsistent with "+
				"their past reading. Returns sessions from the last 30 days."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithString("class_name",
			mcp.Description("Optional class name. Omit when there is only one class.")),
	), s.handleSuspiciousSessions)

	s.mcp.AddTool(mcp.NewTool("recommend_book",
		mcp.WithDescription(
			"Suggest one book a specific student would enjoy next, chosen from their reading "+
				"history and pitched at their reading level. Use this when a student asks what "+
				"to read next, says they are bored or stuck, or finishes a book and wants "+
				"another. Returns a single suggestion with a reason."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		// The only tool that genuinely reaches outside this system: it calls
		// the Claude API rather than reading the databases.
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithString("student_name", mcp.Required(),
			mcp.Description("The student's name. A first name or partial name is fine.")),
	), s.handleRecommend)
}

func (s *Server) handleRecommend(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	studentName, err := req.RequireString("student_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	profile, err := s.app.Reading.GetRecommendationProfile(ctx, studentName)
	if err != nil {
		return toolError("recommend_book", err)
	}

	suggestion, err := s.app.Recommender.RecommendBook(ctx, profile)
	if err != nil {
		// A missing key is a configuration state, not a failure the model
		// should retry — say so plainly so it stops rather than looping.
		if errors.Is(err, ai.ErrNotConfigured) {
			return mcp.NewToolResultError(
				"Book recommendations are not available: the server has no Anthropic API key " +
					"configured. All other ReadQuest features still work."), nil
		}
		return toolError("recommend_book", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Recommendation for %s (reading level: ages %d-%d):\n\n",
		profile.StudentName, profile.AgeMin, profile.AgeMax)
	b.WriteString(suggestion)
	b.WriteString("\n\nPresent this to the student as-is; do not substitute a different book.\n")

	return mcp.NewToolResultText(b.String()), nil
}

func (s *Server) handleLogSession(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	studentName, err := req.RequireString("student_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	bookTitle, err := req.RequireString("book_title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	pages, err := req.RequireInt("pages_read")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	minutes, err := req.RequireInt("minutes_spent")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	res, err := s.app.Reading.LogSession(ctx, studentName, bookTitle, pages, minutes)
	if err != nil {
		return toolError("log_reading_session", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Session logged for %s.\n", res.Student.Name)
	fmt.Fprintf(&b, "Book: %s (%s)\n", res.Book.Title, res.Book.Genre)
	fmt.Fprintf(&b, "Read: %d pages in %d minutes\n", res.PagesRead, res.MinutesSpent)
	fmt.Fprintf(&b, "XP awarded: +%d (total %d)\n", res.XPAwarded, res.TotalXP)
	fmt.Fprintf(&b, "Level: %s\n", res.Level)
	fmt.Fprintf(&b, "Streak: %d day(s)\n", res.StreakDays)

	if len(res.NewBadges) > 0 {
		b.WriteString("Badges unlocked in this session:\n")
		for _, badge := range res.NewBadges {
			fmt.Fprintf(&b, "  - %s: %s\n", badge.Name, badge.Description)
		}
	} else {
		b.WriteString("No new badges this time.\n")
	}

	if res.BookWasCreated {
		fmt.Fprintf(&b, "\nNote: %q was not in the catalogue, so it was added with an unknown "+
			"genre. Mention this in case the title was misheard.\n", res.Book.Title)
	}

	// Generate a comprehension question and append it to the response.
	// The question is generated after the session is committed so a Claude
	// failure never blocks XP from being awarded.
	//
	// The final instruction tells the model what to do next — without it,
	// the model may continue the conversation instead of asking the question.
	question := s.generateAndStoreQuestion(ctx, res)
	if question != "" {
		fmt.Fprintf(&b, "\nComprehension check — ask the student this question before continuing:\n\n")
		fmt.Fprintf(&b, "  %s\n\n", question)
		fmt.Fprintf(&b, "Once the student answers, call answer_comprehension_question with their response.\n")
	}

	return mcp.NewToolResultText(b.String()), nil
}

// generateAndStoreQuestion calls Claude and persists the question. It returns
// an empty string on any failure rather than surfacing errors: question
// generation is best-effort and must never disrupt a session that has already
// been committed.
func (s *Server) generateAndStoreQuestion(ctx context.Context, res *reading.SessionResult) string {
	if s.app.Recommender == nil {
		return ""
	}
	// Auto-created placeholder books have genre 'Unknown' and no real content.
	// Generating a question for "The Test Book 20260828" would be embarrassing
	// mid-demo and meaningless as a comprehension check.
	if res.Book.Genre == "Unknown" {
		return ""
	}

	question, err := s.app.Recommender.GenerateComprehensionQuestion(ctx, res.Book.Title, res.Book.Genre)
	if err != nil {
		slog.Warn("comprehension question generation failed — session unaffected",
			"book", res.Book.Title, "error", err)
		return ""
	}

	if _, err := s.app.Reading.StoreQuestion(
		ctx, res.SessionID, res.Student.ID, res.Book.Title, question,
	); err != nil {
		slog.Warn("storing comprehension question failed — session unaffected",
			"book", res.Book.Title, "error", err)
		return ""
	}
	return question
}

func (s *Server) handleAnswerQuestion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	studentName, err := req.RequireString("student_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	answer, err := req.RequireString("answer")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	pending, err := s.app.Reading.GetPendingQuestion(ctx, studentName)
	if err != nil {
		return toolError("answer_comprehension_question", err)
	}

	// Evaluate with Claude. On failure, store a blank evaluation rather than
	// surfacing an error — the answer is still recorded for teacher review.
	var evaluation string
	if s.app.Recommender != nil {
		evaluation, err = s.app.Recommender.EvaluateAnswer(ctx, pending.BookTitle, pending.Question, answer)
		if err != nil {
			slog.Warn("answer evaluation failed — recording answer without evaluation",
				"book", pending.BookTitle, "error", err)
		}
	}

	if err := s.app.Reading.RecordAnswer(ctx, pending.ID, answer, evaluation); err != nil {
		return toolError("answer_comprehension_question", err)
	}

	slog.Info("comprehension answer recorded",
		"book", pending.BookTitle, "student", studentName,
		"has_evaluation", evaluation != "")

	var b strings.Builder
	if evaluation != "" {
		b.WriteString(evaluation)
	} else {
		b.WriteString("Thanks for answering — your response has been noted.")
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (s *Server) handleGetProgress(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	studentName, err := req.RequireString("student_name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	p, err := s.app.Reading.GetProgress(ctx, studentName)
	if err != nil {
		return toolError("get_student_progress", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s — level %s, %d XP\n", p.Student.Name, p.Level, p.Student.XP)
	fmt.Fprintf(&b, "Sessions: %d | Pages: %d | Minutes: %d | Genres: %d | Streak: %d day(s)\n",
		p.SessionCount, p.TotalPages, p.TotalMinutes, p.DistinctGenres, p.Student.StreakDays)

	b.WriteString("\nBadges earned:\n")
	if len(p.Earned) == 0 {
		b.WriteString("  (none yet)\n")
	}
	for _, badge := range p.Earned {
		fmt.Fprintf(&b, "  - %s: %s\n", badge.Name, badge.Description)
	}

	if len(p.Locked) > 0 {
		b.WriteString("\nStill to earn (current/required):\n")
		for _, badge := range p.Locked {
			fmt.Fprintf(&b, "  - %s: %d/%d — %s\n",
				badge.Name, badge.Have, badge.Need, badge.Description)
		}
	}

	if len(p.Recent) > 0 {
		b.WriteString("\nRecent reading:\n")
		for _, r := range p.Recent {
			fmt.Fprintf(&b, "  - %s (%s), %d pages in %d min on %s\n",
				r.Title, r.Genre, r.PagesRead, r.MinutesSpent, r.SessionDate.Format("2006-01-02"))
		}
	}
	return mcp.NewToolResultText(b.String()), nil
}

func (s *Server) handleGetBooks(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	filter := req.GetString("filter", "")

	rows, err := s.app.PG.Pool.Query(ctx, `
		SELECT title, author, genre, age_min, age_max, coalesce(description, '')
		FROM books
		WHERE $1 = '' OR title ILIKE '%' || $1 || '%' OR genre ILIKE '%' || $1 || '%'
		ORDER BY genre, title`, filter)
	if err != nil {
		return toolError("get_book_list", err)
	}
	defer rows.Close()

	var b strings.Builder
	var n int
	for rows.Next() {
		var title, author, genre, desc string
		var ageMin, ageMax *int
		if err := rows.Scan(&title, &author, &genre, &ageMin, &ageMax, &desc); err != nil {
			return toolError("get_book_list", err)
		}
		n++

		fmt.Fprintf(&b, "- %s by %s (%s", title, author, genre)
		if ageMin != nil && ageMax != nil {
			fmt.Fprintf(&b, ", ages %d-%d", *ageMin, *ageMax)
		}
		b.WriteString(")")
		if desc != "" {
			fmt.Fprintf(&b, ": %s", desc)
		}
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		return toolError("get_book_list", err)
	}

	if n == 0 {
		return mcp.NewToolResultText(
			fmt.Sprintf("No books match %q. Try a different genre or title.", filter)), nil
	}

	header := fmt.Sprintf("%d books in the catalogue", n)
	if filter != "" {
		header = fmt.Sprintf("%d books matching %q", n, filter)
	}
	return mcp.NewToolResultText(header + ":\n" + b.String()), nil
}

func (s *Server) handleDashboard(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	className := req.GetString("class_name", "")

	board, err := s.app.Dashboard.ClassDashboard(ctx, className)
	if err != nil {
		return toolError("get_class_dashboard", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Class: %s\n", board.ClassName)
	fmt.Fprintf(&b, "Summary: %s.\n\n", board.AttentionSummary())

	// Ordering is the product here: the list is already ranked most-urgent
	// first, so the model should present it in the order it arrives.
	b.WriteString("Students, ranked by who most needs attention:\n")
	for i, st := range board.Students {
		flag := "OK"
		if st.AtRisk {
			flag = "NEEDS ATTENTION"
		}
		fmt.Fprintf(&b, "%d. %s [%s] — %s\n", i+1, st.Name, flag, st.Reason)
		fmt.Fprintf(&b, "   level %s, %d XP, %d day streak, %d pages in the last 7 days (%.1f/day), last read %s\n",
			st.Level, st.XP, st.StreakDays, st.PagesLast7d, st.VelocityPerDay, lastReadPhrase(st.DaysSinceRead))
	}
	return mcp.NewToolResultText(b.String()), nil
}

func lastReadPhrase(daysSince *int) string {
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

func (s *Server) handleSuspiciousSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	className := req.GetString("class_name", "")

	sessions, err := s.app.Dashboard.SuspiciousSessions(ctx, className)
	if err != nil {
		return toolError("get_suspicious_sessions", err)
	}

	if len(sessions) == 0 {
		return mcp.NewToolResultText(
			"No suspicious sessions detected in the last 30 days. " +
				"All recorded reading rates and session patterns look plausible."), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d suspicious session(s) detected in the last 30 days:\n\n", len(sessions))
	for i, ss := range sessions {
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, ss.StudentName, ss.Reason)
		if ss.BookTitle != "" {
			fmt.Fprintf(&b, "   Book: %s (%s), %d pages in %d min\n",
				ss.BookTitle, ss.Genre, ss.PagesRead, ss.MinutesSpent)
		}
		fmt.Fprintf(&b, "   Date: %s\n", ss.SessionDate.Format("2006-01-02"))
	}
	b.WriteString("\nThese are flagged for teacher review, not automatically penalised. ")
	b.WriteString("Consider following up with the student directly.")
	return mcp.NewToolResultText(b.String()), nil
}

// toolError decides whether a failure is the model's problem or ours.
//
// Domain errors are handed back as tool results so the conversation can
// recover in one turn — a "did you mean Maya Chen?" is far more useful to a
// child than a protocol failure. Infrastructure errors are returned as real
// errors, because no rewording of the request will fix a dead database.
func toolError(tool string, err error) (*mcp.CallToolResult, error) {
	var resErr *reading.ResolutionError
	switch {
	case errors.As(err, &resErr),
		errors.Is(err, reading.ErrInvalidInput),
		errors.Is(err, reading.ErrNoPendingQuestion):
		slog.Info("tool call rejected", "tool", tool, "reason", err)
		return mcp.NewToolResultError(err.Error()), nil
	default:
		slog.Error("tool call failed", "tool", tool, "error", err)
		return nil, err
	}
}
