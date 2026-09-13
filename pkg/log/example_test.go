package log_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/vislake/speed/pkg/log"
)

// moduleLogger stands in for the logging capability the registry hands a
// module in its New callback. The example that uses it is compiled and not
// run, so nothing is ever called on it.
var moduleLogger log.Logger

// Example shows the shape a module takes logging up in: it registers its
// redaction rules and takes a named logger in its own New, and its request
// path carries the logger in the context.
//
// This example is compiled rather than run. Both halves of it resist a fixed
// expectation: the capability is handed out by the registry during a real
// assembly, which a test binary cannot start, and a log record carries the
// time it was written, so its rendered text is never the same twice. The
// examples below carry expected output and are run.
func Example() {
	// Registration comes before the logger is taken. Named binds an
	// attribute, and an attribute bound through With is judged once, at
	// binding time, against the rules that exist then; a rule registered
	// afterwards does not reach it.
	moduleLogger.Redaction().AddKeys("password", "token")
	logger := moduleLogger.Named("http")

	// The injection point puts the logger in the context. Everything
	// downstream takes it from there, so a handler deep in the call path
	// logs to the configured destinations without being passed a logger.
	ctx := log.WithLogger(context.Background(), logger)
	ctx = log.WithAttrs(ctx, "tenant_id", "t-42")
	log.FromContext(ctx).InfoContext(ctx, "request served", "duration_ms", 12)
}

// ExampleFromContext shows what a call site gets when its context carries no
// logger: the process logger, not a no-op and not a panic.
//
// A missing injection therefore costs the configured destinations rather than
// the records themselves — the path's logs turn up on standard output, which
// is visible without being a failure.
func ExampleFromContext() {
	ctx := context.Background()

	fmt.Println(log.FromContext(ctx) == log.Default())
	// Output:
	// true
}

// ExampleWithLogger shows the injection point: the logger goes into the
// context once, and every call site below it takes the same one back.
//
// The logger to inject is the one taken from the capability through Named.
// Injecting Default() would take the whole path off the configured
// destinations.
func ExampleWithLogger() {
	logger := log.Default().With("request_id", "r-7")
	ctx := log.WithLogger(context.Background(), logger)

	fmt.Println(log.FromContext(ctx) == logger)
	// The original context is unchanged, so a sibling path is unaffected.
	fmt.Println(log.FromContext(context.Background()) == logger)
	// Output:
	// true
	// false
}

// ExampleWithAttrs shows attributes accumulating on the logger a context
// carries, so the call sites below do not repeat what the request already
// knows.
func ExampleWithAttrs() {
	ctx := log.WithAttrs(context.Background(), "tenant_id", "t-42")
	ctx = log.WithAttrs(ctx, "user_id", "u-9")

	// Both attributes now travel with every record written through this
	// context, and the process logger is untouched.
	fmt.Println(log.FromContext(ctx) != log.Default())
	// Output:
	// true
}

// ExampleMatcher shows a value-shape rule: it reports the spans of a value
// that need masking and leaves the rest of the text alone.
//
// The matcher runs on every resolved string value and error text of every
// record in the process, so its cost lands on the whole logging path.
func ExampleMatcher() {
	var bearer log.Matcher = func(value string) []log.Span {
		const prefix = "Bearer "
		if !strings.HasPrefix(value, prefix) {
			return nil
		}
		return []log.Span{{Start: len(prefix), End: len(value)}}
	}

	fmt.Println(bearer("Bearer abc123"))
	fmt.Println(bearer("no credential here"))
	// Output:
	// [{7 13}]
	// []
}
