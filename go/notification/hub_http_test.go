package notification

// hub_http_test.go drives the inbox stream route (handler.go's handleStream,
// fed by hub.go) through a real HTTP server and client -- the transport
// handler_test.go's flushRecorder deliberately bypasses. Where that file
// asserts what the handler writes to a hand-made ResponseWriter, this file
// asserts what a genuine net/http connection delivers: a client that
// connects to the route over httptest.NewServer reads the announcement
// frames the route flushes, receives nothing for announcements that are not
// its own, and sees the connection end when it cancels the request -- the
// real-server half of the stream's contract (the route's framing and
// filtering rules themselves live in handler_test.go, which reaches the
// same code without a socket).
//
// The route subscribes to the hub before it writes its 200 headers, so a
// client whose request has returned has a subscription that is already
// live: every announcement published after that point is guaranteed to be
// considered by the connection, and the frame ordering assertions below
// rest on that.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// hubServer wraps one handlerEnv behind a real HTTP server whose request
// context carries the tenant -- the shape of a host's composed stack, where
// tenancy middleware has already run by the time the request reaches the
// handler.
type hubServer struct {
	env    *handlerEnv
	server *httptest.Server
}

// newHubServer starts a hubServer over a fresh handlerEnv.
func newHubServer(t *testing.T) *hubServer {
	t.Helper()
	env := newHandlerEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Derive from the request's own context rather than replacing it
		// (tenantCtx builds on context.Background): the stream handler ends
		// the connection when r.Context() is done, and a replacement that
		// drops the cancellation chain would never see the client go away.
		// A real host's tenancy middleware derives the same way.
		ctx := pkgcore.WithTenant(r.Context(), pkgcore.TenantID(handlerTenant))
		env.h.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(server.Close)
	return &hubServer{env: env, server: server}
}

// openStream starts one streaming request and returns its response body
// reader; the request has returned (200 headers arrived) by the time it
// returns, so announcements published afterwards cannot race the
// subscription. The caller cancels ctx to end the stream.
func (s *hubServer) openStream(t *testing.T, ctx context.Context) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.server.URL+apiPath+"/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	return resp, bufio.NewReader(resp.Body)
}

// readSSEFrame reads one frame off the stream: the lines up to the blank
// line that ends each "event: message\ndata: ...\n\n" pair the route
// writes. It fails the test if no complete frame arrives within five
// seconds, so a stream that silently stops writing cannot hang the suite.
func readSSEFrame(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	type outcome struct {
		frame string
		err   error
	}
	read := make(chan outcome, 1)
	go func() {
		var frame strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				read <- outcome{frame: frame.String(), err: err}
				return
			}
			if line == "\n" {
				read <- outcome{frame: frame.String()}
				return
			}
			frame.WriteString(line)
		}
	}()
	select {
	case got := <-read:
		if got.err != nil {
			t.Fatalf("read an SSE frame: %v (partial frame %q)", got.err, got.frame)
		}
		return got.frame
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for an SSE frame")
		return ""
	}
}

// assertFrameIsTheAnnouncement fails t unless frame is the route's standard
// "event: message" / "data: <json>" pair whose data is an announcement for
// messageID. readSSEFrame keeps the blank line that ends each frame, so the
// trailing newline is trimmed before splitting into the two content lines.
func assertFrameIsTheAnnouncement(t *testing.T, frame, messageID string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "event: message" || !strings.HasPrefix(lines[1], "data: ") {
		t.Fatalf("frame = %q, want an event/data pair", frame)
	}
	var ann map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ann); err != nil {
		t.Fatalf("frame data is not JSON: %v (%q)", err, frame)
	}
	if ann["message_id"] != messageID || ann["recipient_user_id"] != handlerUser || ann["tenant_id"] != handlerTenant {
		t.Errorf("announcement = %v, want the announced message for the caller", ann)
	}
}

// TestHubHTTP_Stream_DeliversAnnouncementsAsFramesOverARealServer drives the
// stream's happy path end to end over a real HTTP connection: two
// announcements for the caller arrive as two frames, in order, each the
// event/data pair carrying the announcement JSON.
func TestHubHTTP_Stream_DeliversAnnouncementsAsFramesOverARealServer(t *testing.T) {
	server := newHubServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, reader := server.openStream(t, ctx)
	server.env.announceInbox(t, "message-1", handlerUser, handlerTenant)
	assertFrameIsTheAnnouncement(t, readSSEFrame(t, reader), "message-1")
	server.env.announceInbox(t, "message-2", handlerUser, handlerTenant)
	assertFrameIsTheAnnouncement(t, readSSEFrame(t, reader), "message-2")
}

// TestHubHTTP_Stream_NeverDeliversAnnouncementsForOthers pins the route's
// filtering over a real connection: announcements for another recipient or
// another tenant are published first, and the first frame the client reads
// is still its own -- a mis-forwarded foreign announcement would arrive
// ahead of it and fail the assertion.
func TestHubHTTP_Stream_NeverDeliversAnnouncementsForOthers(t *testing.T) {
	server := newHubServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, reader := server.openStream(t, ctx)
	server.env.announceInbox(t, "m-foreign-user", "user-8", handlerTenant)
	server.env.announceInbox(t, "m-foreign-tenant", handlerUser, handlerOtherTenant)
	server.env.announceInbox(t, "m-mine", handlerUser, handlerTenant)
	assertFrameIsTheAnnouncement(t, readSSEFrame(t, reader), "m-mine")
}

// TestHubHTTP_Stream_CancellingTheRequestEndsTheConnection pins the
// stream's termination over a real transport: when the client cancels the
// request, the server-side handler ends (its request context is cancelled
// through the connection) and the client's read reaches end of stream --
// the connection is not held open by a handler that forgot its context.
func TestHubHTTP_Stream_CancellingTheRequestEndsTheConnection(t *testing.T) {
	server := newHubServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	_, reader := server.openStream(t, ctx)
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := reader.ReadString('\n')
		if err != nil {
			// The transport reports the cancel either as the closed
			// connection or as the request context's own error; both are
			// the connection ending, which is what the cancel demanded.
			if err == io.EOF || errors.Is(err, context.Canceled) ||
				strings.Contains(err.Error(), "closed") {
				return
			}
			t.Fatalf("read after cancel: %v", err)
		}
		// A line may already be buffered from the headers; keep reading
		// until the closed connection surfaces.
	}
	t.Fatalf("the stream did not end within five seconds of the cancel")
}
