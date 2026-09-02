package i18n

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// The locales the M0 catalog ships. They are deliberately the only two:
// docs/internal/11-cross-cutting.md scopes v1.0 to zh-CN and en-US, and
// both the language-negotiation chain (which stores these exact values) and
// the locale files themselves are written against them.
const (
	// LocaleZHCN is Simplified Chinese, the default language of the
	// negotiation chain in docs/internal/11-cross-cutting.md.
	LocaleZHCN = "zh-CN"
	// LocaleENUS is American English.
	LocaleENUS = "en-US"
)

// localeFileNames maps every supported locale to the name of its message
// file inside a module's Locales() embed.FS.
var localeFileNames = map[string]string{
	LocaleZHCN: "zh-CN.toml",
	LocaleENUS: "en-US.toml",
}

// localeTags maps every supported locale to its parsed language tag, the
// form go-i18n's Bundle.AddMessages expects.
var localeTags = map[string]language.Tag{
	LocaleZHCN: language.MustParse(LocaleZHCN),
	LocaleENUS: language.MustParse(LocaleENUS),
}

// Sentinel errors. Every one of them is wrapped with context (module name,
// locale, code) before it leaves the package, so errors.Is identifies the
// failure class and the error text says exactly what failed.
var (
	// ErrEmptyModuleName is returned by (*Builder).AddModule when module is
	// empty. A module name is the "<module>" half of every message id it
	// ships, so an unnamed module cannot own any message.
	ErrEmptyModuleName = errors.New("i18n: module name is empty")

	// ErrDuplicateModule is returned by (*Builder).AddModule when a module
	// with the same name was already added. Each module contributes its
	// messages once; a second bundle from the same module would be a bug
	// rather than a merge, and every message id carries the module name, so
	// no legitimate caller ever needs to add a module twice.
	ErrDuplicateModule = errors.New("i18n: duplicate module name")

	// ErrMissingLocaleFile is returned by (*Builder).AddModule when a
	// module ships some locale files but not the full zh-CN.toml +
	// en-US.toml pair. A module either ships no locale resources at all
	// (its Locales() embed.FS is empty, which contributes nothing) or it
	// ships both languages: the two-language guarantee is the point of the
	// catalog, and a single file is how an id silently stops existing for
	// half the users.
	ErrMissingLocaleFile = errors.New("i18n: missing locale file")

	// ErrUnsupportedLocale is returned by (*Builder).AddModule when a
	// module ships a locale file that is neither zh-CN.toml nor
	// en-US.toml. M0 catalogs support exactly those two locales; a third
	// file is a mismatch between the module's resources and the catalog
	// contract, not a third language this package could render.
	ErrUnsupportedLocale = errors.New("i18n: unsupported locale file")

	// ErrUnsupportedShape is returned by (*Builder).AddModule when a
	// locale file does not follow the flat one-entry-per-message shape:
	// a value that is neither a string nor a table of message keys, a
	// grouping section such as [errors], a message id without the
	// "<module>." prefix, or a message table with no translation. The
	// wrapped text names the offending file and key.
	ErrUnsupportedShape = errors.New("i18n: unsupported locale file shape")

	// ErrParityMismatch is returned by (*Builder).AddModule when a
	// module's zh-CN.toml and en-US.toml carry different message id sets.
	// The wrapped text lists the ids each language has that the other
	// lacks. tools/check_i18n_keys.py enforces the same rule in CI; this
	// error is the same check enforced in Go, at merge time.
	ErrParityMismatch = errors.New("i18n: locale key parity mismatch")

	// ErrUnknownLocale is returned by Lookup and LookupPlural when locale
	// is not a locale this catalog ships.
	ErrUnknownLocale = errors.New("i18n: unknown locale")

	// ErrUnknownCode is returned by Lookup and LookupPlural when code is
	// not a message id this catalog's locale carries. Because
	// (*Builder).AddModule enforces per-module key parity, an id that one
	// locale lacks is a bug in the module that shipped it -- it is never
	// silently rendered from another language.
	ErrUnknownCode = errors.New("i18n: unknown message code")
)

