package i18n

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/BurntSushi/toml"

	"github.com/vislake/speed/go/pkgcore/locales"
)

// The notes module fixture: one plain message, one parameterized message and
// one plural message, in both locales. zh-CN carries only the other form of
// the plural message -- its sole CLDR category -- while en-US defines one
// and other.
const (
	notesZH = `# Notes module messages, Simplified Chinese.
"notes.text_required" = "备注内容不能为空。"
"notes.greeting" = "你好,{{.Name}}!"
["notes.over_quota"]
description = "工作区超出备注配额时发送。"
other = "备注配额已超限,请升级。"
`
	notesEN = `# Notes module messages, American English.
"notes.text_required" = "Note text must not be empty."
"notes.greeting" = "Hello, {{.Name}}!"
["notes.over_quota"]
description = "Sent when a workspace exceeds its note quota."
one = "{{.Count}} note remains."
other = "{{.Count}} notes remain."
`

	billingZH = `# Billing module messages, Simplified Chinese.
"billing.quota_exceeded" = "您的{{.Plan}}配额已超限。"
["billing.credits_low"]
other = "剩余积分不足,请充值。"
`
	billingEN = `# Billing module messages, American English.
"billing.quota_exceeded" = "Your {{.Plan}} quota is exceeded."
["billing.credits_low"]
one = "{{.Count}} credit remains."
other = "Only {{.Count}} credits remain."
`

	// The same fixtures in German, added when a third language joins the
	// catalog -- the point of the file-driven mechanism is that joining it
	// is exactly this: one more file per message-shipping module, nothing
	// else. German follows the same one/other CLDR plural rules as English,
	// so its plural messages carry both forms.
	notesDE = `# Notes module messages, German.
"notes.text_required" = "Notiztext darf nicht leer sein."
"notes.greeting" = "Hallo, {{.Name}}!"
["notes.over_quota"]
description = "Gesendet, wenn ein Workspace sein Notizkontingent überschreitet."
one = "{{.Count}} Notiz verbleibt."
other = "{{.Count}} Notizen verbleiben."
`
	billingDE = `# Billing module messages, German.
"billing.quota_exceeded" = "Ihr {{.Plan}}-Kontingent ist überschritten."
["billing.credits_low"]
one = "{{.Count}} Guthaben verbleibt."
other = "Nur noch {{.Count}} Guthaben verbleiben."
`
)

// localeFS builds an embed-style fs.FS from a file-name to body map, like a
// module's Locales() embed.FS with its locale files flat at the root.
func localeFS(files map[string]string) fstest.MapFS {
	fsys := make(fstest.MapFS, len(files))
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// addPair adds one module with the given zh-CN.toml and en-US.toml bodies to b.
func addPair(t *testing.T, b *Builder, module, zh, en string) {
	t.Helper()
	if err := b.AddModule(module, localeFS(map[string]string{
		"zh-CN.toml": zh,
		"en-US.toml": en,
	})); err != nil {
		t.Fatalf("AddModule(%q) = %v", module, err)
	}
}

// addTrio adds one module with zh-CN.toml, en-US.toml and de-DE.toml bodies
// to b, the shape a module takes once a third language joined the catalog.
func addTrio(t *testing.T, b *Builder, module, zh, en, de string) {
	t.Helper()
	if err := b.AddModule(module, localeFS(map[string]string{
		"zh-CN.toml": zh,
		"en-US.toml": en,
		"de-DE.toml": de,
	})); err != nil {
		t.Fatalf("AddModule(%q) = %v", module, err)
	}
}

// notesCatalog builds a catalog holding exactly the notes fixture module.
func notesCatalog(t *testing.T) *Catalog {
	t.Helper()
	b := NewBuilder()
	addPair(t, b, "notes", notesZH, notesEN)
	return b.Build()
}

// wantError asserts that err is, or wraps, sentinel and that its text carries
// fragment (when fragment is non-empty), so sentinel class and actionable
// detail are both pinned down.
func wantError(t *testing.T, err error, sentinel error, fragment string) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, %v) = false; err = %v", sentinel, err)
	}
	if fragment != "" && !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error text %q does not contain %q", err.Error(), fragment)
	}
}

