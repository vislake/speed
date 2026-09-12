package pkgcore

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// configSchemaFixture exercises the whole declaration vocabulary at once.
type configSchemaFixture struct {
	Host      string `json:"host"`
	TLSMode   string `json:"tls_mode" config:"expose"`
	CipherKey []byte `json:"cipher_key" config:"derive,required,sensitive,group=authn"`
	Port      int    `json:"port" config:"env=APP_PORT"`
	Unused    string `json:"unused" config:"-"`
	JSONSkip  string `json:"-"`
	hidden    string
	Timeout   time.Duration     `json:"timeout"`
	Password  passwordFixture   `json:"password"`
	Labels    map[string]string `json:"labels"`
	Any       any               `json:"any"`
}

// passwordFixture is the nested block the fixture descends into.
type passwordFixture struct {
	Memory uint32 `json:"memory"`
}

// documentedConfigSchemaFixture carries the optional ConfigDocs surface with
// a value receiver -- the shape whose call on the registered typed nil
// pointer would panic without configDocs' routing through a fresh value.
type documentedConfigSchemaFixture struct {
	Host      string `json:"host" config:"expose"`
	CipherKey []byte `json:"cipher_key" config:"derive,sensitive"`
}

func (documentedConfigSchemaFixture) ConfigDocs() map[string]FieldDoc {
	return map[string]FieldDoc{
		// Keyed by the Go field name on purpose: doc lookup must fold to the
		// same key as the json tag spelling.
		"Host":       {Description: "the address the listener binds", Default: "127.0.0.1", Example: "0.0.0.0"},
		"cipher_key": {Description: "32 bytes of key material sealing the fixture's rows"},
	}
}

// pointerDocumentedSchemaFixture implements ConfigDocs on the pointer
// receiver, the other shape a schema target can take.
type pointerDocumentedSchemaFixture struct {
	Endpoints []string `json:"endpoints" config:"expose"`
}

func (*pointerDocumentedSchemaFixture) ConfigDocs() map[string]FieldDoc {
	return map[string]FieldDoc{"endpoints": {Description: "the upstream endpoints"}}
}

func TestAnalyzeConfigSchemaVocabulary(t *testing.T) {
	fixture := configSchemaFixture{hidden: "an unexported field the analyzer must skip"}
	fields, err := analyzeConfigSchema(&fixture)
	if err != nil {
		t.Fatalf("analyzeConfigSchema = %v, want nil", err)
	}

	type want struct {
		key       string
		expose    bool
		derive    bool
		required  bool
		sensitive bool
		env       string
		group     string
	}
	wants := []want{
		{key: "host"},
		{key: "tls_mode", expose: true},
		{key: "cipher_key", expose: true, derive: true, required: true, sensitive: true, group: "authn"},
		{key: "port", env: "APP_PORT"},
		{key: "timeout"},
		{key: "password.memory"},
		{key: "labels"},
		{key: "any"},
	}
	if len(fields) != len(wants) {
		t.Fatalf("analyzed %d fields (%v), want %d", len(fields), fieldKeys(fields), len(wants))
	}
	for i, f := range fields {
		w := wants[i]
		got := want{key: f.key, expose: f.expose, derive: f.derive, required: f.required, sensitive: f.sensitive, env: f.env, group: f.group}
		if got != w {
			t.Errorf("field %d = %+v, want %+v", i, got, w)
		}
	}
}

// fieldKeys renders the analyzed fields' local key paths, for failure text.
func fieldKeys(fields []schemaField) []string {
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, f.key)
	}
	return keys
}

