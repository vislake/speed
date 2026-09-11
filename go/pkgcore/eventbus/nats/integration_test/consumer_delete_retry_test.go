//go:build integration

package nats_test

// The bounded retry TestEventBus_ReaderRecoversFromALostConsumer uses when
// it manufactures its precondition -- a durable consumer removed out from
// under a running reader -- together with hermetic tests of that retry's two
// halves, neither of which needs a server: the predicate that classifies a
// failed delete, and the loop that acts on the classification.
//
// The removal can race a delivery in flight: nats-server refuses to remove a
// consumer while its observation directory still holds files from a delivery
// being written, reporting the refusal as its generic 500-class stream
// failure (JSStreamGeneralErrorF, err_code 10051) whose description carries
// the underlying OS error text -- the raw unlinkat failure naming the
// consumer's obs directory and ending "directory not empty". The same
// delete succeeds once that delivery lands, so that exact shape is retried,
// while every other failure -- a different code, or this code with any
// other description -- is surfaced immediately instead of being masked by
// retries, and a failure that persists exhausts the bounded attempts.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// jsErrCodeStreamGeneralFailure is nats-server's JSStreamGeneralErrorF error
// code: the generic server-side failure whose description carries the
// underlying error text -- which is why the predicate matching it must match
// the description as well, never the code alone.
const jsErrCodeStreamGeneralFailure jetstream.ErrorCode = 10051

// The retry's shape: bounded attempts spaced by a brief backoff, both sized
// to span a delivery in flight without stalling a genuinely broken delete.
const (
	consumerDeleteAttempts     = 5
	consumerDeleteRetryBackoff = 150 * time.Millisecond
)

// consumerDeleter is the one method deleteConsumerWithRetry calls, so the
// retry loop can be driven hermetically by a scripted fake;
// jetstream.JetStream satisfies it structurally.
type consumerDeleter interface {
	DeleteConsumer(ctx context.Context, stream, consumer string) error
}

// directoryNotEmptyDeleteErr returns the JetStream API error the server
// reports for the delete race: the general-failure code carrying the raw
// unlinkat error for the consumer's observation directory.
func directoryNotEmptyDeleteErr() *jetstream.APIError {
	return &jetstream.APIError{
		Code:      500,
		ErrorCode: jsErrCodeStreamGeneralFailure,
		Description: "unlinkat /tmp/nats/jetstream/$G/streams/PKGCORE_EVENTS_invoice_paid/obs/" +
			"pkgcore-bus-f1dfe08091819e8bfffa95f8: directory not empty",
	}
}

// isRetryableConsumerDeleteErr reports whether err is the transient
// consumer-delete failure in which the server could not remove the
// consumer's observation directory because a delivery was still writing to
// it. Every other error reports false, so the retry never reruns a call
// whose failure it does not understand.
func isRetryableConsumerDeleteErr(err error) bool {
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode == jsErrCodeStreamGeneralFailure &&
		strings.Contains(apiErr.Description, "directory not empty")
}

// deleteConsumerWithRetry deletes the consumer, retrying only the
// directory-not-empty race isRetryableConsumerDeleteErr recognizes. A
// failure of any other kind is returned after the first call, one that
// persists is returned after consumerDeleteAttempts calls, and success --
// including one reached after retries -- is nil.
func deleteConsumerWithRetry(ctx context.Context, js consumerDeleter, streamName, consumerName string) error {
	var err error
	for attempt := 1; attempt <= consumerDeleteAttempts; attempt++ {
		if err = js.DeleteConsumer(ctx, streamName, consumerName); err == nil || !isRetryableConsumerDeleteErr(err) {
			return err
		}
		time.Sleep(consumerDeleteRetryBackoff)
	}
	return err
}

// scriptedConsumerDeleter is a consumerDeleter whose every call returns the
// next scripted result, making the retry loop's attempt count observable
// without a server.
type scriptedConsumerDeleter struct {
	results []error
	calls   int
}

