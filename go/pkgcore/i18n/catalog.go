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

// LocaleZHCN and LocaleENUS name the two languages whose full coverage v1.0
// guarantees (docs/internal/11-cross-cutting.md). They are naming
// conveniences for callers that want the M0 pair by hand -- the zh-CN
// default of the negotiation chain, the lookup tests -- not a closed list:
// the catalog serves exactly the languages modules ship locale files for,
// and a language added later is one more file, never a change here.
const (
	// LocaleZHCN is Simplified Chinese, the default language of the
	// negotiation chain in docs/internal/11-cross-cutting.md.
	LocaleZHCN = "zh-CN"
	// LocaleENUS is American English.
	LocaleENUS = "en-US"
)

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
	// module ships some locale files but not one for every language of the
	// catalog. A module either ships no locale resources at all (its
	// Locales() embed.FS is empty, which contributes nothing) or it ships
	// one file per catalog language -- in M0, zh-CN.toml and en-US.toml:
	// full coverage is the point of the catalog, and a missing file is how
	// an id silently stops existing for the users of that language.
	ErrMissingLocaleFile = errors.New("i18n: missing locale file")

	// ErrUnsupportedLocale is returned by (*Builder).AddModule when a
	// module ships a locale file that is not a language of the catalog:
	// a file name that is not the canonical spelling of a BCP 47 language
	// tag (the file name is the language), or a file for a language the
	// catalog does not serve -- the set fixed by the first module that
	// shipped files. The catalog is not closed to zh-CN and en-US; its
	// languages are the files modules ship.
	ErrUnsupportedLocale = errors.New("i18n: unsupported locale file")

	// ErrUnsupportedShape is returned by (*Builder).AddModule when a
	// locale file does not follow the flat one-entry-per-message shape:
	// a value that is neither a string nor a table of message keys, a
	// grouping section such as [errors], a message id without the
	// "<module>." prefix, or a message table with no translation. The
	// wrapped text names the offending file and key.
	ErrUnsupportedShape = errors.New("i18n: unsupported locale file shape")

	// ErrParityMismatch is returned by (*Builder).AddModule when a
	// module's locale files carry different message id sets. The wrapped
	// text lists, per language, the ids one language has that another
	// lacks. tools/check_i18n_keys.py enforces the same rule for the
	// zh-CN/en-US pair in CI; this error is the same check enforced in Go
	// across every language a module ships, at merge time.
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
// The catalog's languages are declared by the locale files themselves,
// never by this package: a file is <language>.toml at the root of a
// module's Locales() embed.FS, and the catalog serves exactly the
// languages the modules ship. The first module that ships files fixes the
// catalog's language set; every later module that ships files must ship
// exactly that set, so every catalog language is covered by every module
// that renders content. Adding a language is therefore one new file per
// message-shipping module and nothing else -- the mechanism
// docs/internal/11-cross-cutting.md requires, which scopes v1.0's
// full-coverage guarantee to zh-CN and en-US without freezing the catalog
// to them.
//
// AddModule validates each module's locale files as they arrive: their
// shape, their id prefixes, the completeness of the file set against the
// catalog's languages and the parity of the languages' id sets. Everything
// that can be wrong with a module's message resources therefore fails the
// bootstrap at the module that owns it, before the catalog exists and
// while the error can still name the file. A Builder is not safe for
// concurrent use and does not need to be: Bootstrap drives it
// sequentially.
type Builder struct {
	modules map[string]struct{}
	bundle  *goi18n.Bundle
	// catalogLocales is the catalog's language set: the sorted locale
	// codes (file stems) of the first module that shipped files. It stays
	// nil until some module ships files, so a bootstrap whose modules all
	// render nothing builds a language-less catalog.
	catalogLocales []string
	localeCodes    map[string]map[string]struct{}
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
// holding one file per catalog language flat at its root, following the
// file contract in the package documentation: a file is <language>.toml,
// and the first module that ships files fixes which languages the catalog
// serves. A later module must ship exactly that same set -- one file for
// every catalog language, none for any other.
//
// An fsys with no .toml files at all contributes nothing and is not an
// error: modules that render no content ship an empty embed.FS (the
// embed.FS{} every current test double returns), and treating that as a
// module with messages would invent ids rather than detect a bug. Any
// .toml file at the root of a non-empty fsys, however, commits the module
// to the full contract: one file per language the catalog serves (in M0,
// zh-CN.toml and en-US.toml) and no file for any other language, each file
// parsing into the flat one-entry-per-message shape, every id starting
// with module+".", and every language carrying the same id set.
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

	// A file name is a language: <language>.toml, where <language> must be
	// the canonical spelling of a BCP 47 tag -- the form lookup and
	// negotiation values use ("zh-CN", never "zh-cn" or "zh_CN"). Parsing
	// here yields the tag go-i18n's AddMessages expects and rejects a file
	// that is not a language at all.
	shipped := make(map[string]language.Tag, len(files))
	codes := make([]string, 0, len(files))
	for _, name := range files {
		code := strings.TrimSuffix(name, ".toml")
		tag, err := language.Parse(code)
		if err != nil || tag.String() != code {
			return fmt.Errorf(
				"%w: module %q ships %q, which is not a locale file name: a locale file is <language>.toml, where <language> is the canonical spelling of a BCP 47 language tag, such as %q",
				ErrUnsupportedLocale, module, name, LocaleZHCN+".toml")
		}
		shipped[code] = tag
		codes = append(codes, code)
	}
	slices.Sort(codes)

	if b.catalogLocales == nil {
		// The first module to ship files fixes the catalog's language set:
		// no language list exists outside the files, so whatever this
		// module ships is what the catalog serves.
		b.catalogLocales = codes
	} else {
		// Every later module must ship exactly that set -- one file per
		// catalog language, and no file for any other. A module missing a
		// language's file would render that language's ids nowhere; a
		// module shipping an extra language would render ids the other
		// modules never provided. Either mismatch is reported at the
		// module that carries it.
		for _, code := range b.catalogLocales {
			if _, ok := shipped[code]; !ok {
				return fmt.Errorf(
					"%w: module %q ships locale files but not %q: every module that ships messages ships one file per language the catalog serves (%s)",
					ErrMissingLocaleFile, module, code+".toml", fileNames(b.catalogLocales))
			}
		}
		for _, code := range codes {
			if !slices.Contains(b.catalogLocales, code) {
				return fmt.Errorf(
					"%w: module %q ships %q, but this catalog serves %s: adding a language means adding its file to every module that ships messages",
					ErrUnsupportedLocale, module, code+".toml", fileNames(b.catalogLocales))
			}
		}
	}

	parsed := make(map[string][]*goi18n.Message, len(codes))
	for _, code := range codes {
		data, err := fs.ReadFile(fsys, code+".toml")
		if err != nil {
			return fmt.Errorf("i18n: read %s of module %q: %w", code+".toml", module, err)
		}
		messages, err := parseLocaleFile(module, code+".toml", data)
		if err != nil {
			return err
		}
		parsed[code] = messages
	}

	if mismatch := parityMismatch(parsed); mismatch != "" {
		return fmt.Errorf("%w: module %q: %s", ErrParityMismatch, module, mismatch)
	}

	// The merge itself: one AddMessages call per language. Every message
	// was already validated by parseLocaleFile, and id spaces are disjoint
	// per module by construction, so the only failure left is a missing
	// plural rule -- go-i18n ships CLDR plural rules for every language --
	// and a module is added to the bookkeeping below only after its
	// messages are in the bundle, so an error here leaves nothing
	// half-merged.
	for _, code := range codes {
		if err := b.bundle.AddMessages(shipped[code], parsed[code]...); err != nil {
			return fmt.Errorf("i18n: merge %s messages of module %q: %w", code, module, err)
		}
		merged := b.localeCodes[code]
		if merged == nil {
			merged = make(map[string]struct{})
			b.localeCodes[code] = merged
		}
		for id := range codeSet(parsed[code]) {
			merged[id] = struct{}{}
		}
	}

	b.modules[module] = struct{}{}
	return nil
}

