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
// id sets, ids under the module's own prefix and carrying the channel
// between the type key and the part
// (<type_key>.<channel>.<part>, e.g. clinic.appointment_reminder.email.subject),
// and the recipient-facing params spelled out as {{.snake_case}}
// placeholders.
//
// clinic.appointment_reminder carries full copy for all three channels --
// an in-app title/body pair, an email subject/body_text pair and an SMS
// text -- so render tests can drive the per-channel part table in every
// direction; clinic.reminder_only carries an in-app title alone, so a
// render that asks for its missing body gets the coded failure the
// convention promises. The three fixture types also ship their
// "<type_key>.description" id in both languages, the copy the type
// directory (handler.go's NotificationListTypes) renders from.
func testClinicCatalog(t *testing.T) *i18n.Catalog {
	t.Helper()
	fs := fstest.MapFS{
		"zh-CN.toml": &fstest.MapFile{Data: []byte(`
"clinic.appointment_reminder.in_app.title" = "预约提醒"
"clinic.appointment_reminder.in_app.body" = "{{.patient_name}} 您好，您预约的 {{.appointment_time}} 快到了。"
"clinic.appointment_reminder.email.subject" = "预约提醒"
"clinic.appointment_reminder.email.body_text" = "{{.patient_name}} 您好，您预约的 {{.appointment_time}} 快到了。详情请登录查看。"
"clinic.appointment_reminder.sms.text" = "{{.patient_name}} 您好，您预约的 {{.appointment_time}} 快到了。"
"clinic.appointment_reminder.description" = "在您的预约时间临近时发送的就诊提醒。"
"clinic.result_ready.description" = "在检查结果就绪时通知您。"
"clinic.security_alert.description" = "与您账户安全相关的重要通知。"
"clinic.reminder_only.in_app.title" = "只有标题的提醒"
`)},
		"en-US.toml": &fstest.MapFile{Data: []byte(`
"clinic.appointment_reminder.in_app.title" = "Appointment reminder"
"clinic.appointment_reminder.in_app.body" = "Hi {{.patient_name}}, your appointment at {{.appointment_time}} is coming up."
"clinic.appointment_reminder.email.subject" = "Appointment reminder"
"clinic.appointment_reminder.email.body_text" = "Hi {{.patient_name}}, your appointment at {{.appointment_time}} is coming up. Sign in for details."
"clinic.appointment_reminder.sms.text" = "Hi {{.patient_name}}, your appointment at {{.appointment_time}} is coming up."
"clinic.appointment_reminder.description" = "An appointment reminder sent as your visit approaches."
"clinic.result_ready.description" = "Lets you know when a result is ready."
"clinic.security_alert.description" = "Alerts about the security of your account."
"clinic.reminder_only.in_app.title" = "A reminder with no body copy"
`)},
	}
	builder := i18n.NewBuilder()
	if err := builder.AddModule("clinic", fs); err != nil {
		t.Fatalf("AddModule(clinic): %v", err)
	}
	return builder.Build()
}

// renderTestParams is the interpolation shape testClinicCatalog's templates
// reference.
var renderTestParams = map[string]any{
	"patient_name":     "王芳",
	"appointment_time": "2026-09-10 09:30",
}

// TestRenderContent_RendersInAppCopyInTheAskedLocale drives the in-app leg
// of the render seam through a real merged catalog in both directions: the
// same type and the same params render the module's zh-CN copy when the
// recipient's negotiated locale is zh-CN and its en-US copy when it is
// en-US -- the recipient's locale, never the operator's and never a
// fallback. The returned map carries exactly the in-app channel's parts
// (title and body) -- never the union of every channel's copy.
func TestRenderContent_RendersInAppCopyInTheAskedLocale(t *testing.T) {
	catalog := testClinicCatalog(t)

	parts, err := renderContent(catalog, i18n.LocaleZHCN, "clinic.appointment_reminder", ChannelInApp, renderTestParams)
	if err != nil {
		t.Fatalf("renderContent(zh-CN, in_app): %v", err)
	}
	if len(parts) != 2 {
		t.Errorf("zh-CN parts = %v keys, want exactly the in-app pair (title, body)", len(parts))
	}
	if got := parts["title"]; got != "预约提醒" {
		t.Errorf("zh-CN title = %q, want the zh-CN template rendered", got)
	}
	if got := parts["body"]; got != "王芳 您好，您预约的 2026-09-10 09:30 快到了。" {
		t.Errorf("zh-CN body = %q, want the zh-CN template rendered with params interpolated", got)
	}

	parts, err = renderContent(catalog, i18n.LocaleENUS, "clinic.appointment_reminder", ChannelInApp, renderTestParams)
	if err != nil {
		t.Fatalf("renderContent(en-US, in_app): %v", err)
	}
	if got := parts["title"]; got != "Appointment reminder" {
		t.Errorf("en-US title = %q, want the en-US template rendered", got)
	}
	if got := parts["body"]; got != "Hi 王芳, your appointment at 2026-09-10 09:30 is coming up." {
		t.Errorf("en-US body = %q, want the en-US template rendered with params interpolated", got)
	}
}

