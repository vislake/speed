package aliyun_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/sms/aliyun"
)

// ExampleNewSender shows the documented construction-and-send shape a host
// wires: a Config holding the operator's account data, a sender implementing
// pkgcore's SMSSender seam (so it can be handed to go/authn's WithSMSSender
// or go/notification's), then one Send per message. The transport is
// scripted (WithClient) so the example runs offline; a real host omits the
// option and the sender talks to Aliyun's gateway through the SSRF-guarded
// default client.
func ExampleNewSender() {
	sender, err := aliyun.NewSender(aliyun.Config{
		AccessKeyID:     "LTAI-example-key-id",
		AccessKeySecret: "example-secret",
		SignName:        "example-sign",
		TemplateCode:    "SMS_0000001",
	}, aliyun.WithClient(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"Code":"OK","Message":"OK","RequestId":"REQ-1"}`)),
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