// Builder aggregates the locale bundles of every module during kernel
// wiring, one Builder per bootstrap, and freezes them into a Catalog.
//
// AddModule validates each module's locale files as they arrive: their
// shape, their id prefixes, the completeness of the zh-CN/en-US file pair
// and the parity of the two id sets. Everything that can be wrong with a
// module's message resources therefore fails the bootstrap at the module
// that owns it, before the catalog exists and while the error can still
// name the file. A Builder is not safe for concurrent use and does not need
// to be: Bootstrap drives it sequentially.
type Builder struct {
	modules     map[string]struct{}
	bundle      *goi18n.Bundle
	localeCodes map[string]map[string]struct{}
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{
		modules:     make(map[string]struct{}),
		bundle:      goi18n.NewBundle(language.English),
		localeCodes: make(map[string]map[string]struct{}),
	}
}

// AddModule loads module's locale resources and merges them into the
// catalog under construction.
//
// module must be the module's Name -- the "<module>" prefix every message
// id it ships must start with. fsys is the module's Locales() embed.FS,
// which is expected to hold zh-CN.toml and en-US.toml flat at its root,
// following the file contract in the package documentation.
//
// An fsys with no .toml files at all contributes nothing and is not an
// error: modules that render no content ship an empty embed.FS (the
// embed.FS{} every current test double returns), and treating that as a
// module with messages would invent ids rather than detect a bug. Any
// .toml file at the root of a non-empty fsys, however, commits the module
// to the full contract: both locale files must be present, each must parse
// into the flat one-entry-per-message shape, every id must start with
// module+".", and the two files must carry the same id set.
//
// Nothing is merged when AddModule returns an error. A module may be added
// at most once; adding it again returns ErrDuplicateModule.
func (b *Builder) AddModule(module string, fsys fs.FS) error {
	if module == "" {
		return ErrEmptyModuleName
	}
	if _, seen := b.modules[module]; seen {
		return fmt.Errorf("%w: %q", ErrDuplicateModule, module)
	}

	files, err := tomlFiles(fsys)
	if err != nil {
		return fmt.Errorf("i18n: read locale files of module %q: %w", module, err)
	}
	if len(files) == 0 {
		// No locale resources at all: the module renders nothing, matching
		// how dbkit's migration registry treats a module with no migration
		// directory for a dialect. Mark the module as seen anyway, so a
		// duplicated registration cannot slip through as two no-ops.
		b.modules[module] = struct{}{}
		return nil
	}

	for _, name := range files {
		if _, supported := localeFileNames[strings.TrimSuffix(name, ".toml")]; !supported {
			return fmt.Errorf(
				"%w: module %q ships %q, but M0 catalogs support exactly %s and %s",
				ErrUnsupportedLocale, module, name, localeFileNames[LocaleZHCN], localeFileNames[LocaleENUS])
		}
	}
	for _, fileName := range localeFileNames {
		if !slices.Contains(files, fileName) {
			return fmt.Errorf("%w: module %q ships locale files but not %q: every module ships both languages", ErrMissingLocaleFile, module, fileName)
		}
	}

	parsed := make(map[string][]*goi18n.Message, len(localeFileNames))
	for locale, fileName := range localeFileNames {
		data, err := fs.ReadFile(fsys, fileName)
		if err != nil {
			return fmt.Errorf("i18n: read %s of module %q: %w", fileName, module, err)
		}
		messages, err := parseLocaleFile(module, fileName, data)
		if err != nil {
			return err
		}
		parsed[locale] = messages
	}

	if mismatch := parityMismatch(parsed[LocaleZHCN], parsed[LocaleENUS]); mismatch != "" {
		return fmt.Errorf("%w: module %q: %s", ErrParityMismatch, module, mismatch)
	}

	// The merge itself: one AddMessages call per language. Every message
	// was already validated by parseLocaleFile, and the two id spaces are
	// disjoint per module by construction, so the only failure left is a
	// missing plural rule -- impossible for the two locales above -- and a
	// module is added to the bookkeeping below only after its messages are
	// in the bundle, so an error here leaves nothing half-merged.
	for locale, messages := range parsed {
		if err := b.bundle.AddMessages(localeTags[locale], messages...); err != nil {
			return fmt.Errorf("i18n: merge %s messages of module %q: %w", locale, module, err)
		}
		merged := b.localeCodes[locale]
		if merged == nil {
			merged = make(map[string]struct{})
			b.localeCodes[locale] = merged
		}
		for code := range codeSet(parsed[locale]) {
			merged[code] = struct{}{}
		}
	}

	b.modules[module] = struct{}{}
	return nil
}