// TestRenderContent_RendersEmailCopy pins the email leg of the part table:
// the email channel's copy is a subject/body_text pair, keyed by those part
// names, and an SMS render never carries an email part into its output.
func TestRenderContent_RendersEmailCopy(t *testing.T) {
	catalog := testClinicCatalog(t)

	parts, err := renderContent(catalog, i18n.LocaleENUS, "clinic.appointment_reminder", ChannelEmail, renderTestParams)
	if err != nil {
		t.Fatalf("renderContent(en-US, email): %v", err)
	}
	if len(parts) != 2 {
		t.Errorf("email parts = %v keys, want exactly the subject/body_text pair", len(parts))
	}
	if got := parts["subject"]; got != "Appointment reminder" {
		t.Errorf("email subject = %q, want the email subject template rendered", got)
	}
	if got := parts["body_text"]; got != "Hi 王芳, your appointment at 2026-09-10 09:30 is coming up. Sign in for details." {
		t.Errorf("email body_text = %q, want the email body template rendered with params interpolated", got)
	}
}

// TestRenderContent_RendersSMSCopy pins the SMS leg of the part table: an
// SMS has a single text part, and the returned map carries only it -- one
// key, not a padded title or body the SMS template never declared.
func TestRenderContent_RendersSMSCopy(t *testing.T) {
	catalog := testClinicCatalog(t)

	parts, err := renderContent(catalog, i18n.LocaleZHCN, "clinic.appointment_reminder", ChannelSMS, renderTestParams)
	if err != nil {
		t.Fatalf("renderContent(zh-CN, sms): %v", err)
	}
	if len(parts) != 1 {
		t.Errorf("sms parts = %v keys, want exactly the single text part", len(parts))
	}
	if got := parts["text"]; got != "王芳 您好，您预约的 2026-09-10 09:30 快到了。" {
		t.Errorf("sms text = %q, want the sms template rendered with params interpolated", got)
	}
}

// TestRenderContent_MissingTemplateId_ReportedAsInternal pins the failure
// mode render.go's doc comment promises: a type whose copy is incomplete
// (here, clinic.reminder_only ships an in-app title but no in-app body)
// renders nothing -- the missing id surfaces as ErrInternal, never a
// half-rendered message and never another language's text. The same coded
// failure covers a type no module declared at all.
func TestRenderContent_MissingTemplateId_ReportedAsInternal(t *testing.T) {
	catalog := testClinicCatalog(t)

	_, err := renderContent(catalog, i18n.LocaleZHCN, "clinic.reminder_only", ChannelInApp, nil)
	assertCode(t, err, "notification.internal_error")

	_, err = renderContent(catalog, i18n.LocaleZHCN, "clinic.ghost_type", ChannelInApp, nil)
	assertCode(t, err, "notification.internal_error")
}

// TestRenderContent_UnknownChannel_ReportedAsInternal pins the channel
// table's own guard: a channel outside the closed in_app/email/sms
// vocabulary is a coded failure -- a delivery path asking for copy it
// cannot name would otherwise read an empty part set and ship an empty
// message.
func TestRenderContent_UnknownChannel_ReportedAsInternal(t *testing.T) {
	catalog := testClinicCatalog(t)

	_, err := renderContent(catalog, i18n.LocaleZHCN, "clinic.appointment_reminder", "carrier_pigeon", nil)
	assertCode(t, err, "notification.internal_error")
}

// TestRenderContent_UnknownLocale_ReportedAsInternal proves the never-falls-
// back half of the convention: a locale the catalog does not ship -- a
// recipient whose negotiation produced something outside zh-CN/en-US -- is a
// coded failure, not an implicit render in the catalog's default language.
func TestRenderContent_UnknownLocale_ReportedAsInternal(t *testing.T) {
	catalog := testClinicCatalog(t)

	_, err := renderContent(catalog, "fr-FR", "clinic.appointment_reminder", ChannelInApp, nil)
	assertCode(t, err, "notification.internal_error")
}

// TestRenderContent_NilCatalog_ReportedAsInternal pins the wiring failure:
// a delivery path calling the render seam before the host's catalog exists
// (or with none installed) must not guess -- it reports internal, loudly.
func TestRenderContent_NilCatalog_ReportedAsInternal(t *testing.T) {
	_, err := renderContent(nil, i18n.LocaleZHCN, "clinic.appointment_reminder", ChannelInApp, nil)
	assertCode(t, err, "notification.internal_error")
}
