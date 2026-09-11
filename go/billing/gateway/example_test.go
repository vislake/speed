package gateway_test

import (
	"fmt"

	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/billing/gateway"
)

// ExampleOutTradeNo shows the merchant order-number derivation both
// domestic providers share: the caller's own idempotency key when it set
// one -- so a retried CreateCharge reaches the same channel-side order --
// and the invoice id otherwise.
func ExampleOutTradeNo() {
	fmt.Println(gateway.OutTradeNo(billing.ChargeRequest{IdempotencyKey: "job-42", InvoiceID: "inv-7"}))
	fmt.Println(gateway.OutTradeNo(billing.ChargeRequest{InvoiceID: "inv-7"}))
	// Output:
	// job-42
	// inv-7
}

// ExampleRequireCNY shows the settlement-currency guard every domestic
// provider's CreateCharge calls first: CNY passes case-insensitively, any
// other currency is refused with the channel named in the error's params.
func ExampleRequireCNY() {
	fmt.Println(gateway.RequireCNY("alipay", "cny"))
	fmt.Println(gateway.RequireCNY("wechat", "USD"))
	// Output:
	// <nil>
	// billing.unsupported_currency
}
