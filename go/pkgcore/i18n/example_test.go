package i18n_test

import (
	"fmt"
	"testing/fstest"

	i18n "github.com/vislake/speed/go/pkgcore/i18n"
)

// ExampleBuilder walks the lifecycle Kernel.Bootstrap drives: every module's
// Locales() embed.FS is added to one Builder (here a module "notes" serving
// a pair of in-memory files standing in for its embed.FS), then Build freezes
// the merged catalog for lookup. Real locale files follow the same shape as
// these two.
func ExampleBuilder() {
	fsys := fstest.MapFS{
		"zh-CN.toml": {Data: []byte(`
"notes.text_required" = "备注内容不能为空。"
"notes.note_count" = { other = "共 {{.Count}} 条备注。" }
`)},
		"en-US.toml": {Data: []byte(`
"notes.text_required" = "Note text must not be empty."
"notes.note_count" = { one = "{{.Count}} note.", other = "{{.Count}} notes." }
`)},
	}

	builder := i18n.NewBuilder()
	if err := builder.AddModule("notes", fsys); err != nil {
		panic(err)
	}
	catalog := builder.Build()

	enText, err := catalog.Lookup(i18n.LocaleENUS, "notes.text_required", nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(enText)

	// The count selects the plural category; the template reads it from
	// params, so both must be passed when the message renders {{.Count}}.
	for _, count := range []int64{1, 5} {
		text, err := catalog.LookupPlural(i18n.LocaleZHCN, "notes.note_count", count,
			map[string]any{"Count": count})
		if err != nil {
			panic(err)
		}
		fmt.Println(text)
	}

	// Output:
	// Note text must not be empty.
	// 共 1 条备注。
	// 共 5 条备注。
}