func TestLookupRendersMessagesInBothLocales(t *testing.T) {
	c := notesCatalog(t)
	got, err := c.Lookup(LocaleENUS, "notes.text_required", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Note text must not be empty." {
		t.Errorf("en-US text_required = %q", got)
	}
	got, err = c.Lookup(LocaleZHCN, "notes.text_required", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "备注内容不能为空。" {
		t.Errorf("zh-CN text_required = %q", got)
	}
}

func TestLookupInterpolatesParams(t *testing.T) {
	c := notesCatalog(t)
	got, err := c.Lookup(LocaleENUS, "notes.greeting", map[string]any{"Name": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello, Ada!" {
		t.Errorf("en-US greeting = %q", got)
	}
	got, err = c.Lookup(LocaleZHCN, "notes.greeting", map[string]any{"Name": "艾达"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "你好,艾达!" {
		t.Errorf("zh-CN greeting = %q", got)
	}
}

func TestLookupMissesAParameterAsNoValue(t *testing.T) {
	c := notesCatalog(t)
	got, err := c.Lookup(LocaleENUS, "notes.greeting", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello, <no value>!" {
		t.Errorf("greeting without Name = %q", got)
	}
}

func TestLookupPluralSelectsCategoryPerLocale(t *testing.T) {
	c := notesCatalog(t)
	one, err := c.LookupPlural(LocaleENUS, "notes.over_quota", 1, map[string]any{"Count": 1})
	if err != nil {
		t.Fatal(err)
	}
	if one != "1 note remains." {
		t.Errorf("en-US count=1 = %q", one)
	}
	many, err := c.LookupPlural(LocaleENUS, "notes.over_quota", 5, map[string]any{"Count": 5})
	if err != nil {
		t.Fatal(err)
	}
	if many != "5 notes remain." {
		t.Errorf("en-US count=5 = %q", many)
	}
	// zh-CN has a single CLDR plural category ("other"), so every count
	// renders the other form.
	zhOne, err := c.LookupPlural(LocaleZHCN, "notes.over_quota", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	zhMany, err := c.LookupPlural(LocaleZHCN, "notes.over_quota", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if zhOne != "备注配额已超限,请升级。" || zhMany != zhOne {
		t.Errorf("zh-CN count=1 %q, count=5 %q; both must be the other form", zhOne, zhMany)
	}
}

func TestLookupOnPluralMessageRendersOtherForm(t *testing.T) {
	c := notesCatalog(t)
	// Lookup carries no count, so a plural message renders its other form,
	// like go-i18n renders a plural message without a PluralCount. The count
	// still reaches the template through params, which is why the params map
	// makes the other form visible in the rendered text.
	got, err := c.Lookup(LocaleENUS, "notes.over_quota", map[string]any{"Count": 9})
	if err != nil {
		t.Fatal(err)
	}
	if got != "9 notes remain." {
		t.Errorf("Lookup on plural = %q, want the other form", got)
	}
}

func TestPluralMessageInlineTableForm(t *testing.T) {
	// The plural message may equally be written as an inline table; the id is
	// the quoted key either way.
	zh := `"notes.x" = { other = "中文。", description = "d" }` + "\n"
	en := `"notes.x" = { one = "{{.Count}} item.", other = "{{.Count}} items." }` + "\n"
	c := mustBuild(t, "notes", zh, en)
	got, err := c.LookupPlural(LocaleENUS, "notes.x", 2, map[string]any{"Count": 2})
	if err != nil {
		t.Fatal(err)
	}
	if got != "2 items." {
		t.Errorf("inline plural count=2 = %q", got)
	}
}

func TestLocalesReturnsSortedSupportedLocales(t *testing.T) {
	c := notesCatalog(t)
	if len(c.Locales()) != 2 || c.Locales()[0] != LocaleENUS || c.Locales()[1] != LocaleZHCN {
		t.Errorf("Locales() = %v, want [%s %s]", c.Locales(), LocaleENUS, LocaleZHCN)
	}
}

// mustBuild adds one module pair and builds the catalog, failing the test on
// any error.
func mustBuild(t *testing.T, module, zh, en string) *Catalog {
	t.Helper()
	b := NewBuilder()
	addPair(t, b, module, zh, en)
	return b.Build()
}

func TestAddModuleMergesModulesIntoOneCatalog(t *testing.T) {
	b := NewBuilder()
	addPair(t, b, "notes", notesZH, notesEN)
	addPair(t, b, "billing", billingZH, billingEN)
	c := b.Build()

	if got, err := c.Lookup(LocaleENUS, "notes.text_required", nil); err != nil || got == "" {
		t.Fatalf("notes id after merge: %q, %v", got, err)
	}
	got, err := c.Lookup(LocaleZHCN, "billing.quota_exceeded", map[string]any{"Plan": "专业版"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "您的专业版配额已超限。" {
		t.Errorf("billing zh = %q", got)
	}
	if got, err := c.LookupPlural(LocaleENUS, "billing.credits_low", 3, map[string]any{"Count": 3}); err != nil || got != "Only 3 credits remain." {
		t.Errorf("billing plural = %q, %v", got, err)
	}
}

func TestEmptyLocaleFSContributesNothing(t *testing.T) {
	b := NewBuilder()
	if err := b.AddModule("notes", localeFS(nil)); err != nil {
		t.Fatalf("AddModule with empty FS = %v", err)
	}
	c := b.Build()
	if got := c.Locales(); len(got) != 0 {
		t.Errorf("Locales() = %v, want none", got)
	}
	_, err := c.Lookup(LocaleENUS, "notes.text_required", nil)
	wantError(t, err, ErrUnknownLocale, "en-US")
}

func TestBuildWithNoModulesIsAnEmptyCatalog(t *testing.T) {
	c := NewBuilder().Build()
	if got := c.Locales(); len(got) != 0 {
		t.Errorf("Locales() = %v, want none", got)
	}
	_, err := c.Lookup(LocaleZHCN, "anything", nil)
	wantError(t, err, ErrUnknownLocale, "")
}

func TestAddModuleIgnoresNonTomlFiles(t *testing.T) {
	// An embed.FS may carry a README or other noise next to the locale files;
	// only the .toml files participate in the contract.
	fsys := localeFS(map[string]string{
		"zh-CN.toml": `"notes.x" = "中文。"` + "\n",
		"en-US.toml": `"notes.x" = "text"` + "\n",
		"README.md":  "not a locale file",
	})
	b := NewBuilder()
	if err := b.AddModule("notes", fsys); err != nil {
		t.Fatalf("AddModule with extra non-toml file = %v", err)
	}
	if _, err := b.Build().Lookup(LocaleENUS, "notes.x", nil); err != nil {
		t.Fatal(err)
	}
}

func TestAddModuleRejectsEmptyModuleName(t *testing.T) {
	b := NewBuilder()
	err := b.AddModule("", localeFS(map[string]string{
		"zh-CN.toml": `"notes.x" = "x"` + "\n",
		"en-US.toml": `"notes.x" = "x"` + "\n",
	}))
	wantError(t, err, ErrEmptyModuleName, "")
}

func TestAddModuleRejectsDottedModuleName(t *testing.T) {
	// A module name is the "<module>." prefix of every message id the
	// module ships, and go-i18n silently overwrites a message whose id is
	// added twice. A dotted name would nest one module's prefix inside
	// another's -- "my" and "my.module" could both own
	// "my.module.user_invite" -- so AddModule must refuse it up front,
	// which is what keeps every module's id space disjoint by construction.
	b := NewBuilder()
	err := b.AddModule("notes.api", localeFS(map[string]string{
		"zh-CN.toml": `"notes.api.x" = "中文。"` + "\n",
		"en-US.toml": `"notes.api.x" = "text"` + "\n",
	}))
	wantError(t, err, ErrInvalidModuleName, `"notes.api"`)
	// The rejection happens before anything merges: a dot-free name is
	// still addable afterwards, and the dotted one leaves no trace.
	addPair(t, b, "notes", notesZH, notesEN)
	c := b.Build()
	if got, err := c.Lookup(LocaleENUS, "notes.text_required", nil); err != nil || got == "" {
		t.Errorf("notes id after rejected dotted AddModule: %q, %v", got, err)
	}
}

func TestAddModuleRejectsDuplicateModule(t *testing.T) {
	b := NewBuilder()
	addPair(t, b, "notes", notesZH, notesEN)
	err := b.AddModule("notes", localeFS(map[string]string{
		"zh-CN.toml": `"notes.x" = "中文。"` + "\n",
		"en-US.toml": `"notes.x" = "text"` + "\n",
	}))
	wantError(t, err, ErrDuplicateModule, `"notes"`)
}

func TestAddModuleRejectsASecondEmptyRegistration(t *testing.T) {
	// Even a module that contributed nothing the first time cannot be added
	// again: a duplicated registration is a bug, not a merge, no-op or not.
	b := NewBuilder()
	if err := b.AddModule("notes", localeFS(nil)); err != nil {
		t.Fatal(err)
	}
	err := b.AddModule("notes", localeFS(nil))
	wantError(t, err, ErrDuplicateModule, "")
}

func TestAddModuleRejectsMissingLocaleFile(t *testing.T) {
	// A lone zh-CN.toml is a legal first module -- it fixes the catalog's
	// language set to what it ships -- so a missing file only means a
	// module next to a fuller catalog: once the notes module established
	// the zh-CN/en-US set, a billing module shipping only zh-CN.toml
	// leaves en-US uncovered.
	b := NewBuilder()
	addPair(t, b, "notes", notesZH, notesEN)
	err := b.AddModule("billing", localeFS(map[string]string{
		"zh-CN.toml": billingZH,
	}))
	wantError(t, err, ErrMissingLocaleFile, "en-US.toml")
}

func TestAddModuleRejectsNonLocaleFileNames(t *testing.T) {
	// A file name is a language, so it must be the canonical spelling of a
	// BCP 47 tag: a README smuggled in as README.toml, an underscore
	// spelling, a lowercase spelling -- none of these is a language, and a
	// file that is not one must fail loudly instead of loading as whatever
	// language the tag parser would guess ("fr.toml", by contrast, is a
	// legal file name once French is a language the catalog serves).
	tests := []struct{ name, wantErr string }{
		{"README.toml", "README.toml"},
		{"zh_CN.toml", "zh_CN.toml"},
		{"zh-cn.toml", "zh-cn.toml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder()
			err := b.AddModule("notes", localeFS(map[string]string{
				"zh-CN.toml": `"notes.x" = "中文。"` + "\n",
				"en-US.toml": `"notes.x" = "text"` + "\n",
				tc.name:      `"notes.x" = "x"` + "\n",
			}))
			wantError(t, err, ErrUnsupportedLocale, tc.wantErr)
		})
	}
}

func TestThirdLanguageNeedsOnlyOneFilePerModule(t *testing.T) {
	// A language joins the catalog as files, never as code: once the notes
	// module ships a de-DE.toml next to its existing pair, the billing
	// module needs only its own de-DE.toml to serve German too. Nothing in
	// the Builder or the Catalog knows about a language list.
	b := NewBuilder()
	addTrio(t, b, "notes", notesZH, notesEN, notesDE)
	addTrio(t, b, "billing", billingZH, billingEN, billingDE)
	c := b.Build()

	want := []string{"de-DE", "en-US", "zh-CN"}
	if got := c.Locales(); !slices.Equal(got, want) {
		t.Fatalf("Locales() = %v, want %v", got, want)
	}
	got, err := c.Lookup("de-DE", "notes.text_required", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Notiztext darf nicht leer sein." {
		t.Errorf("de-DE text_required = %q", got)
	}
	got, err = c.Lookup("de-DE", "billing.quota_exceeded", map[string]any{"Plan": "Pro"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Ihr Pro-Kontingent ist überschritten." {
		t.Errorf("de-DE quota_exceeded = %q", got)
	}
	// German follows the same one/other CLDR plural rules as English, so a
	// count of 1 takes the one form and every other count the other form.
	one, err := c.LookupPlural("de-DE", "notes.over_quota", 1, map[string]any{"Count": 1})
	if err != nil {
		t.Fatal(err)
	}
	if one != "1 Notiz verbleibt." {
		t.Errorf("de-DE count=1 = %q", one)
	}
	many, err := c.LookupPlural("de-DE", "notes.over_quota", 5, map[string]any{"Count": 5})
	if err != nil {
		t.Fatal(err)
	}
	if many != "5 Notizen verbleiben." {
		t.Errorf("de-DE count=5 = %q", many)
	}
	// The original pair still renders exactly as before the third language
	// joined.
	if got, err := c.Lookup(LocaleZHCN, "notes.text_required", nil); err != nil || got != "备注内容不能为空。" {
		t.Errorf("zh-CN after de-DE joined = %q, %v", got, err)
	}
}

func TestAddModuleRejectsASecondModuleOffTheCatalogLanguageSet(t *testing.T) {
	// The first module that ships files fixes the catalog's language set; a
	// later module must match that set exactly. Both directions are
	// covered: a module shipping fewer files than the catalog has
	// languages, and one shipping more.
	t.Run("missing a catalog language", func(t *testing.T) {
		b := NewBuilder()
		addTrio(t, b, "notes", notesZH, notesEN, notesDE)
		err := b.AddModule("billing", localeFS(map[string]string{
			"zh-CN.toml": billingZH,
			"en-US.toml": billingEN,
		}))
		wantError(t, err, ErrMissingLocaleFile, "de-DE.toml")
	})
	t.Run("an extra language", func(t *testing.T) {
		b := NewBuilder()
		addPair(t, b, "notes", notesZH, notesEN)
		err := b.AddModule("billing", localeFS(map[string]string{
			"zh-CN.toml": billingZH,
			"en-US.toml": billingEN,
			"de-DE.toml": billingDE,
		}))
		wantError(t, err, ErrUnsupportedLocale, "de-DE.toml")
	})
}

func TestCatalogLanguageSetIsFixedByItsFirstFileShippingModule(t *testing.T) {
	// The catalog has no language list outside the files, so the first
	// module that ships any declares the whole set: a module shipping only
	// zh-CN.toml builds a single-language catalog -- a legitimate consumer
	// serving one market -- and every other locale is unknown to it, as any
	// unshipped language is.
	b := NewBuilder()
	if err := b.AddModule("notes", localeFS(map[string]string{
		"zh-CN.toml": notesZH,
	})); err != nil {
		t.Fatalf("AddModule(notes, zh-CN only) = %v", err)
	}
	c := b.Build()
	if got := c.Locales(); len(got) != 1 || got[0] != LocaleZHCN {
		t.Fatalf("Locales() = %v, want [%s]", got, LocaleZHCN)
	}
	if got, err := c.Lookup(LocaleZHCN, "notes.text_required", nil); err != nil || got == "" {
		t.Errorf("zh-CN lookup on the single-language catalog = %q, %v", got, err)
	}
	if _, err := c.Lookup(LocaleENUS, "notes.text_required", nil); !errors.Is(err, ErrUnknownLocale) {
		t.Errorf("en-US lookup on the single-language catalog = %v, want ErrUnknownLocale", err)
	}
}

func TestAddModuleEnforcesParityAcrossEveryLocale(t *testing.T) {
	// Parity is N-way, not a zh-CN/en-US property: a third file that drifts
	// from the pair fails the module just the same, naming the language
	// whose file lacks the id.
	deMissingGreeting := strings.Replace(notesDE, `"notes.greeting" = "Hallo, {{.Name}}!"`+"\n", "", 1)
	b := NewBuilder()
	err := b.AddModule("notes", localeFS(map[string]string{
		"zh-CN.toml": notesZH,
		"en-US.toml": notesEN,
		"de-DE.toml": deMissingGreeting,
	}))
	wantError(t, err, ErrParityMismatch, "de-DE lacks ids that en-US has: notes.greeting")
}

func TestAddModuleRejectsMalformedLocaleFiles(t *testing.T) {
	// A parseable, well-formed en-US.toml is used throughout; each case
	// breaks zh-CN.toml in exactly one documented way. Whatever survives
	// parsing would still fail later checks (parity, prefix), so each case
	// must fail with ErrUnsupportedShape carrying the fragment in wantErr.
	en := `"notes.x" = "text"` + "\n"
	tests := []struct {
		name    string
		zh      string
		wantErr string
	}{
		{
			name:    "grouping section under a module-prefixed header",
			zh:      "[\"notes.errors\"]\n\"notes.text_required\" = \"...\"\n",
			wantErr: "grouping sections",
		},
		{
			name:    "bare grouping section like the pre-flat notes shape",
			zh:      "[errors]\n\"notes.text_required\" = \"...\"\n",
			wantErr: "does not start with the module name",
		},
		{
			name:    "id outside the module namespace",
			zh:      "\"other_module.x\" = \"text\"\n",
			wantErr: "does not start with the module name",
		},
		{
			name:    "id equal to the bare module prefix",
			zh:      "\"notes.\" = \"text\"\n",
			wantErr: "does not start with the module name",
		},
		{
			name:    "empty string translation",
			zh:      "\"notes.x\" = \"\"\n",
			wantErr: "empty translation",
		},
		{
			name:    "value that is neither string nor table",
			zh:      "\"notes.x\" = 42\n",
			wantErr: "neither a string nor a table",
		},
		{
			name:    "table id key disagreeing with its own key",
			zh:      "[\"notes.x\"]\nid = \"notes.y\"\n",
			wantErr: `its id key says "notes.y"`,
		},
		{
			name:    "table with only metadata and no translation",
			zh:      "[\"notes.x\"]\ndescription = \"about\"\n",
			wantErr: "table with no translation",
		},
		{
			name:    "nested v1-style table under translation",
			zh:      "[\"notes.x\"]\ntranslation = { other = \"text\" }\n",
			wantErr: "nested tables",
		},
		{
			name:    "empty plural category value",
			zh:      "[\"notes.x\"]\nother = \"\"\n",
			wantErr: "empty translation",
		},
		{
			name:    "plural category with a non-string value",
			zh:      "[\"notes.x\"]\nother = 3\n",
			wantErr: "must be a string",
		},
		{
			name:    "not valid TOML",
			zh:      "\"notes.x\" = \n",
			wantErr: "not valid TOML",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder()
			err := b.AddModule("notes", localeFS(map[string]string{
				"zh-CN.toml": tc.zh,
				"en-US.toml": en,
			}))
			if err == nil {
				t.Fatal("AddModule succeeded, want ErrUnsupportedShape")
			}
			if !errors.Is(err, ErrUnsupportedShape) {
				t.Fatalf("errors.Is(err, ErrUnsupportedShape) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error text %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestAddModuleWrapsTheTomlParseError(t *testing.T) {
	// The parse failure behind ErrUnsupportedShape is wrapped with %w, so a
	// caller can reach the parser's own error through errors.As instead of
	// re-parsing the file to learn why it failed. The error chain must carry
	// the toml.ParseError itself, not just a textual echo of it.
	b := NewBuilder()
	err := b.AddModule("notes", localeFS(map[string]string{
		"zh-CN.toml": "\"notes.x\" = \n",
		"en-US.toml": "\"notes.x\" = \"text\"\n",
	}))
	if err == nil {
		t.Fatal("AddModule succeeded on invalid TOML, want ErrUnsupportedShape")
	}
	if !errors.Is(err, ErrUnsupportedShape) {
		t.Fatalf("errors.Is(err, ErrUnsupportedShape) = false; err = %v", err)
	}
	var parseErr toml.ParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("errors.As(err, &toml.ParseError) = false; err = %v", err)
	}
}

func TestAddModuleRejectsTranslationMixedWithPluralCategories(t *testing.T) {
	// The v1 "translation" key is a synonym for the whole other form: valid
	// alone, never next to a plural category key. parseMessageTable walks
	// the table keys in sorted order, and the rejection must not depend on
	// it -- "zero" and "two" sort after "translation" (a guard keyed off the
	// already-set other form never fires for them), while "one", "few" and
	// "many" sort before it without setting the other form. Every category
	// gets a table mixing it with "translation", plus a mix written with
	// different key casing.
	en := `"notes.x" = "text"` + "\n"
	tests := []struct {
		name    string
		zh      string
		wantErr string
	}{
		{
			name:    "zero, which sorts after translation",
			zh:      "[\"notes.x\"]\ntranslation = \"中文。\"\nzero = \"...\"\n",
			wantErr: `mixes "translation" with a plural category`,
		},
		{
			name:    "two, which sorts after translation",
			zh:      "[\"notes.x\"]\ntranslation = \"中文。\"\ntwo = \"...\"\n",
			wantErr: `mixes "translation" with a plural category`,
		},
		{
			name:    "one, which sorts before translation",
			zh:      "[\"notes.x\"]\ntranslation = \"中文。\"\none = \"...\"\n",
			wantErr: `mixes "translation" with a plural category`,
		},
		{
			name:    "few, which sorts before translation",
			zh:      "[\"notes.x\"]\ntranslation = \"中文。\"\nfew = \"...\"\n",
			wantErr: `mixes "translation" with a plural category`,
		},
		{
			name:    "many, which sorts before translation",
			zh:      "[\"notes.x\"]\ntranslation = \"中文。\"\nmany = \"...\"\n",
			wantErr: `mixes "translation" with a plural category`,
		},
		{
			name:    "other, which fills the same field",
			zh:      "[\"notes.x\"]\ntranslation = \"中文。\"\nother = \"...\"\n",
			wantErr: `mixes "translation" with a plural category`,
		},
		{
			name:    "keys spelled with different case",
			zh:      "[\"notes.x\"]\nTranslation = \"中文。\"\nZero = \"...\"\n",
			wantErr: `mixes "Translation" with a plural category`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder()
			err := b.AddModule("notes", localeFS(map[string]string{
				"zh-CN.toml": tc.zh,
				"en-US.toml": en,
			}))
			wantError(t, err, ErrUnsupportedShape, tc.wantErr)
		})
	}
}

func TestTranslationSynonymRendersSingleFormMessages(t *testing.T) {
	// The v1 synonym keeps loading for the shape it exists for -- a table
	// whose only form is the translation key, metadata keys allowed. Only a
	// mix with plural categories is rejected, so these legitimate tables
	// must keep rendering in both locales.
	zh := "[\"notes.x\"]\ntranslation = \"你好,{{.Name}}!\"\ndescription = \"greeting\"\n"
	en := "[\"notes.x\"]\ntranslation = \"Hello, {{.Name}}!\"\ndescription = \"greeting\"\n"
	c := mustBuild(t, "notes", zh, en)
	got, err := c.Lookup(LocaleENUS, "notes.x", map[string]any{"Name": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello, Ada!" {
		t.Errorf("en-US v1-synonym render = %q", got)
	}
	got, err = c.Lookup(LocaleZHCN, "notes.x", map[string]any{"Name": "艾达"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "你好,艾达!" {
		t.Errorf("zh-CN v1-synonym render = %q", got)
	}
}

func TestAddModuleEnforcesIdParityBetweenLocales(t *testing.T) {
	tests := []struct {
		name string
		zh   string
		en   string
		// wantText is the parity report fragment naming which language owns
		// the id the other lacks, e.g. notes.y only shipped in en-US is
		// reported as "zh-CN lacks ids that en-US has".
		wantText string
	}{
		{
			name:     "en-US carries an extra id",
			zh:       "\"notes.x\" = \"中文。\"\n",
			en:       "\"notes.x\" = \"text\"\n\"notes.y\" = \"text\"\n",
			wantText: "zh-CN lacks ids that en-US has: notes.y",
		},
		{
			name:     "zh-CN carries an extra id",
			zh:       "\"notes.x\" = \"中文。\"\n\"notes.y\" = \"中文。\"\n",
			en:       "\"notes.x\" = \"text\"\n",
			wantText: "en-US lacks ids that zh-CN has: notes.y",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder()
			err := b.AddModule("notes", localeFS(map[string]string{
				"zh-CN.toml": tc.zh,
				"en-US.toml": tc.en,
			}))
			wantError(t, err, ErrParityMismatch, tc.wantText)
		})
	}
}

func TestAddModuleFailureLeavesNothingMerged(t *testing.T) {
	// The second module fails parity, so none of its messages may reach the
	// bundle -- the failing module's merge happens only after every check
	// passed, and an error leaves the catalog exactly as it was before the
	// call. The first module's messages stay, of course.
	b := NewBuilder()
	addPair(t, b, "notes", notesZH, notesEN)
	err := b.AddModule("billing", localeFS(map[string]string{
		"zh-CN.toml": `"billing.x" = "中文。"` + "\n",
		"en-US.toml": `"billing.x" = "text"` + "\n" + `"billing.y" = "extra"` + "\n",
	}))
	wantError(t, err, ErrParityMismatch, "")
	c := b.Build()
	if _, err := c.Lookup(LocaleENUS, "billing.x", nil); !errors.Is(err, ErrUnknownCode) {
		t.Errorf("billing.x lookup after failed AddModule = %v, want ErrUnknownCode", err)
	}
	if _, err := c.Lookup(LocaleZHCN, "notes.text_required", nil); err != nil {
		t.Errorf("notes id lost after a failed sibling AddModule: %v", err)
	}
}

func TestLookupFailsLoudlyOnUnknownLocale(t *testing.T) {
	c := notesCatalog(t)
	_, err := c.Lookup("fr-FR", "notes.text_required", nil)
	wantError(t, err, ErrUnknownLocale, `"fr-FR"`)
	_, err = c.LookupPlural("fr-FR", "notes.over_quota", 1, nil)
	wantError(t, err, ErrUnknownLocale, "")
}

func TestLookupFailsLoudlyOnUnknownCode(t *testing.T) {
	c := notesCatalog(t)
	_, err := c.Lookup(LocaleENUS, "notes.no_such_id", nil)
	wantError(t, err, ErrUnknownCode, `"notes.no_such_id"`)
	_, err = c.LookupPlural(LocaleZHCN, "billing.anything", 1, nil)
	wantError(t, err, ErrUnknownCode, `for locale "zh-CN"`)
}

func TestTemplateSyntaxErrorSurfacesAtRenderTime(t *testing.T) {
	// go-i18n parses message templates when the message is rendered, not when
	// it is added, so a broken template is a loud render error -- never a
	// silently blank string.
	zh := `"notes.x" = "中文。"` + "\n"
	en := `"notes.x" = "Hello {{.Count"` + "\n"
	c := mustBuild(t, "notes", zh, en)
	_, err := c.Lookup(LocaleENUS, "notes.x", nil)
	if err == nil {
		t.Fatal("Lookup succeeded on an unclosed template, want a render error")
	}
	if !strings.Contains(err.Error(), "i18n: render") {
		t.Fatalf("error %q is not wrapped as a render error", err)
	}
}

func TestPluralMessageMissingCategoryFailsAtRenderTime(t *testing.T) {
	// en-US distinguishes one and other; a plural message defining only one
	// renders fine for count=1 but must fail loudly for count=2 rather than
	// inventing text.
	zh := `["notes.x"]` + "\n" + `other = "中文。"` + "\n"
	en := `["notes.x"]` + "\n" + `one = "{{.Count}} item."` + "\n"
	c := mustBuild(t, "notes", zh, en)
	if _, err := c.LookupPlural(LocaleENUS, "notes.x", 1, map[string]any{"Count": 1}); err != nil {
		t.Fatalf("count=1 (one form) = %v", err)
	}
	if _, err := c.LookupPlural(LocaleENUS, "notes.x", 2, map[string]any{"Count": 2}); err == nil {
		t.Fatal("count=2 with no other form succeeded, want a render error")
	}
}

func TestSingleFormMessageMissingCategoryFailsAtRenderTime(t *testing.T) {
	// The mirror case of the test above: a single-form message defines only
	// the other form, so LookupPlural on it must fail loudly for a count
	// that falls into a category it does not define -- count 1 in en-US is
	// "one" -- while counts that fall into other render its text, and in
	// zh-CN, whose sole category is other, every count renders. Single-form
	// messages are rendered with Lookup, which carries no count.
	c := notesCatalog(t) // notes.text_required is single-form in both locales
	if _, err := c.LookupPlural(LocaleENUS, "notes.text_required", 1, nil); err == nil {
		t.Fatal("LookupPlural count=1 on a single-form message succeeded, want a render error")
	}
	for _, count := range []int64{2, 0} {
		got, err := c.LookupPlural(LocaleENUS, "notes.text_required", count, nil)
		if err != nil {
			t.Fatalf("LookupPlural count=%d (other form) = %v", count, err)
		}
		if got != "Note text must not be empty." {
			t.Errorf("LookupPlural count=%d = %q", count, got)
		}
	}
	zh, err := c.LookupPlural(LocaleZHCN, "notes.text_required", 1, nil)
	if err != nil {
		t.Fatalf("zh-CN LookupPlural count=1 = %v (zh-CN's sole category is other)", err)
	}
	if zh != "备注内容不能为空。" {
		t.Errorf("zh-CN LookupPlural count=1 = %q", zh)
	}
}

func TestCatalogConcurrentLookups(t *testing.T) {
	// A built Catalog is immutable and lock-free; run this under -race to
	// prove it. Mixed Lookup and LookupPlural traffic across both locales.
	b := NewBuilder()
	addPair(t, b, "notes", notesZH, notesEN)
	addPair(t, b, "billing", billingZH, billingEN)
	c := b.Build()

	const goroutines, iterations = 16, 500
	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if _, err := c.Lookup(LocaleENUS, "notes.greeting", map[string]any{"Name": "Ada"}); err != nil {
					errCh <- err
					return
				}
				if _, err := c.Lookup(LocaleZHCN, "notes.text_required", nil); err != nil {
					errCh <- err
					return
				}
				if _, err := c.LookupPlural(LocaleENUS, "notes.over_quota", int64(g+i), map[string]any{"Count": g + i}); err != nil {
					errCh <- err
					return
				}
				if _, err := c.LookupPlural(LocaleZHCN, "billing.credits_low", int64(i), nil); err != nil {
					errCh <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestRealPkgcoreSeedFiles(t *testing.T) {
	// The honest end-to-end path: pkgcore's own shipped seed bundle, loaded
	// through the same fs.FS seam a module's Locales() embed.FS uses.
	b := NewBuilder()
	if err := b.AddModule("pkgcore", locales.FS); err != nil {
		t.Fatalf("AddModule(pkgcore, locales.FS) = %v", err)
	}
	c := b.Build()
	got, err := c.Lookup(LocaleENUS, "pkgcore.seed.params", map[string]any{"Name": "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "seed") {
		t.Errorf("en-US seed.params = %q", got)
	}
	zh, err := c.Lookup(LocaleZHCN, "pkgcore.seed.params", map[string]any{"Name": "种子"})
	if err != nil {
		t.Fatal(err)
	}
	if zh == got {
		t.Errorf("zh-CN seed.params %q must differ from the en-US text", zh)
	}
	for _, count := range []int64{1, 5} {
		if _, err := c.LookupPlural(LocaleZHCN, "pkgcore.seed.plural", count, map[string]any{"Count": count}); err != nil {
			t.Fatalf("zh-CN seed.plural count=%d: %v", count, err)
		}
	}
	if _, err := c.LookupPlural(LocaleENUS, "pkgcore.seed.plural", 1, map[string]any{"Count": 1}); err != nil {
		t.Fatalf("en-US seed.plural count=1: %v", err)
	}
	if _, err := c.LookupPlural(LocaleENUS, "pkgcore.seed.plural", 5, map[string]any{"Count": 5}); err != nil {
		t.Fatalf("en-US seed.plural count=5: %v", err)
	}
}

func TestRealPkgcoreSeedIdsResolveInEveryLocale(t *testing.T) {
	// Pins the shipped seed id set on top of the file-level CI check: every
	// id the bundle declares must resolve through the runtime catalog in both
	// locales, so an accidental divergence between the two files is caught by
	// more than one gate.
	b := NewBuilder()
	if err := b.AddModule("pkgcore", locales.FS); err != nil {
		t.Fatalf("AddModule(pkgcore, locales.FS) = %v", err)
	}
	c := b.Build()
	for _, code := range []string{"pkgcore.seed.plain", "pkgcore.seed.params", "pkgcore.seed.plural"} {
		for _, locale := range c.Locales() {
			if _, err := c.Lookup(locale, code, nil); err != nil {
				t.Errorf("Lookup(%s, %s) = %v", locale, code, err)
			}
		}
	}
}