// Build freezes the accumulated messages into a read-only Catalog. It
// always succeeds: every check that can reject a module runs in AddModule,
// so there is nothing left to fail here, and a Builder with no modules
// builds an empty catalog just as a bootstrap with no modules is an empty
// application. The Builder must not be used after Build.
func (b *Builder) Build() *Catalog {
	locals := make(map[string]*goi18n.Localizer, len(b.localeCodes))
	for locale := range b.localeCodes {
		locals[locale] = goi18n.NewLocalizer(b.bundle, locale)
	}
	return &Catalog{
		bundle: b.bundle,
		locals: locals,
		codes:  b.localeCodes,
	}
}

// Catalog is the frozen result of a Builder: every module's messages,
// merged and validated, ready for concurrent reads.
//
// A Catalog is immutable after Build -- it has no mutating methods -- and
// is therefore safe for concurrent use without a lock; the concurrency
// smoke test exercises exactly that. Only Kernel.Bootstrap produces one,
// which is why Registry.Locales() returns nil on a hand-built Registry,
// mirroring Registry.ObjectStore().
type Catalog struct {
	bundle *goi18n.Bundle
	// locals holds one go-i18n Localizer per locale this catalog ships,
	// pre-bound to the exact language tag of that locale.
	locals map[string]*goi18n.Localizer
	// codes records, per locale, the message ids that locale carries. It
	// backs the loud unknown-locale and unknown-code errors: go-i18n's own
	// lookup would otherwise fall back to the bundle's default language
	// and render English text for a zh-CN miss.
	codes map[string]map[string]struct{}
}

// Locales returns the locales this catalog ships, sorted, so callers can
// enumerate what a message renderer supports without hardcoding the pair.
func (c *Catalog) Locales() []string {
	out := make([]string, 0, len(c.locals))
	for locale := range c.locals {
		out = append(out, locale)
	}
	slices.Sort(out)
	return out
}

// Lookup renders the single-form message code in locale, interpolating
// params into its template.
//
// code must be a message id the locale carries. locale must be one of the
// locales this catalog ships (LocaleZHCN or LocaleENUS in M0); it is
// matched exactly, like the values the negotiation chain stores. params
// provides the template data: a message referencing {{.Name}} is rendered
// with params["Name"], and a parameter the message references but params
// does not provide renders as "<no value>", Go's text/template default.
//
// A message that defines plural categories is rendered with the "other"
// form by Lookup; call LookupPlural to select the category by count.
// Lookup never falls back to another language: an unknown locale returns
// ErrUnknownLocale, an unknown code ErrUnknownCode, and a message file
// whose template does not parse returns a render error -- all loud, none
// silent.
func (c *Catalog) Lookup(locale, code string, params map[string]any) (string, error) {
	return c.localize(locale, code, nil, params)
}

// LookupPlural renders the plural message code in locale for the given
// count, selecting the CLDR plural category count falls into for that
// locale -- zh-CN has only "other", en-US distinguishes "one" from "other"
// -- and interpolating params into the chosen form's template.
//
// count selects the category only; a plural message that renders the count
// itself, such as "{{.Count}} requests remain this month.", receives it
// through params, so the example above is rendered with
// LookupPlural(locale, code, n, map[string]any{"Count": n}).
//
// Like Lookup, LookupPlural fails loudly on an unknown locale or code
// rather than falling back to another language.
func (c *Catalog) LookupPlural(locale, code string, count int64, params map[string]any) (string, error) {
	return c.localize(locale, code, &count, params)
}

// localize is the shared core of Lookup and LookupPlural: check the locale
// and the code against this catalog's own bookkeeping -- never letting
// go-i18n's matcher fall back to the bundle's default language -- then
// delegate the actual rendering to the localizer bound to that locale.
func (c *Catalog) localize(locale, code string, pluralCount *int64, params map[string]any) (string, error) {
	codes, ok := c.codes[locale]
	if !ok {
		return "", fmt.Errorf("%w: %q (this catalog ships %v)", ErrUnknownLocale, locale, c.Locales())
	}
	if _, ok := codes[code]; !ok {
		return "", fmt.Errorf("%w: %q for locale %q", ErrUnknownCode, code, locale)
	}
	cfg := &goi18n.LocalizeConfig{MessageID: code, TemplateData: params}
	if pluralCount != nil {
		cfg.PluralCount = *pluralCount
	}
	rendered, err := c.locals[locale].Localize(cfg)
	if err != nil {
		return "", fmt.Errorf("i18n: render %q for locale %q: %w", code, locale, err)
	}
	return rendered, nil
}

