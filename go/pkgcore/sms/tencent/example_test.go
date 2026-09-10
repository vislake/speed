package tencent_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/sms/tencent"
)

// ExampleNewSender shows the documented construction-and-send shape a host
// wires: a Config holding the operator's account data -- the Templates map
// naming, per "<locale>/<message-id>" identity, the approved template and
// the seam parameter names its positional variables take -- a sender
// implementing pkgcore's SMSSender seam (so it can be handed to go/authn's
// WithSMSSender or go/notification's), then one Send per message. The
// transport is scripted (WithClient) so the
// example runs offline; a real host omits the option and the sender talks to
// Tencent's gateway through the SSRF-guarded default client.
func ExampleNewSender() {
	sender, err := tencent.NewSender(tencent.Config{
		SecretID:  "AKID-example-secret-id",
		SecretKey: "example-secret-key",
		SdkAppID:  "1400006666",
		SignName:  "example-sign",
		Templates: map[string]tencent.Template{
			"zh-CN/authn.sms.verification_code": {
				ID:     "1234567",
				Params: []string{"code", "minutes"},
			},
		},
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

	if err := sender.Send(context.Background(), pkgcore.SMS{
		To:        "+8613800000000",
		Text:      "your code is 123456, valid for 5 minutes",
		MessageID: "authn.sms.verification_code",
		Locale:    "zh-CN",
		Params:    map[string]string{"code": "123456", "minutes": "5"},
	}); err != nil {
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
