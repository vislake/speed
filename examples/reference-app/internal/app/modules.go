package app

// This file holds the host's module wiring helpers: the adapters and
// registrar calls the host's own components (host_wiring.go) use to build
// every module over this app's database, ciphers and key material.

import (
	"context"
	"fmt"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
)

// OrgSubtreeResolverFor adapts org.Scope's Path method onto
// rbac.SubtreeResolver's NodePath -- the two no-import seams differ just
// enough (three return values vs. two; "not found" folded into an error vs.
// a plain boolean) that an adapter is needed, unlike the feature-gate
// seams' exact structural match (org.FeatureGateFunc over the config
// module's Handle). It returns the
// closure wrapped in rbac.SubtreeResolverFunc, so the wiring site passes
// the option a value it already accepts. org.Scope is safe to capture
// directly (unlike *config.Service in the readers below): orgModule.Scope()
// is available the moment org.NewModule returns, with no Attach-ordering
// constraint.
func OrgSubtreeResolverFor(scope org.Scope) rbac.SubtreeResolverFunc {
	return func(ctx context.Context, nodeID string) (string, bool, error) {
		path, err := scope.Path(ctx, nodeID)
		if err != nil {
			if apperr.HasCode(err, org.ErrNodeNotFound.Code) {
				// rbac's own contract: an unresolvable node DENIES the binding
				// that named it rather than erroring, so the caller's Can/
				// DataScope decision reports "no such node" as ok == false,
				// never widening to the tenant (SubtreeResolver's own doc
				// comment).
				return "", false, nil
			}
			return "", false, err
		}
		return path, true, nil
	}
}

// registerModuleSerializers binds the four module-owned encrypted columns
// through each module's own registrar -- the module knows the GORM
// serializer name its schema expects, so the name never crosses this
// boundary as a hand-typed string. Because the shared cipher is the only
// input, evaluating all four before reporting the first failure is
// equivalent to failing fast on it.
func registerModuleSerializers(cipher *dbkit.Cipher) error {
	for _, registration := range []struct {
		what string
		err  error
	}{
		{"org email serializer", org.RegisterEmailSerializer(cipher)},
		{"notification contact-address serializer", notification.RegisterContactAddressSerializer(cipher)},
		{"ai-gateway credential serializer", aigateway.RegisterCredentialAPIKeySerializer(cipher)},
		{"integration webhook-secret serializer", integration.RegisterWebhookSecretSerializer(cipher)},
	} {
		if registration.err != nil {
			return fmt.Errorf("reference-app: register the %s: %w", registration.what, registration.err)
		}
	}
	return nil
}

// buildModuleIndexers builds the blind indexers the org and notification
// modules query their encrypted columns through, each through the module's
// own constructor: the module owns its index column and canonical form, so
// neither crosses this boundary as a hand-typed string. org's invitation
// addresses and notification's verified contacts are made queryable by
// SEPARATE HMAC keys -- reusing cfg.Config.Cipher_Key for both would be
// exactly the AES-key-doubling-as-an-HMAC-key weakness dbkit warns against.
// One key serves notification's email and phone indexers alike (authn's
// single blind-index key precedent).
func buildModuleIndexers(cfg ServerConfig) (*dbkit.BlindIndexer, *dbkit.BlindIndexer, *dbkit.BlindIndexer, error) {
	orgIndexer, err := org.NewEmailIndexer(cfg.Org.Invitation_Email_Index_Key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build the org email indexer: %w", err)
	}
	contactEmailIndexer, err := notification.NewContactEmailIndexer(cfg.Notification.Contact_Index_Key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact email indexer: %w", err)
	}
	contactPhoneIndexer, err := notification.NewContactPhoneIndexer(cfg.Notification.Contact_Index_Key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reference-app: build the notification contact phone indexer: %w", err)
	}
	return orgIndexer, contactEmailIndexer, contactPhoneIndexer, nil
}
