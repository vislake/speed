package notification

import (
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/go/pkgcore/i18n"
)

// testClinicCatalog returns a merged catalog holding a fake business
// module's ("clinic") notification-type copy -- the shape every real
// declaring module ships in its own locale files, per render.go's
// template-id convention: one zh-CN.toml and one en-US.toml with identical
// id sets, ids under the module's own prefix, and the recipient-facing
// params spelled out as {{.snake_case}} placeholders.
//
// clinic.appointment_reminder carries both a title and a body template;
// clinic.reminder_only carries a title alone, so render tests can ask for
// the missing body and get the coded failure the convention promises.
func testClinicCatalog(t *testing.T) *i18n.Catalog {
	t.Helper()
	fs := fstest.MapFS{
		"zh-CN.toml": &fstest.MapFile{Data: []byte(`
"clinic.appointment_reminder.title" = "预约提醒"
"clinic.appointment_reminder.body" = "{{.patient_name}} 您好，您预约的 {{.appointment_time}} 快到了。"
"clinic.reminder_only.title" = "只有标题的提醒"
`)},
		"en-US.toml": &fstest.MapFile{Data: []byte(`
"clinic.appointment_reminder.title" = "Appointment reminder"
"clinic.appointment_reminder.body" = "Hi {{.patient_name}}, your appointment at {{.appointment_time}} is coming up."
"clinic.reminder_only.title" = "A reminder with no body copy"
`)},
	}
	builder := i18n.NewBuilder()
	if err := builder.AddModule("clinic", fs); err != nil {
		t.Fatalf("AddModule(clinic): %v", err)
	}
	return builder.Build()
}

// TestRenderContent_RendersTitleAndBodyInTheAskedLocale drives the render
// seam through a real merged catalog in both directions: the same type and
// the same params render the module's zh-CN copy when the recipient's
// negotiated locale is zh-CN and its en-US copy when it is en-US -- the
// recipient's locale, never the operator's and never a fallback.
func TestRenderContent_RendersTitleAndBodyInTheAskedLocale(t *testing.T) {
	catalog := testClinicCatalog(t)
	params := map[string]any{
		"patient_name":     "王芳",
		"appointment_time": "2026-09-10 09:30",
	}

	title, body, err := renderContent(catalog, i18n.LocaleZHCN, "clinic.appointment_reminder", params)
	if err != nil {
		t.Fatalf("renderContent(zh-CN): %v", err)
	}
	if title != "预约提醒" {
		t.Errorf("zh-CN title = %q, want the zh-CN template rendered", title)
	}
	if body != "王芳 您好，您预约的 2026-09-10 09:30 快到了。" {
		t.Errorf("zh-CN body = %q, want the zh-CN template rendered with params interpolated", body)
	}

	title, body, err = renderContent(catalog, i18n.LocaleENUS, "clinic.appointment_reminder", params)
	if err != nil {
		t.Fatalf("renderContent(en-US): %v", err)
	}
	if title != "Appointment reminder" {
		t.Errorf("en-US title = %q, want the en-US template rendered", title)
	}
	if body != "Hi 王芳, your appointment at 2026-09-10 09:30 is coming up." {
		t.Errorf("en-US body = %q, want the en-US template rendered with params interpolated", body)
	}
}

// TestRenderContent_MissingTemplateId_ReportedAsInternal pins the failure
// mode render.go's doc comment promises: a type whose copy is incomplete
// (here, clinic.reminder_only ships a title but no body) renders nothing --
// the missing id surfaces as ErrInternal with the id on the record, never a
// half-rendered message and never another language's text. The same coded
// failure covers a type no module declared at all.
func TestRenderContent_MissingTemplateId_ReportedAsInternal(t *testing.T) {
	catalog := testClinicCatalog(t)

	_, _, err := renderContent(catalog, i18n.LocaleZHCN, "clinic.reminder_only", nil)
	assertCode(t, err, "notification.internal_error")

	_, _, err = renderContent(catalog, i18n.LocaleZHCN, "clinic.ghost_type", nil)
	assertCode(t, err, "notification.internal_error")
}

// TestRenderContent_UnknownLocale_ReportedAsInternal proves the never-falls-
// back half of the convention: a locale the catalog does not ship -- a
// recipient whose negotiation produced something outside zh-CN/en-US -- is a
// coded failure, not an implicit render in the catalog's default language.
func TestRenderContent_UnknownLocale_ReportedAsInternal(t *testing.T) {
	catalog := testClinicCatalog(t)

	_, _, err := renderContent(catalog, "fr-FR", "clinic.appointment_reminder", nil)
	assertCode(t, err, "notification.internal_error")
}

// TestRenderContent_NilCatalog_ReportedAsInternal pins the wiring failure:
// a delivery path calling the render seam before the host's catalog exists
// (or with none installed) must not guess -- it reports internal, loudly.
func TestRenderContent_NilCatalog_ReportedAsInternal(t *testing.T) {
	_, _, err := renderContent(nil, i18n.LocaleZHCN, "clinic.appointment_reminder", nil)
	assertCode(t, err, "notification.internal_error")
}