// tomlFiles lists the .toml files at the root of fsys, sorted by name so
// that error reporting is deterministic. Files with any other name or
// extension are ignored, like dbkit's migration registry ignores non-.sql
// files: an embed.FS that also carries a README or an fs.go sibling never
// gets in the way. A directory is not recursed into: locale files are
// embedded flat at the FS root.
func tomlFiles(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".toml") {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// parseLocaleFile parses one locale file into the messages it declares,
// validating the flat one-entry-per-message shape as it goes. Every error
// wraps ErrUnsupportedShape and names the file, the message id and the
// problem, so a module author knows exactly which entry to fix.
func parseLocaleFile(module, fileName string, data []byte) ([]*goi18n.Message, error) {
	raw := make(map[string]any)
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %s: not valid TOML: %w", ErrUnsupportedShape, fileName, err)
	}

	prefix := module + "."
	keys := make([]string, 0, len(raw))
	for code := range raw {
		keys = append(keys, code)
	}
	slices.Sort(keys)

	messages := make([]*goi18n.Message, 0, len(keys))
	for _, code := range keys {
		if !strings.HasPrefix(code, prefix) || len(code) == len(prefix) {
			return nil, fmt.Errorf(
				"%w: %s: message id %q does not start with the module name %q and a dot -- the id is the whole quoted top-level key, and every id a module ships must live in its own \"<module>.\" id space (grouping sections such as [errors] are not supported either: go-i18n would fold the section name into the id)",
				ErrUnsupportedShape, fileName, code, module)
		}

		message := &goi18n.Message{ID: code}
		value := raw[code]
		switch v := value.(type) {
		case string:
			if v == "" {
				return nil, fmt.Errorf("%w: %s: message %q has an empty translation", ErrUnsupportedShape, fileName, code)
			}
			message.Other = v
		case map[string]any:
			if err := parseMessageTable(message, fileName, code, v); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf(
				"%w: %s: message %q is neither a string nor a table of message keys (zero/one/two/few/many/other, description, ...) but %T",
				ErrUnsupportedShape, fileName, code, value)
		}
		messages = append(messages, message)
	}
	return messages, nil
}