func TestAnalyzeConfigSchemaRejectsMalformedTags(t *testing.T) {
	cases := []struct {
		name   string
		schema any
		want   string
	}{
		{
			name: "unknown option",
			schema: &struct {
				A string `config:"bogus"`
			}{},
			want: "unknown config tag option",
		},
		{
			name: "repeated derive",
			schema: &struct {
				A []byte `config:"derive,derive"`
			}{},
			want: "repeats the",
		},
		{
			name: "derive on a non-byte field",
			schema: &struct {
				A string `config:"derive"`
			}{},
			want: "only a []byte field can hold key material",
		},
		{
			name: "empty env pin",
			schema: &struct {
				A string `config:"env="`
			}{},
			want: "malformed or repeated",
		},
		{
			name: "repeated env pin",
			schema: &struct {
				A string `config:"env=ONE,env=TWO"`
			}{},
			want: "malformed or repeated",
		},
		{
			name: "empty group",
			schema: &struct {
				A string `config:"group="`
			}{},
			want: "malformed or repeated",
		},
		{
			name: "repeated group",
			schema: &struct {
				A string `config:"group=one,group=two"`
			}{},
			want: "malformed or repeated",
		},
		{
			name: "skip beside another option",
			schema: &struct {
				A string `config:"-,required"`
			}{},
			want: "beside other options",
		},
		{
			name: "options on a nested struct",
			schema: &struct {
				A struct {
					B string
				} `config:"expose"`
			}{},
			want: "belong on the fields of",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := analyzeConfigSchema(tc.schema)
			if !errors.Is(err, ErrInvalidComponent) {
				t.Fatalf("analyzeConfigSchema = %v, want ErrInvalidComponent", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestAnalyzeConfigSchemaLeafShape(t *testing.T) {
	type container struct {
		Deep struct {
			Value string `json:"value"`
		} `json:"deep"`
		Pointer *struct {
			Leaf string `json:"leaf"`
		} `json:"pointer"`
		Stamp time.Time `json:"stamp"`
	}
	fields, err := analyzeConfigSchema((*container)(nil))
	if err != nil {
		t.Fatalf("analyzeConfigSchema = %v, want nil", err)
	}
	want := []string{"deep.value", "pointer.leaf", "stamp"}
	if len(fields) != len(want) {
		t.Fatalf("fields = %v, want %v", fieldKeys(fields), want)
	}
	for i, f := range fields {
		if f.key != want[i] {
			t.Errorf("field %d = %q, want %q", i, f.key, want[i])
		}
	}
}

func TestAnalyzeConfigSchemaNilAndShape(t *testing.T) {
	fields, err := analyzeConfigSchema(nil)
	if err != nil || fields != nil {
		t.Fatalf("analyzeConfigSchema(nil) = %v, %v, want no fields and no error", fields, err)
	}
	if _, err := analyzeConfigSchema(struct{}{}); !errors.Is(err, ErrInvalidComponent) {
		t.Fatalf("analyzeConfigSchema(non-pointer) = %v, want ErrInvalidComponent", err)
	}
	if _, err := analyzeConfigSchema(&[]string{}); !errors.Is(err, ErrInvalidComponent) {
		t.Fatalf("analyzeConfigSchema(pointer to slice) = %v, want ErrInvalidComponent", err)
	}
}

func TestConfigKeyPrefix(t *testing.T) {
	cases := []struct {
		name      string
		component string
		namespace string
		want      string
		wantErr   bool
	}{
		{name: "empty namespace resolves at the bare path", component: "authn", namespace: "", want: ""},
		{name: "empty namespace with no component name", component: "", namespace: "", want: ""},
		{name: "custom with dot", component: "authn", namespace: "platform.", want: "platform."},
		{name: "custom without dot", component: "authn", namespace: "platform", want: "platform."},
		{name: "custom multi-segment", component: "authn", namespace: "speed.authn", want: "speed.authn."},
		{name: "leading empty segment", component: "authn", namespace: ".platform", wantErr: true},
		{name: "interior empty segment", component: "authn", namespace: "speed..authn", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := configKeyPrefix(tc.component, tc.namespace)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidComponent) {
					t.Fatalf("configKeyPrefix(%q, %q) = %v, want ErrInvalidComponent", tc.component, tc.namespace, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("configKeyPrefix(%q, %q) = %v, want nil", tc.component, tc.namespace, err)
			}
			if got != tc.want {
				t.Errorf("configKeyPrefix(%q, %q) = %q, want %q", tc.component, tc.namespace, got, tc.want)
			}
		})
	}
}

func TestDescribeComponentSchema(t *testing.T) {
	descriptors, err := DescribeComponentSchema("fixture", (*documentedConfigSchemaFixture)(nil))
	if err != nil {
		t.Fatalf("DescribeComponentSchema = %v, want nil", err)
	}
	if len(descriptors) != 2 {
		t.Fatalf("described %d fields, want 2", len(descriptors))
	}

	host := descriptors[0]
	if host.Key != "host" || host.Type != "string" || !host.Expose || host.Derive || host.Required || host.Sensitive {
		t.Errorf("host descriptor = %+v, want the bare-path exposed string", host)
	}
	if host.Doc.Description != "the address the listener binds" || host.Doc.Default != "127.0.0.1" || host.Doc.Example != "0.0.0.0" {
		t.Errorf("host doc = %+v, want the ConfigDocs entry matched case-insensitively", host.Doc)
	}

	key := descriptors[1]
	if key.Key != "cipher_key" || key.Type != "[]byte" || !key.Derive || !key.Expose || !key.Sensitive {
		t.Errorf("cipher_key descriptor = %+v, want the derive/sensitive key-material shape", key)
	}
	if key.Doc.Description == "" {
		t.Errorf("cipher_key doc = %+v, want the documented description", key.Doc)
	}
}

func TestDescribeComponentSchemaNilPointerDocs(t *testing.T) {
	descriptors, err := DescribeComponentSchema("fixture", (*pointerDocumentedSchemaFixture)(nil))
	if err != nil {
		t.Fatalf("DescribeComponentSchema = %v, want nil", err)
	}
	if len(descriptors) != 1 || descriptors[0].Key != "endpoints" {
		t.Fatalf("descriptors = %+v, want one endpoint field", descriptors)
	}
	if descriptors[0].Doc.Description != "the upstream endpoints" {
		t.Errorf("doc = %+v, want the pointer-receiver ConfigDocs entry", descriptors[0].Doc)
	}

	if descriptors, err := DescribeComponentSchema("fixture", nil); err != nil || len(descriptors) != 0 {
		t.Fatalf("DescribeComponentSchema(nil schema) = %v, %v, want an empty description", descriptors, err)
	}
}

func TestDescribeComponentSchemaFollowsRegisteredNamespace(t *testing.T) {
	custom := plainComponent("testschema.custom", &struct{}{})
	custom.ConfigNamespace = "platform."
	if err := Register(custom); err != nil {
		t.Fatalf("Register(%q) = %v, want nil", custom.Name, err)
	}
	flat := plainComponent("testschema.flat", &struct{}{})
	if err := Register(flat); err != nil {
		t.Fatalf("Register(%q) = %v, want nil", flat.Name, err)
	}

	schema := &struct {
		Token string `json:"token"`
	}{}

	descriptors, err := DescribeComponentSchema("testschema.custom", schema)
	if err != nil {
		t.Fatalf("DescribeComponentSchema = %v, want nil", err)
	}
	if descriptors[0].Key != "platform.token" {
		t.Errorf("custom-namespace key = %q, want %q", descriptors[0].Key, "platform.token")
	}

	descriptors, err = DescribeComponentSchema("testschema.flat", schema)
	if err != nil {
		t.Fatalf("DescribeComponentSchema = %v, want nil", err)
	}
	if descriptors[0].Key != "token" {
		t.Errorf("empty-namespace key = %q, want the bare path %q", descriptors[0].Key, "token")
	}

	descriptors, err = DescribeComponentSchema("testschema.unregistered", schema)
	if err != nil {
		t.Fatalf("DescribeComponentSchema = %v, want nil", err)
	}
	if descriptors[0].Key != "token" {
		t.Errorf("unregistered-name key = %q, want the bare path: no registration declares a namespace", descriptors[0].Key)
	}
}

func TestConfigCarriesKey(t *testing.T) {
	cfg := ComponentConfig{}.
		With("Host", "h").
		With("Password", ComponentConfig{}.With("Memory", 256))
	cases := []struct {
		local string
		want  bool
	}{
		{"host", true},
		{"HOST", true},
		{"password.memory", true},
		{"password.missing", false},
		{"password.memory.deep", false},
		{"host.deeper", false},
		{"absent", false},
	}
	for _, tc := range cases {
		if got := configCarriesKey(cfg, tc.local); got != tc.want {
			t.Errorf("configCarriesKey(%q) = %t, want %t", tc.local, got, tc.want)
		}
	}
}
