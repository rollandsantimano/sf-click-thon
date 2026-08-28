// Package ai turns a student's reading history into a book recommendation
// using Claude.
//
// This is the one part of ReadQuest that cannot be done with SQL. Everything
// else — XP, streaks, badges, at-risk ranking — is arithmetic over stored
// facts. Choosing what a particular child might enjoy next is judgement, and
// it is where the model earns its place in the app rather than decorating it.
package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"readquest/internal/domain/reading"
)

// ErrNotConfigured is returned when no API key is set. It is a distinct error
// so the chat layer can say "this feature needs a key" rather than reporting a
// generic failure — every other tool keeps working without one.
var ErrNotConfigured = errors.New("ANTHROPIC_API_KEY is not set, so book recommendations are unavailable")

const (
	model = "claude-opus-5"

	// A recommendation is a title, an author and a sentence. The ceiling is
	// generous only because thinking tokens count against it.
	maxTokens = 2000

	// Low effort is deliberate: picking a children's book from a short history
	// is not a hard reasoning problem, and this runs live in front of an
	// audience where latency is felt. Quality has not suffered in testing.
	effort = anthropic.OutputConfigEffortLow

	requestTimeout = 30 * time.Second
)

const systemPrompt = `You are a warm, knowledgeable children's librarian helping a child pick their next book.

Recommend exactly ONE real, published children's book. Reply in this shape, and nothing else:

<title> by <author>
Then one or two short sentences, addressed to the child, on why they might like it.

Rules:
- Recommend a real book that genuinely exists. Never invent a title or author.
- Do not recommend anything the child has already read.
- Match the reading level you are given. When in doubt, pick slightly easier — a book finished builds more confidence than a book abandoned.
- Connect the suggestion to something specific they have already enjoyed.
- Speak to the child directly, plainly and without condescension. No emoji, no exclamation marks, no preamble.`

type Recommender struct {
	client *anthropic.Client
}

// NewRecommender returns nil when no key is configured. Callers check for nil
// rather than receiving a broken client, so a missing key is a visible state
// instead of a runtime surprise on first use.
func NewRecommender(apiKey string) *Recommender {
	if apiKey == "" {
		slog.Warn("ANTHROPIC_API_KEY not set — recommend_book will report itself as unavailable")
		return nil
	}
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Recommender{client: &c}
}

// RecommendBook asks Claude for one suggestion tailored to this student.
func (r *Recommender) RecommendBook(ctx context.Context, p *reading.RecommendationProfile) (string, error) {
	if r == nil {
		return "", ErrNotConfigured
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	prompt := buildPrompt(p)
	slog.Info("requesting book recommendation",
		"student", p.StudentName, "age_band", fmt.Sprintf("%d-%d", p.AgeMin, p.AgeMax),
		"history", len(p.Recent))

	resp, err := r.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:        model,
		MaxTokens:    maxTokens,
		OutputConfig: anthropic.OutputConfigParam{Effort: effort},
		System: []anthropic.TextBlockParam{{
			Text: systemPrompt,
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("asking Claude for a recommendation: %w", err)
	}

	// A safety refusal arrives as a normal 200 with no usable text, so
	// stop_reason has to be checked before reading content.
	if resp.StopReason == anthropic.StopReasonRefusal {
		slog.Warn("recommendation refused", "category", resp.StopDetails.Category)
		return "", fmt.Errorf("the model declined to answer this request")
	}

	var out strings.Builder
	for _, block := range resp.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(text.Text)
		}
	}

	answer := strings.TrimSpace(out.String())
	if answer == "" {
		return "", fmt.Errorf("the model returned an empty recommendation")
	}
	return answer, nil
}

// buildPrompt lays out the history most-recent-first, since what a child read
// yesterday says more about what they want next than what they read a month
// ago.
func buildPrompt(p *reading.RecommendationProfile) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Child's name: %s\n", p.StudentName)
	fmt.Fprintf(&b, "Reading level: books for ages %d-%d\n\n", p.AgeMin, p.AgeMax)

	if len(p.Recent) == 0 {
		b.WriteString("They have not logged any reading yet, so suggest an engaging, " +
			"accessible starting point for this age range.\n")
		return b.String()
	}

	b.WriteString("Books they have read, most recent first:\n")
	for _, r := range p.Recent {
		fmt.Fprintf(&b, "- %s (%s), %d pages in %d minutes\n",
			r.Title, r.Genre, r.PagesRead, r.MinutesSpent)
	}

	fmt.Fprintf(&b, "\nDo not recommend any of these: %s\n", strings.Join(p.AlreadyRead, "; "))
	return b.String()
}
