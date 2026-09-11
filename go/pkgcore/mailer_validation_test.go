package pkgcore

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateMail_RejectsControlCharactersInAddressFields pins the rule
// validateMail applies to the three address fields: From, every To entry and
// the optional ReplyTo carry no control character at all -- the C0 range (a
// tab included, which is never header whitespace inside an address), DEL and
// the C1 range. Those values travel unescaped into the SMTP command lines and
// the raw DATA headers, so anything in that class would reach the relay
// verbatim. A CR or LF -- the characters that would end the line and let
// further headers be injected -- is named as a line break in its error;
// every other control character gets its own message. No error ever echoes
// the offending value back.
func TestValidateMail_RejectsControlCharactersInAddressFields(t *testing.T) {
	t.Parallel()

	valid := Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "valid", Text: "body",
	}

	controls := []struct {
		name string
		char string
		want string
	}{
		{"CR", "\r", "must not contain a line break"},
		{"LF", "\n", "must not contain a line break"},
		{"TAB", "\t", "must not contain a control character"},
		{"NUL", "\x00", "must not contain a control character"},
		{"VT", "\x0b", "must not contain a control character"},
		{"DEL", "\x7f", "must not contain a control character"},
		{"NEL", "\u0085", "must not contain a control character"},
	}
	fields := []struct {
		name string
		mut  func(m *Mail, s string)
	}{
		{"From", func(m *Mail, s string) { m.From = "ops@example.com" + s }},
		{"To", func(m *Mail, s string) { m.To = []string{"ada@example.com" + s} }},
		{"ReplyTo", func(m *Mail, s string) { m.ReplyTo = "ops@example.com" + s }},
	}

	for _, field := range fields {
		for _, control := range controls {
			t.Run(field.name+"_"+control.name, func(t *testing.T) {
				t.Parallel()

				mail := valid
				field.mut(&mail, control.char)

				err := validateMail(mail)
				if !errors.Is(err, ErrInvalidMail) {
					t.Fatalf("validateMail(%s carrying %#v) = %v, want ErrInvalidMail", field.name, control.char, err)
				}
				if !strings.Contains(err.Error(), control.want) {
					t.Errorf("error = %v, want it to say %q", err, control.want)
				}
				if strings.Contains(err.Error(), "example.com") {
					t.Errorf("error = %v, want the offending address left out of the text", err)
				}
			})
		}
	}
}

// TestValidateMail_SubjectKeepsTheLineBreakOnlyRule pins where the subject's
// rule stops: a CR or LF would end the header line and is rejected, while a
// tab -- legal header whitespace in an unstructured field -- and non-ASCII
// text, which encodeSubject renders as an RFC 2047 encoded word, stay
// acceptable.
func TestValidateMail_SubjectKeepsTheLineBreakOnlyRule(t *testing.T) {
	t.Parallel()

	for _, bad := range []struct{ name, subject string }{
		{"a_CR", "subject\rInjected: header"},
		{"an_LF", "subject\nInjected: header"},
	} {
		t.Run("rejects_"+bad.name, func(t *testing.T) {
			t.Parallel()

			mail := Mail{From: "ops@example.com", To: []string{"ada@example.com"}, Subject: bad.subject, Text: "body"}
			err := validateMail(mail)
			if !errors.Is(err, ErrInvalidMail) {
				t.Fatalf("validateMail(subject %#v) = %v, want ErrInvalidMail", bad.subject, err)
			}
			if !strings.Contains(err.Error(), "Subject must not contain a line break") {
				t.Errorf("error = %v, want it to name the Subject's line break", err)
			}
		})
	}

	for _, good := range []struct{ name, subject string }{
		{"a_tab", "subject\twith a tab"},
		{"non_ASCII_text", "发票 #1042 已开具"},
	} {
		t.Run("accepts_"+good.name, func(t *testing.T) {
			t.Parallel()

			mail := Mail{From: "ops@example.com", To: []string{"ada@example.com"}, Subject: good.subject, Text: "body"}
			if err := validateMail(mail); err != nil {
				t.Errorf("validateMail(subject %#v) = %v, want nil", good.subject, err)
			}
		})
	}
}

