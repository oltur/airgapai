// Package redact removes personal data that a pattern can find: email addresses, phone numbers,
// IBANs and payment card numbers. It is applied to a user's question before the model or anything else
// sees it, to the text fields of a proposed ticket, and to every log line. Names, postal addresses and
// anything else without a fixed form are not found. See ADR 0023.
package redact

import (
	"regexp"
	"strings"
)

// Kind is a sort of personal data.
type Kind string

const (
	Email Kind = "email address"
	Phone Kind = "phone number"
	IBAN  Kind = "IBAN"
	Card  Kind = "payment card number"
)

// placeholder is what replaces each find.
func placeholder(k Kind) string { return "[" + string(k) + " removed]" }

var (
	// A local part and a domain with a dot, as written in text. Not every address RFC 5322 allows,
	// but the ones people write.
	email = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9._%+-]*@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)
	// Two country letters, two check digits, then 11 to 30 letters or digits, with optional spaces in
	// groups of four. The checksum below decides.
	iban = regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`)
	// 13 to 19 digits starting with 2 to 6, as card networks' numbers do, in groups separated by single
	// spaces or hyphens. The checksum below decides. The first digit keeps a UUID such as the all-zero
	// one, whose digits pass the checksum, from being read as a card.
	card = regexp.MustCompile(`\b[2-6](?:[ -]?\d){12,18}\b`)
	// A number that starts with + and a country code, or with a 0 trunk prefix, then digits in
	// groups separated by spaces, hyphens, dots, slashes or brackets. Counted below: 8 to 15 digits.
	phone = regexp.MustCompile(`(?:\+\d{1,3}|\(0\d{1,5}\)|\b0\d{1,5})(?:[ ./-]?\(?\d+\)?)+`)
)

// Text returns s with every find replaced by a placeholder, and how many of each kind it found.
// Cards and IBANs go first, since their digits could also pass for a phone number.
func Text(s string) (string, map[Kind]int) {
	found := map[Kind]int{}
	replace := func(re *regexp.Regexp, k Kind, ok func(string) bool) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			if ok != nil && !ok(m) {
				return m
			}
			found[k]++
			return placeholder(k)
		})
	}
	replace(email, Email, nil)
	replace(iban, IBAN, ibanValid)
	replace(card, Card, luhnValid)
	replace(phone, Phone, phoneLike)
	return s, found
}

// String is Text without the counts, for log lines.
func String(s string) string {
	out, _ := Text(s)
	return out
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// phoneLike counts the digits: fewer than 8 is a short number such as a port or a year, more than 15
// is longer than any international number.
func phoneLike(m string) bool {
	n := len(digits(m))
	return n >= 8 && n <= 15
}

// luhnValid is the payment card checksum.
func luhnValid(m string) bool {
	d := digits(m)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum := 0
	for i := range len(d) {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// ibanValid is the ISO 13616 checksum: the first four characters moved to the end, letters as
// numbers from 10, and the whole taken modulo 97 must be 1.
func ibanValid(m string) bool {
	s := strings.ReplaceAll(m, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			rem = (rem*10 + int(r-'0')) % 97
		case r >= 'A' && r <= 'Z':
			rem = (rem*100 + int(r-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}