// parseMessageTable fills message from a plural-message table: keys from
// go-i18n's reserved message set, each carrying a string translation or
// annotation. A key outside that set means the table is doing something the
// flat contract does not support -- most commonly a grouping section such
// as [errors], which go-i18n would otherwise fold into the message id.
func parseMessageTable(message *goi18n.Message, fileName, code string, table map[string]any) error {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	// The v1 "translation" key is a whole-message synonym for other: valid
	// alone (and beside metadata keys), never next to a plural category
	// key. The mix is checked over the table, before the sorted walk, so
	// the rejection does not depend on which key the walk reaches first --
	// "zero" and "two" sort after "translation", and "one", "few" and
	// "many" never set the other form.
	v1Key, sawCategory := "", false
	for key := range table {
		switch strings.ToLower(key) {
		case "translation":
			v1Key = key
		case "zero", "one", "two", "few", "many", "other":
			sawCategory = true
		}
	}
	if v1Key != "" && sawCategory {
		return fmt.Errorf("%w: %s: message %q mixes %q with a plural category", ErrUnsupportedShape, fileName, code, v1Key)
	}

	hasTranslation := false
	for _, key := range keys {
		lower := strings.ToLower(key)
		switch lower {
		case "id":
			// The id key is redundant -- the table's own top-level key is
			// the id -- but when present it must agree with that key, so a
			// copy-pasted table cannot silently claim another message.
			value, ok := table[key].(string)
			if !ok {
				return fmt.Errorf("%w: %s: message %q: key %q must be a string", ErrUnsupportedShape, fileName, code, key)
			}
			if value != code {
				return fmt.Errorf("%w: %s: message %q: its id key says %q", ErrUnsupportedShape, fileName, code, value)
			}
			continue
		case "hash":
			// A translation-workflow artifact; carries no rendering meaning.
			continue
		case "description":
			value, ok := table[key].(string)
			if !ok {
				return fmt.Errorf("%w: %s: message %q: key %q must be a string", ErrUnsupportedShape, fileName, code, key)
			}
			message.Description = value
			continue
		case "leftdelim":
			value, ok := table[key].(string)
			if !ok {
				return fmt.Errorf("%w: %s: message %q: key %q must be a string", ErrUnsupportedShape, fileName, code, key)
			}
			message.LeftDelim = value
			continue
		case "rightdelim":
			value, ok := table[key].(string)
			if !ok {
				return fmt.Errorf("%w: %s: message %q: key %q must be a string", ErrUnsupportedShape, fileName, code, key)
			}
			message.RightDelim = value
			continue
		case "translation":
			// The v1 "translation" key is accepted as a synonym for other:
			// a single-form message may be written either way. The check
			// above the loop already rejected any table mixing it with
			// plural category keys; a nested v1-style table under it is not
			// supported either -- write the plural categories directly.
			value, ok := table[key].(string)
			if !ok {
				return fmt.Errorf("%w: %s: message %q: nested tables under %q are not supported; write the plural categories directly", ErrUnsupportedShape, fileName, code, key)
			}
			if value == "" {
				return fmt.Errorf("%w: %s: message %q: key %q has an empty translation", ErrUnsupportedShape, fileName, code, key)
			}
			message.Other = value
			hasTranslation = true
			continue
		case "zero", "one", "two", "few", "many", "other":
			// Handled below, where the value is read once.
		default:
			return fmt.Errorf(
				"%w: %s: message %q: key %q is not a message key (zero/one/two/few/many/other, description, ...) -- grouping sections such as [errors] are not supported, every message is one flat top-level entry",
				ErrUnsupportedShape, fileName, code, key)
		}

		value, ok := table[key].(string)
		if !ok {
			return fmt.Errorf("%w: %s: message %q: key %q must be a string", ErrUnsupportedShape, fileName, code, key)
		}
		if value == "" {
			return fmt.Errorf("%w: %s: message %q: key %q has an empty translation", ErrUnsupportedShape, fileName, code, key)
		}
		field := pluralField(message, lower)
		*field = value
		hasTranslation = true
	}

	if !hasTranslation {
		return fmt.Errorf("%w: %s: message %q is a table with no translation", ErrUnsupportedShape, fileName, code)
	}
	return nil
}

// pluralField returns the Message field holding the plural category form,
// or nil for a category this message does not carry.
func pluralField(message *goi18n.Message, category string) *string {
	switch category {
	case "zero":
		return &message.Zero
	case "one":
		return &message.One
	case "two":
		return &message.Two
	case "few":
		return &message.Few
	case "many":
		return &message.Many
	case "other":
		return &message.Other
	}
	return nil
}

// parityMismatch compares the ids of the zh-CN and en-US message sets of
// one module and returns a human-readable description of every id one
// language has that the other lacks, or "" when the sets are equal.
func parityMismatch(zhCN, enUS []*goi18n.Message) string {
	zhCodes, enCodes := codeSet(zhCN), codeSet(enUS)
	var missingInZH, missingInEN []string
	for code := range enCodes {
		if _, ok := zhCodes[code]; !ok {
			missingInZH = append(missingInZH, code)
		}
	}
	for code := range zhCodes {
		if _, ok := enCodes[code]; !ok {
			missingInEN = append(missingInEN, code)
		}
	}
	if len(missingInZH) == 0 && len(missingInEN) == 0 {
		return ""
	}
	slices.Sort(missingInZH)
	slices.Sort(missingInEN)
	var parts []string
	if len(missingInZH) > 0 {
		parts = append(parts, fmt.Sprintf("zh-CN lacks ids that en-US has: %s", strings.Join(missingInZH, ", ")))
	}
	if len(missingInEN) > 0 {
		parts = append(parts, fmt.Sprintf("en-US lacks ids that zh-CN has: %s", strings.Join(missingInEN, ", ")))
	}
	return strings.Join(parts, "; ")
}

// codeSet extracts the id set of a parsed message list.
func codeSet(messages []*goi18n.Message) map[string]struct{} {
	codes := make(map[string]struct{}, len(messages))
	for _, m := range messages {
		codes[m.ID] = struct{}{}
	}
	return codes
}