// TestValidateMail_RejectsAddressesThatDoNotParse pins the structural half
// of the address rule: the three address fields -- From, every To entry and
// the optional ReplyTo -- must each parse as one address through
// net/mail.ParseAddress. A value that is not even address-shaped would
// otherwise travel verbatim into the MAIL FROM/RCPT TO commands and the raw
// DATA headers, where no relay can make it deliverable. Each failure names
// the field and never echoes the offending value.
func TestValidateMail_RejectsAddressesThatDoNotParse(t *testing.T) {
	t.Parallel()

	valid := Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "valid", Text: "body",
	}
	tests := []struct {
		name string
		mut  func(*Mail)
		want string
		leak string
	}{
		{"a_bare_string_in_From", func(m *Mail) { m.From = "ops" }, "From is not a valid address", "ops"},
		{"a_missing_at_in_From", func(m *Mail) { m.From = "ops.example.com" }, "From is not a valid address", "ops.example.com"},
		{"an_empty_domain_in_From", func(m *Mail) { m.From = "ops@" }, "From is not a valid address", "ops@"},
		{"an_empty_local_part_in_From", func(m *Mail) { m.From = "@example.com" }, "From is not a valid address", "@example.com"},
		{"more_than_one_at_in_From", func(m *Mail) { m.From = "ops@@example.com" }, "From is not a valid address", "ops@@example.com"},
		{"an_unclosed_angle_addr_in_From", func(m *Mail) { m.From = "Ada <ada@example.com" }, "From is not a valid address", "Ada <ada@example.com"},
		{"a_bare_string_in_To", func(m *Mail) { m.To = []string{"ada"} }, "To contains an invalid address", "ada"},
		{"adjacent_dots_in_the_domain_in_To", func(m *Mail) { m.To = []string{"ada@example..com"} }, "To contains an invalid address", "ada@example..com"},
		{"two_addresses_in_one_To_entry", func(m *Mail) { m.To = []string{"ada@example.com, grace@example.com"} }, "To contains an invalid address", "ada@example.com, grace@example.com"},
		{"a_bare_string_in_ReplyTo", func(m *Mail) { m.ReplyTo = "reply-to" }, "ReplyTo is not a valid address", "reply-to"},
	}

	for _, tt := range tests {
		t.Run("rejects_"+tt.name, func(t *testing.T) {
			t.Parallel()

			mail := valid
			tt.mut(&mail)

			err := validateMail(mail)
			if !errors.Is(err, ErrInvalidMail) {
				t.Fatalf("validateMail(%+v) = %v, want ErrInvalidMail", mail, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to say %q", err, tt.want)
			}
			if strings.Contains(err.Error(), tt.leak) {
				t.Errorf("error = %v, want the offending value %q left out of the text", err, tt.leak)
			}
		})
	}
}

// TestValidateMail_AcceptsAddressFormsTheContractAllows pins the accept side
// of the address rule: forms the structural check must not over-reject. The
// plain-address form, the name-and-address display form the Mail
// documentation leaves to the caller, a plus-tagged local part, non-ASCII
// and IDN forms, a domain without a dot -- everything net/mail.ParseAddress
// itself accepts -- and a message whose ReplyTo is left empty all pass
// validation.
func TestValidateMail_AcceptsAddressFormsTheContractAllows(t *testing.T) {
	t.Parallel()

	mails := []struct {
		name string
		mail Mail
	}{
		{
			name: "plain addresses",
			mail: Mail{From: "ops@example.com", To: []string{"ada@example.com", "grace@example.com"}, Subject: "plain", Text: "body"},
		},
		{
			name: "display-name address forms",
			mail: Mail{From: "Ada Lovelace <ada@example.com>", To: []string{"Grace Hopper <grace@example.com>"}, Subject: "display names", Text: "body"},
		},
		{
			name: "a quoted display name",
			mail: Mail{From: `"Ada Lovelace" <ada@example.com>`, To: []string{"Grace Hopper <grace@example.com>"}, Subject: "quoted display name", Text: "body"},
		},
		{
			name: "a plus-tagged local part",
			mail: Mail{From: "ops+notifications@example.com", To: []string{"ada+invoices@example.com"}, Subject: "plus tag", Text: "body"},
		},
		{
			name: "an IDN domain and a non-ASCII local part",
			mail: Mail{From: "ada@例子.公司", To: []string{"用户@example.com"}, Subject: "non-ASCII", Text: "body"},
		},
		{
			name: "a domain without a dot, as the parser itself accepts it",
			mail: Mail{From: "ops@example", To: []string{"ada@localhost"}, Subject: "dotless domain", Text: "body"},
		},
		{
			name: "an empty ReplyTo",
			mail: Mail{From: "ops@example.com", To: []string{"ada@example.com"}, Subject: "no reply-to", Text: "body"},
		},
	}
	for _, tt := range mails {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := validateMail(tt.mail); err != nil {
				t.Errorf("validateMail(%+v) = %v, want nil", tt.mail, err)
			}
		})
	}
}
