package app

import (
	"bytes"
	"testing"
)

// TestPlatformKeyMaterial_MapsEveryKey pins the ServerConfig-to-platform
// mapping: each of the six resolved materials lands on the declaration field
// whose key path the modules declare, so the engine's cipher and the
// binding verification run against the values this host resolved.
func TestPlatformKeyMaterial_MapsEveryKey(t *testing.T) {
	cfg := ServerConfig{
		ConfigKey:            bytes.Repeat([]byte{0x11}, 32),
		OrgIndexKey:          bytes.Repeat([]byte{0x22}, 32),
		NotificationIndexKey: bytes.Repeat([]byte{0x33}, 32),
		PKILocalKeyCipherKey: bytes.Repeat([]byte{0x44}, 32),
		AuthnBlindIndexKey:   bytes.Repeat([]byte{0x55}, 32),
		AuthnPIICipherKey:    bytes.Repeat([]byte{0x66}, 32),
	}

	platform := platformKeyMaterial(cfg)
	for _, tc := range []struct {
		key  string
		got  []byte
		want []byte
	}{
		{"config.cipher_key", platform.Config.Cipher_Key, cfg.ConfigKey},
		{"org.invitation_email_index_key", platform.Org.Invitation_Email_Index_Key, cfg.OrgIndexKey},
		{"notification.contact_index_key", platform.Notification.Contact_Index_Key, cfg.NotificationIndexKey},
		{"pki.local_key_cipher_key", platform.PKI.Local_Key_Cipher_Key, cfg.PKILocalKeyCipherKey},
		{"authn.blind_index_key", platform.Authn.Blind_Index_Key, cfg.AuthnBlindIndexKey},
		{"authn.pii_cipher_key", platform.Authn.PII_Cipher_Key, cfg.AuthnPIICipherKey},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Errorf("platform material %s = %x, want the ServerConfig value %x", tc.key, tc.got, tc.want)
		}
	}
}

// TestObservabilitySpec pins the hand-over the engine's Run initializes
// from: the fixed service name, and the resolved OTLP endpoint carried
// as-is (the engine omits an empty one, which keeps the local exporters).
func TestObservabilitySpec(t *testing.T) {
	if got := observabilitySpec(ServerConfig{}); got.ServiceName != "reference-app" || got.OTLPEndpoint != "" {
		t.Fatalf("observabilitySpec with no endpoint = %+v, want the service name and an empty endpoint", got)
	}
	const endpoint = "collector.example.internal:4317"
	if got := observabilitySpec(ServerConfig{OTLPEndpoint: endpoint}); got.OTLPEndpoint != endpoint {
		t.Fatalf("observabilitySpec endpoint = %q, want %q", got.OTLPEndpoint, endpoint)
	}
}

// TestHTTPSpec_CarriesTheComposeFaceAndSPA pins the HTTP hand-over: the
// protected-face composition is always declared, the listen address is the
// resolved port, and the SPA appears exactly when the deployment serves a
// frontend directory.
func TestHTTPSpec_CarriesTheComposeFaceAndSPA(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	spec := b.httpSpec()
	if spec.Compose == nil {
		t.Fatal("httpSpec left Compose nil; the app's protected face is composed by chain.Standard in composeFace")
	}
	if spec.Addr != ":8080" {
		t.Fatalf("httpSpec Addr = %q, want the resolved port", spec.Addr)
	}
	if spec.SPA != nil {
		t.Fatalf("httpSpec declared an SPA with no WebDistDir: %+v", spec.SPA)
	}

	b = newServerBuild(ServerConfig{Port: "9090", WebDistDir: "/srv/dist"})
	spec = b.httpSpec()
	if spec.SPA == nil || spec.SPA.Dir != "/srv/dist" {
		t.Fatalf("httpSpec SPA = %+v, want the configured directory", spec.SPA)
	}
	if len(spec.SPA.Options) == 0 {
		t.Fatal("httpSpec SPA carries no options; the server-owned paths must be declared or the frontend answers them from disk")
	}
}

// TestOptions_AssembleTheDeclaredSet pins that one option-building call
// yields the whole declared set -- the configuration targets, the database
// spec, the registrations, the module set, the kernel options and the HTTP
// face -- so a mapping step can never silently drop out of the assembly.
func TestOptions_AssembleTheDeclaredSet(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	if opts := b.options(); len(opts) < 8 {
		t.Fatalf("options() returned %d options, want the full declared set", len(opts))
	}
	// The bus is built as a side effect of the mapping; the close wrapper
	// owns it on a failed assembly, so it must exist by now.
	if b.bus == nil {
		t.Fatal("options() left the audit bus unconstructed")
	}
}
