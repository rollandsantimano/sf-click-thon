package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

const comprehensionTimeout = 20 * time.Second

var generateQuestionPrompt = `You are a children's librarian checking in with a young reader.

Generate ONE comprehension question about the book the student just finished reading.
The question must:
- Be answerable only by someone who actually read the book (not guessable from the title or genre alone)
- Ask about a specific plot point, character moment, or detail from the story
- Be written in simple, friendly language suitable for a child aged 6-14
- Be a single sentence ending with a question mark

Reply with only the question. No preamble, no explanation.`

var evaluateAnswerPrompt = `You are a warm, encouraging children's librarian evaluating a student's answer to a reading comprehension question.

Be kind but honest. You are checking whether the student actually read the book — not grading them.

Reply in 1-2 short sentences. Address the student directly. Choose one of these tones:
- If the answer shows they read it: affirm specifically what they got right
- If the answer is partially right: gently note what was right and what was off
- If the answer suggests they didn't read it: say so kindly and encourage them to try the book

Do not give away the correct answer. Do not use exclamation marks. Do not use emoji.`

// GenerateComprehensionQuestion asks Claude for one specific question about a
// book that only a reader could answer.
//
// It uses the same client as RecommendBook — a nil Recommender means the key
// is absent, and the caller should skip question generation rather than fail.
func (r *Recommender) GenerateComprehensionQuestion(ctx context.Context, bookTitle, genre string) (string, error) {
	if r == nil {
		return "", ErrNotConfigured
	}

	ctx, cancel := context.WithTimeout(ctx, comprehensionTimeout)
	defer cancel()

	userMsg := fmt.Sprintf("Book: %s\nGenre: %s", bookTitle, genre)

	resp, err := r.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:        model,
		MaxTokens:    256, // a question is short; a large ceiling wastes tokens
		OutputConfig: anthropic.OutputConfigParam{Effort: effort},
		System: []anthropic.TextBlockParam{{
			Text: generateQuestionPrompt,
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("generating comprehension question: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", fmt.Errorf("model declined to generate a question")
	}

	var out strings.Builder
	for _, block := range resp.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(text.Text)
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// EvaluateAnswer asks Claude whether a student's free-text answer suggests
// they actually read the book.
//
// Evaluation is deliberately soft — it guides rather than gates. The student's
// XP has already been awarded by the time this runs.
func (r *Recommender) EvaluateAnswer(ctx context.Context, bookTitle, question, answer string) (string, error) {
	if r == nil {
		return "", ErrNotConfigured
	}

	ctx, cancel := context.WithTimeout(ctx, comprehensionTimeout)
	defer cancel()

	userMsg := fmt.Sprintf("Book: %s\nQuestion asked: %s\nStudent's answer: %s",
		bookTitle, question, answer)

	resp, err := r.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:        model,
		MaxTokens:    256,
		OutputConfig: anthropic.OutputConfigParam{Effort: effort},
		System: []anthropic.TextBlockParam{{
			Text: evaluateAnswerPrompt,
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("evaluating answer: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", fmt.Errorf("model declined to evaluate the answer")
	}

	var out strings.Builder
	for _, block := range resp.Content {
		if text, ok := block.AsAny().(anthropic.TextBlock); ok {
			out.WriteString(text.Text)
		}
	}
	return strings.TrimSpace(out.String()), nil
}
