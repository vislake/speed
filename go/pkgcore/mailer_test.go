package pkgcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConsoleMailer_PrintFormat pins the exact record NewConsoleMailer prints,
// because tests elsewhere and operators filtering logs both depend on it. A
// message with both bodies prints a header block, one section per non-empty
// body with its body verbatim, and a closing marker.
func TestConsoleMailer_PrintFormat(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	mailer := newConsoleMailer(&out)

	err := mailer.Send(context.Background(), Mail{
		From:    "ops@example.com",
		To:      []string{"ada@example.com", "grace@example.com"},
		Subject: "Your invoice #1042 is ready",
		Text:    "Hello Ada,\n\nyour invoice is ready.\n",
		HTML:    "<p>Hello Ada,</p>",
	})
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}

	want := "" +
		"[mail] from: ops@example.com\n" +
		"[mail] to: ada@example.com, grace@example.com\n" +
		"[mail] subject: Your invoice #1042 is ready\n" +
		"[mail] text/plain:\n" +
		"Hello Ada,\n" +
		"\n" +
		"your invoice is ready.\n" +
		"[mail] text/html:\n" +
		"<p>Hello Ada,</p>\n" +
		"[mail] end\n"
	if got := out.String(); got != want {
		t.Errorf("printed record = %q, want %q", got, want)
	}
}