// fileNames renders locale codes as the .toml file names error text lists,
// "zh-CN.toml, en-US.toml".
func fileNames(codes []string) string {
	names := make([]string, len(codes))
	for i, code := range codes {
		names[i] = code + ".toml"
	}
	return strings.Join(names, ", ")
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
// enumerate what a message renderer supports without hardcoding a language
// list.
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
// locales this catalog ships -- in M0, LocaleZHCN and LocaleENUS, the pair
// Catalog.Locales() enumerates; it is matched exactly, like the values the
// negotiation chain stores. params
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

// parityMismatch compares the id sets of one module's locale files and
// returns a human-readable description of every id one language has that
// another lacks, or "" when the sets are all equal. The first language in
// sorted order is the reference and every other language is reported
// against it, so a report has at most two lines per additional language.
func parityMismatch(byLocale map[string][]*goi18n.Message) string {
	codes := make([]string, 0, len(byLocale))
	for code := range byLocale {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	if len(codes) < 2 {
		return ""
	}
	reference := codes[0]
	refCodes := codeSet(byLocale[reference])
	var parts []string
	for _, other := range codes[1:] {
		otherCodes := codeSet(byLocale[other])
		var missingInOther, missingInReference []string
		for code := range refCodes {
			if _, ok := otherCodes[code]; !ok {
				missingInOther = append(missingInOther, code)
			}
		}
		for code := range otherCodes {
			if _, ok := refCodes[code]; !ok {
				missingInReference = append(missingInReference, code)
			}
		}
		slices.Sort(missingInOther)
		slices.Sort(missingInReference)
		if len(missingInOther) > 0 {
			parts = append(parts, fmt.Sprintf("%s lacks ids that %s has: %s", other, reference, strings.Join(missingInOther, ", ")))
		}
		if len(missingInReference) > 0 {
			parts = append(parts, fmt.Sprintf("%s lacks ids that %s has: %s", reference, other, strings.Join(missingInReference, ", ")))
		}
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
