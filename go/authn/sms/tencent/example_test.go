package tencent_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/sms/tencent"
)

// ExampleNewSender shows the documented construction-and-send shape a host
// wires: a Config holding the operator's account data, a sender implementing
// the module's SMSSender seam (so it can be handed to authn's WithSMSSender),
// then one Send per message. The transport is scripted (WithClient) so the
// example runs offline; a real host omits the option and the sender talks to
// Tencent's gateway through the SSRF-guarded default client.
func ExampleNewSender() {
	sender, err := tencent.NewSender(tencent.Config{
		SecretID:   "AKID-example-secret-id",
		SecretKey:  "example-secret-key",
		SdkAppID:   "1400006666",
		SignName:   "example-sign",
		TemplateID: "1234567",
	}, tencent.WithClient(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Response":{"SendStatusSet":[{"Code":"Ok","Message":"send success"}],"RequestId":"REQ-1"}}`)),
			Header:     make(http.Header),
		}, nil
	})}))
	if err != nil {
		fmt.Println(err)
		return
	}

	if err := sender.Send(context.Background(), authn.SMS{To: "+8613800000000", Text: "your code is 123456"}); err != nil {
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
