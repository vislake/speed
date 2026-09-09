package twilio_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/sms/twilio"
)

// ExampleNewSender shows the documented construction-and-send shape a host
// wires: a Config holding the operator's account data -- the From-based
// sender variant; a MessagingServiceSid-based config is the documented
// alternative -- a sender implementing pkgcore's SMSSender seam (so it can
// be handed to go/authn's WithSMSSender or go/notification's), then one Send
// per message. The transport is scripted (WithClient) so the example runs
// offline; a real
// host omits the option and the sender talks to Twilio's API through the
// SSRF-guarded default client.
func ExampleNewSender() {
	sender, err := twilio.NewSender(twilio.Config{
		AccountSID: "AC11111111111111111111111111111111",
		AuthToken:  "example-auth-token",
		From:       "+15005550000",
	}, twilio.WithClient(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader(`{"sid":"SM123","status":"queued"}`)),
			Header:     make(http.Header),
		}, nil
	})}))
	if err != nil {
		fmt.Println(err)
		return
	}

	if err := sender.Send(context.Background(), pkgcore.SMS{To: "+8613800000000", Text: "your code is 123456"}); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("delivered")
	// Output: delivered
}

// roundTripFunc adapts a function to http.RoundTripper for the scripted
// transport above.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
