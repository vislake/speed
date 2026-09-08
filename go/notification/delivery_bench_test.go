package notification

import "testing"

// delivery_bench_test.go benchmarks the delivery pipeline's key-derivation
// hotspot: deriveDeliveryKey runs once per channel of every delivery attempt
// (the send-record settle and the replay probe both recompute it), so its
// cost is paid on the platform's most frequent delivery path. Part of the
// module-owned benchmark set docs/internal/20-quality-and-security.md's
// performance plan calls for; no external services are involved.
//
// Run: go test -bench=BenchmarkDeriveDeliveryKey -benchmem .

// deliveryBenchSink keeps the last derived key reachable so the compiler
// cannot elide the derivation.
var deliveryBenchSink string

// BenchmarkDeriveDeliveryKey measures one delivery-key derivation for a
// realistic appointment-reminder dispatch: canonical JSON of the delivery
// seed (recipient, type, channel, locale and the rendered parameters) plus
// the SHA-256 over it. The dispatch is the same shape the module's delivery
// tests drive; only ASCII parameter values are used here so the benchmark
// stays inside the tree's no-CJK rule.
func BenchmarkDeriveDeliveryKey(b *testing.B) {
	d := Dispatch{
		TypeKey: "clinic.appointment_reminder",
		Recipient: DispatchRecipient{
			Class:  RecipientClassUser,
			UserID: "user-1042",
		},
		Locale: "zh-CN",
		Params: map[string]any{
			"patient_name":     "patient-8891",
			"appointment_time": "2026-09-10 09:30",
			"clinic_name":      "clinic-7",
			"room":             "room-12",
			"doctor_name":      "doctor-33",
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	var last string
	for i := 0; i < b.N; i++ {
		key, err := deriveDeliveryKey("tenant-acme", d, ChannelEmail)
		if err != nil {
			b.Fatalf("deriveDeliveryKey() error = %v", err)
		}
		last = key
	}
	deliveryBenchSink = last
}
