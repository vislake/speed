package main

// config_example.go carries the target struct the committed
// docs/config.example.yaml loads against, and the load-verification unit
// test in configrefgen_test.go drives the real pkgcore/config loader over
// the real committed file. The struct is a loader target in the loader's
// own SPEED_ spelling family, mirroring what docs/config.example.yaml
// holds: the six platform keys a host must feed.
//
// The struct's fields are deliberately loader-shaped: exported fields map
// to dotted keys lowercased, a nested struct contributes its field names as
// deeper segments, a `config:"required"` tag makes a key mandatory, and the
// defaults assigned here are the lowest-priority source. Keys map to env
// variables as SPEED_<UPPERCASED KEY WITH __ FOR DOTS> and to flags as
// --key.

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/vislake/speed/go/pkgcore/config"
)

// configExampleYAMLPath is the hand-written YAML example the JSON counterpart
// is derived from; configExampleJSONPath is the derived artifact, committed
// beside it.
const (
	configExampleYAMLPath = "docs/config.example.yaml"
	configExampleJSONPath = "docs/config.example.json"
)

// configExampleKeyPlaceholder is the stand-in every platform key in
// docs/config.example.yaml carries: a recognizable literal in place of real
// key material, never a key. The load-verification test asserts each of the
// file's six platform keys parses to exactly this value.
const configExampleKeyPlaceholder = "REPLACE_WITH_64_HEX_CHARS"

// configExampleStructDefault is the lowest-priority source's stand-in: the
// value the target struct carries before any source supplies the key. The
// load-verification test asserts the file outranks it for every platform
// key, and that with no file at all the defaults stand untouched.
const configExampleStructDefault = "STRUCT_DEFAULT_NOT_KEY_MATERIAL"

// deriveConfigExampleJSON renders the JSON counterpart of the committed
// docs/config.example.yaml: the loader accepts YAML or JSON for the same key
// set, so the two committed examples demonstrate one format each, and
// deriving the JSON from the YAML means the pair cannot disagree about a key
// or a value -- a hand-written twin could, and nothing would catch it. The
// derivation runs the same YAML parser the loader's file source runs, so what
// it reads is what the loader reads; koanf's own JSON marshalling sorts the
// keys, keeping the output byte-identical across runs.
func deriveConfigExampleJSON(root string) (string, error) {
	path := filepath.Join(root, configExampleYAMLPath)
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return "", fmt.Errorf("derive %s from %s: %w", configExampleJSONPath, configExampleYAMLPath, err)
	}
	out, err := json.MarshalIndent(k.Raw(), "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", configExampleJSONPath, err)
	}
	return string(out) + "\n", nil
}

// exampleBootstrapConfig is the loader target docs/config.example.yaml is
// verified against: the six platform key materials the file's key set feeds.
// Each inner field name lowercases to exactly the last segment of a declared
// key path, so the nested field maps to the dotted key (Authn.Blind_Index_Key
// is authn.blind_index_key -- env SPEED_AUTHN__BLIND_INDEX_KEY, flag
// --authn.blind_index_key). A segment that carries an underscore is spelled
// with it: the loader lowercases the field name verbatim into the key path,
// so it is the only spelling that reaches the key, and each such field
// carries a staticcheck exception stating exactly that.
type exampleBootstrapConfig struct {
	Authn        exampleKeyAuthn
	Config       exampleKeyConfig
	Notification exampleKeyNotification
	Org          exampleKeyOrg
	Pki          exampleKeyPki
}

type exampleKeyConfig struct {
	//nolint:staticcheck // the field name must lowercase to the dotted key config.cipher_key that docs/config.example.yaml spells.
	Cipher_Key string
}

type exampleKeyAuthn struct {
	//nolint:staticcheck // the field name must lowercase to the dotted key authn.blind_index_key that docs/config.example.yaml spells.
	Blind_Index_Key string
	//nolint:staticcheck // the field name must lowercase to the dotted key authn.pii_cipher_key that docs/config.example.yaml spells.
	PII_Cipher_Key string
}

type exampleKeyNotification struct {
	//nolint:staticcheck // the field name must lowercase to the dotted key notification.contact_index_key that docs/config.example.yaml spells.
	Contact_Index_Key string
}

type exampleKeyOrg struct {
	//nolint:staticcheck // the field name must lowercase to the dotted key org.invitation_email_index_key that docs/config.example.yaml spells.
	Invitation_Email_Index_Key string
}

type exampleKeyPki struct {
	//nolint:staticcheck // the field name must lowercase to the dotted key pki.local_key_cipher_key that docs/config.example.yaml spells.
	Local_Key_Cipher_Key string
}

// exampleBootstrapDefaults returns the struct with the defaults the loader
// falls back to when no source supplies a key (the lowest-priority source).
func exampleBootstrapDefaults() exampleBootstrapConfig {
	return exampleBootstrapConfig{
		Authn: exampleKeyAuthn{
			Blind_Index_Key: configExampleStructDefault,
			PII_Cipher_Key:  configExampleStructDefault,
		},
		Config:       exampleKeyConfig{Cipher_Key: configExampleStructDefault},
		Notification: exampleKeyNotification{Contact_Index_Key: configExampleStructDefault},
		Org:          exampleKeyOrg{Invitation_Email_Index_Key: configExampleStructDefault},
		Pki:          exampleKeyPki{Local_Key_Cipher_Key: configExampleStructDefault},
	}
}

// configLoaderFor returns a Loader pointed at the docs/config.example.yaml
// file with the given injected args (os.Args[1:] form) and environment
// ("KEY=value" form). The injectable shapes exist so the load-verification
// test can pin the precedence chain without mutating the process
// environment.
func configLoaderFor(path string, args, environ []string) *config.Loader {
	return config.New(
		config.WithConfigFile(path),
		config.WithArgs(args),
		config.WithEnviron(environ),
	)
}

// loadConfigExample loads the committed docs/config.example.yaml through the
// real loader with no environment and no flags, returning the resolved
// target. The file is optional in general (a missing one is skipped
// silently), but the load-verification test has already verified it exists.
func loadConfigExample(path string) (exampleBootstrapConfig, error) {
	cfg := exampleBootstrapDefaults()
	err := configLoaderFor(path, nil, nil).Load(&cfg)
	return cfg, err
}