func (d *scriptedConsumerDeleter) DeleteConsumer(context.Context, string, string) error {
	if d.calls >= len(d.results) {
		return fmt.Errorf("scriptedConsumerDeleter: call %d has no scripted result", d.calls+1)
	}
	err := d.results[d.calls]
	d.calls++
	return err
}

// TestIsRetryableConsumerDeleteErr pins the predicate's split: the exact
// server-reported delete race classifies as retryable -- bare and wrapped --
// and every other shape does not.
func TestIsRetryableConsumerDeleteErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the directory-not-empty delete race",
			err:  directoryNotEmptyDeleteErr(),
			want: true,
		},
		{
			name: "the same failure wrapped by a caller",
			err:  fmt.Errorf("delete consumer: %w", directoryNotEmptyDeleteErr()),
			want: true,
		},
		{
			name: "the general-failure code with another description",
			err: &jetstream.APIError{
				Code:        500,
				ErrorCode:   jsErrCodeStreamGeneralFailure,
				Description: "unlinkat /tmp/nats/jetstream/$G/streams/s/obs/c: permission denied",
			},
			want: false,
		},
		{
			name: "another API error code",
			err: &jetstream.APIError{
				Code:        404,
				ErrorCode:   jetstream.JSErrCodeConsumerNotFound,
				Description: "consumer not found",
			},
			want: false,
		},
		{
			name: "a server failure the client did not wrap as an API error",
			err:  errors.New("nats: connection closed"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableConsumerDeleteErr(tc.err); got != tc.want {
				t.Errorf("isRetryableConsumerDeleteErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDeleteConsumerWithRetry pins the retry loop hermetically: a transient
// race is retried until it clears, a persistent one exhausts the bounded
// attempts and is surfaced, and a failure the predicate does not recognize
// is surfaced without a retry.
func TestDeleteConsumerWithRetry(t *testing.T) {
	foreign := &jetstream.APIError{
		Code:        404,
		ErrorCode:   jetstream.JSErrCodeConsumerNotFound,
		Description: "consumer not found",
	}
	persistent := []error{
		directoryNotEmptyDeleteErr(), directoryNotEmptyDeleteErr(), directoryNotEmptyDeleteErr(),
		directoryNotEmptyDeleteErr(), directoryNotEmptyDeleteErr(),
	}

	cases := []struct {
		name      string
		results   []error
		wantErr   error
		wantCalls int
	}{
		{
			name:      "a transient race clears on the third attempt",
			results:   []error{directoryNotEmptyDeleteErr(), directoryNotEmptyDeleteErr(), nil},
			wantErr:   nil,
			wantCalls: 3,
		},
		{
			name:      "a persistent race exhausts the bounded attempts",
			results:   persistent,
			wantErr:   persistent[consumerDeleteAttempts-1],
			wantCalls: consumerDeleteAttempts,
		},
		{
			name:      "a foreign failure is surfaced immediately",
			results:   []error{foreign},
			wantErr:   foreign,
			wantCalls: 1,
		},
		{
			name:      "success on the first attempt",
			results:   []error{nil},
			wantErr:   nil,
			wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedConsumerDeleter{results: tc.results}
			gotErr := deleteConsumerWithRetry(context.Background(), fake, "PKGCORE_EVENTS_invoice_paid", "consumer")
			if tc.wantErr == nil {
				if gotErr != nil {
					t.Errorf("deleteConsumerWithRetry() error = %v, want nil", gotErr)
				}
			} else if !errors.Is(gotErr, tc.wantErr) {
				t.Errorf("deleteConsumerWithRetry() error = %v, want %v", gotErr, tc.wantErr)
			}
			if fake.calls != tc.wantCalls {
				t.Errorf("DeleteConsumer called %d times, want %d", fake.calls, tc.wantCalls)
			}
		})
	}
}
