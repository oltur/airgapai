package redact

import (
	"strings"
	"testing"
)

// All data here is made up. The IBANs and card numbers are the published test values that pass their
// checksums and belong to no one.
func TestFinds(t *testing.T) {
	for _, c := range []struct {
		in, want string
		kind     Kind
	}{
		{"Mail jane.doe@example.com today.", "Mail [email address removed] today.", Email},
		{"cc: a+b.c@sub.example.org,", "cc: [email address removed],", Email},
		{"Call +49 30 12345678.", "Call [phone number removed].", Phone},
		{"Call 030 1234 5678 or", "Call [phone number removed] or", Phone},
		{"Office (030) 123-4567.", "Office [phone number removed].", Phone},
		{"US +1 415 555 0100", "US [phone number removed]", Phone},
		{"Mobile 0176/12345678", "Mobile [phone number removed]", Phone},
		{"Pay to DE89 3704 0044 0532 0130 00 now", "Pay to [IBAN removed] now", IBAN},
		{"Pay to DE89370400440532013000.", "Pay to [IBAN removed].", IBAN},
		{"UK GB82 WEST 1234 5698 7654 32", "UK [IBAN removed]", IBAN},
		{"Card 4111 1111 1111 1111 expires", "Card [payment card number removed] expires", Card},
		{"Card 5555-5555-5555-4444.", "Card [payment card number removed].", Card},
		{"Card 4111111111111111", "Card [payment card number removed]", Card},
	} {
		got, found := Text(c.in)
		if got != c.want || found[c.kind] != 1 {
			t.Errorf("%q: got %q, %v; want %q", c.in, got, found, c.want)
		}
	}
}

// Text from the Kubernetes corpus and this lab that must be left as it is.
func TestLeavesOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"10.43.0.50:5000/airgapai/gateway@sha256:4e3a1b4697d222903f85a3e4a42fde4791b8bf9f721e0e57d48cf7324b9c5bb0",
		"Set port 8080 and nodePort 30080, replicas: 3, cpu: 0.5, memory: 512Mi.",
		"Upgrade to v1.27.4 on 2026-11-15T10:20:30Z, mode 0644, schedule 0 10 * * *.",
		"Proposal 00000000-0000-0000-0000-000000000000 and 7ac586d0-28b6-4b83-b388-b344f4bea42e.",
		"Issue 1234 5678 and job 100000000000 ran.",
		"Not an IBAN: DE00 1234.",
		"Not a card: 4111 1111 1111 1112, nor 1234 5678 9012 3456.",
		"Short numbers: 030 123, +49 30, 0176.",
		"kubectl get pods -o jsonpath='{.items[0].metadata.name}'",
	} {
		if got, found := Text(s); got != s || len(found) != 0 {
			t.Errorf("%q changed to %q, %v", s, got, found)
		}
	}
}

// An IBAN with a bad checksum is never removed as an IBAN. Its digits can still match another rule:
// with ...01, 370400440532013001 passes the card checksum, and with ...02, 0044 0532 0130 02 reads as
// a phone number dialled with 00. Either way bank details are removed, which errs on the safe side.
func TestBadIBANs(t *testing.T) {
	for in, kind := range map[string]Kind{"DE89 3704 0044 0532 0130 01": Card, "DE89 3704 0044 0532 0130 02": Phone} {
		got, found := Text(in)
		if found[IBAN] != 0 || found[kind] != 1 {
			t.Errorf("%q: got %q, %v", in, got, found)
		}
	}
}

// A known false positive: an SSH remote reads as an email address.
func TestKnownFalsePositive(t *testing.T) {
	if got := String("git@github.com:oltur/airgapai.git"); !strings.Contains(got, "[email address removed]") {
		t.Errorf("got %q", got)
	}
}

// Redacting twice changes nothing more, and no placeholder is itself found again.
func TestIdempotent(t *testing.T) {
	s := "jane.doe@example.com, +49 30 12345678, DE89 3704 0044 0532 0130 00, 4111 1111 1111 1111"
	once := String(s)
	if twice, found := Text(once); twice != once || len(found) != 0 {
		t.Errorf("second pass: %q, %v", twice, found)
	}
	for _, leak := range []string{"jane", "12345678", "DE89", "4111"} {
		if strings.Contains(once, leak) {
			t.Errorf("%q left in %q", leak, once)
		}
	}
}