// TestConsoleMailer_OmitsTheEmptyBodySection checks the other two body
// shapes: a text-only message prints no HTML section, and an HTML-only
// message prints no text section.
func TestConsoleMailer_OmitsTheEmptyBodySection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mail Mail
		want string
	}{
		{
			name: "text only",
			mail: Mail{
				From: "ops@example.com", To: []string{"ada@example.com"},
				Subject: "text only", Text: "plain body",
			},
			want: "" +
				"[mail] from: ops@example.com\n" +
				"[mail] to: ada@example.com\n" +
				"[mail] subject: text only\n" +
				"[mail] text/plain:\nplain body\n" +
				"[mail] end\n",
		},
		{
			name: "html only",
			mail: Mail{
				From: "ops@example.com", To: []string{"ada@example.com"},
				Subject: "html only", HTML: "<p>html body</p>",
			},
			want: "" +
				"[mail] from: ops@example.com\n" +
				"[mail] to: ada@example.com\n" +
				"[mail] subject: html only\n" +
				"[mail] text/html:\n<p>html body</p>\n" +
				"[mail] end\n",
		},
		{
			name: "body without a trailing newline still closes its section",
			mail: Mail{
				From: "ops@example.com", To: []string{"ada@example.com"},
				Subject: "no trailing newline", Text: "no newline at the end",
			},
			want: "" +
				"[mail] from: ops@example.com\n" +
				"[mail] to: ada@example.com\n" +
				"[mail] subject: no trailing newline\n" +
				"[mail] text/plain:\nno newline at the end\n" +
				"[mail] end\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := newConsoleMailer(&out).Send(context.Background(), tt.mail); err != nil {
				t.Fatalf("Send() error = %v, want nil", err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("printed record = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestConsoleMailer_CancelledContext_FailsWithoutWriting pins the interface
// rule that a cancelled context fails Send before anything else happens.
func TestConsoleMailer_CancelledContext_FailsWithoutWriting(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	mailer := newConsoleMailer(&out)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := mailer.Send(ctx, Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "never sent", Text: "never printed",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error = %v, want it to wrap context.Canceled", err)
	}
	if out.Len() != 0 {
		t.Errorf("writer received %q, want nothing written after cancellation", out.String())
	}
}

// TestConsoleMailer_RejectsInvalidMail exercises the validity rules every
// Mailer implementation shares, through the console mailer's Send: each
// invalid message fails with ErrInvalidMail and prints nothing. The SMTP
// mailer rejects the same messages, which its own tests pin against the same
// table-shaped expectations.
func TestConsoleMailer_RejectsInvalidMail(t *testing.T) {
	t.Parallel()

	valid := Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "valid", Text: "body",
	}
	tests := []struct {
		name string
		mut  func(*Mail)
	}{
		{"an empty From", func(m *Mail) { m.From = "" }},
		{"no recipients", func(m *Mail) { m.To = nil }},
		{"an empty recipient", func(m *Mail) { m.To = []string{""} }},
		{"a newline in From", func(m *Mail) { m.From = "ops@example.com\r\nBcc: ada@example.com" }},
		{"a newline in To", func(m *Mail) { m.To = []string{"ada@example.com\nbcc@example.com"} }},
		{"a newline in Subject", func(m *Mail) { m.Subject = "subject\r\nInjected: header" }},
		{"no body at all", func(m *Mail) { m.Text = ""; m.HTML = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mail := valid
			tt.mut(&mail)

			var out bytes.Buffer
			err := newConsoleMailer(&out).Send(context.Background(), mail)
			if !errors.Is(err, ErrInvalidMail) {
				t.Fatalf("Send() error = %v, want it to wrap ErrInvalidMail", err)
			}
			if out.Len() != 0 {
				t.Errorf("writer received %q, want nothing printed for an invalid message", out.String())
			}
		})
	}
}

// TestConsoleMailer_ConcurrentSendsDoNotInterleave pins the mutex guarantee:
// every printed message must appear as one contiguous record even when many
// goroutines send at once, so a log reader never sees two messages blurred
// together.
func TestConsoleMailer_ConcurrentSendsDoNotInterleave(t *testing.T) {
	t.Parallel()

	const senders = 8
	const sendsPerSender = 25

	var out bytes.Buffer
	mailer := newConsoleMailer(&out)

	var wg sync.WaitGroup
	for sender := 0; sender < senders; sender++ {
		wg.Add(1)
		go func(sender int) {
			defer wg.Done()
			for i := 0; i < sendsPerSender; i++ {
				mail := Mail{
					From:    fmt.Sprintf("sender-%d@example.com", sender),
					To:      []string{"ada@example.com"},
					Subject: fmt.Sprintf("message %d from sender %d", i, sender),
					Text:    fmt.Sprintf("body %d-%d", sender, i),
				}
				if err := mailer.Send(context.Background(), mail); err != nil {
					t.Errorf("Send() error = %v, want nil", err)
					return
				}
			}
		}(sender)
	}
	wg.Wait()

	records := strings.Split(out.String(), "[mail] end\n")
	// The trailing marker ends the output; splitting on it yields one empty
	// final element.
	if len(records) != senders*sendsPerSender+1 || records[len(records)-1] != "" {
		t.Fatalf("split on the end marker yielded %d records, want %d", len(records)-1, senders*sendsPerSender)
	}
	seen := make(map[string]int)
	for _, record := range records[:len(records)-1] {
		// The split above leaves each record ending in the newline that closed
		// its body block, so splitting on "\n" yields the five content lines
		// plus the empty tail.
		lines := strings.Split(record, "\n")
		if len(lines) != 6 || lines[5] != "" {
			t.Errorf("record %q has %d lines, want the fixed five-line shape plus the split tail", record, len(lines))
			continue
		}
		subject := strings.TrimPrefix(lines[2], "[mail] subject: ")
		if _, dup := seen[subject]; dup {
			t.Errorf("record with subject %q printed twice", subject)
		}
		seen[subject]++
	}
	if len(seen) != senders*sendsPerSender {
		t.Errorf("saw %d distinct records, want %d", len(seen), senders*sendsPerSender)
	}
}

// zeroProgressWriter violates the io.Writer contract by reporting neither
// progress nor an error. It exists to pin the guard that keeps Send from
// spinning forever on such a writer; before the guard existed this test timed
// out instead of failing cleanly.
type zeroProgressWriter struct{}

func (zeroProgressWriter) Write(p []byte) (int, error) { return 0, nil }

// TestConsoleMailer_WriterMakingNoProgress_Fails runs Send against the
// contract-violating writer above in a goroutine, so a regression to the
// spin loop fails the test after a second instead of hanging the whole suite.
func TestConsoleMailer_WriterMakingNoProgress_Fails(t *testing.T) {
	t.Parallel()

	mailer := newConsoleMailer(zeroProgressWriter{})
	mail := Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "never printed", Text: "body",
	}

	done := make(chan error, 1)
	go func() { done <- mailer.Send(context.Background(), mail) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Send() error = nil, want a failure from a writer that makes no progress")
		}
		if !strings.Contains(err.Error(), "made no progress") {
			t.Errorf("Send() error = %v, want it to name the writer's missing progress", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send() did not return: a writer reporting (0, nil) must fail the send, not spin")
	}
}
