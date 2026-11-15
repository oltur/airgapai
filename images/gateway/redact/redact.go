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

// Placeholders are short, so that a field rarely grows past its limit when something in it is removed.
var placeholder = map[Kind]string{Email: "[email]", Phone: "[phone]", IBAN: "[IBAN]", Card: "[card]"}

var (
	// A local part and a domain with a dot, as written in text. Not every address RFC 5322 allows,
	// but the ones people write.
	email = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9._%+-]*@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)
	// Two country letters, two check digits, then letters or digits in groups of up to four, with
	// optional single spaces, in either case. The checksum decides how much of it is an IBAN.
	iban = regexp.MustCompile(`(?i)\b[a-z]{2}\d{2}(?: ?[a-z0-9]{1,4}){3,8}`)
	// Digits starting with 2 to 6, as card networks' numbers do, in groups separated by single spaces
	// or hyphens. The checksum decides how much of it is a card number. The first digit keeps a UUID
	// such as the all-zero one, whose digits pass the checksum, from being read as a card.
	card = regexp.MustCompile(`\b[2-6](?:[ -]?\d)+`)
	// A number that starts with + and a country code, or with a 0 trunk prefix, possibly in brackets,
	// then digit groups separated by spaces, hyphens, dots or slashes.
	phone = regexp.MustCompile(`(?:\+\d{1,3}|\(0\d{1,5}\)|\b0\d{1,5})(?:[ ./-]?\(?\d+\)?)*`)
	// German dates, 05.11.2026, and file modes, 0644 or 0644 0755: numbers the phone pattern would
	// otherwise take.
	germanDate = regexp.MustCompile(`^\d{1,2}\.\d{1,2}\.(?:\d{2}|\d{4})$`)
	fileModes  = regexp.MustCompile(`^0[0-7]{3}(?: 0[0-7]{3})*$`)
)

// Text returns s with every find replaced by a placeholder, and how many of each kind it found.
// Cards and IBANs go first, since their digits could also pass for a phone number.
func Text(s string) (string, map[Kind]int) {
	found := map[Kind]int{}
	s = replace(s, email, Email, found, func(m string) int { return len(m) })
	s = replace(s, iban, IBAN, found, ibanPrefix)
	s = replace(s, card, Card, found, cardPrefix)
	s = replace(s, phone, Phone, found, phonePrefix)
	return s, found
}

// String is Text without the counts, for log lines.
func String(s string) string {
	out, _ := Text(s)
	return out
}

// replace finds each match of re in s, and asks take how long a prefix of it is personal data: 0 for
// none. That prefix is replaced, and the search goes on straight after it, so that a number written
// next to another, such as a card number and its expiry date, is still found. A match with no such
// prefix is kept whole.
func replace(s string, re *regexp.Regexp, k Kind, found map[Kind]int, take func(string) int) string {
	var b strings.Builder
	for {
		loc := re.FindStringIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		n := take(s[loc[0]:loc[1]])
		if n == 0 {
			// Not personal data: keep the whole match, so no part of the same token is tried again.
			b.WriteString(s[:loc[1]])
			s = s[loc[1]:]
			continue
		}
		found[k]++
		b.WriteString(s[:loc[0]])
		b.WriteString(placeholder[k])
		s = s[loc[0]+n:]
	}
}

// digitEnds lists, for each digit in m, the length of the prefix that ends with it, and how many
// digits that prefix holds.
func digitEnds(m string) (ends, counts []int) {
	n := 0
	for i := range len(m) {
		if m[i] >= '0' && m[i] <= '9' {
			n++
			ends, counts = append(ends, i+1), append(counts, n)
		}
	}
	return ends, counts
}

// cardPrefix is the longest prefix of 13 to 19 digits that passes the Luhn check, or 0.
func cardPrefix(m string) int {
	ends, counts := digitEnds(m)
	for i := len(ends) - 1; i >= 0; i-- {
		if counts[i] >= 13 && counts[i] <= 19 && luhnValid(digits(m[:ends[i]])) {
			return ends[i]
		}
	}
	return 0
}

// phonePrefix is the longest prefix, ending with a group of digits, that holds 8 to 15 digits, or 0.
// A group starting with 0 after a space begins another number once 8 digits are held. German dates
// and file modes are not phone numbers.
func phonePrefix(m string) int {
	best, count := 0, 0
	for i := 0; i < len(m); i++ {
		if m[i] < '0' || m[i] > '9' {
			continue
		}
		if count >= 8 && m[i] == '0' && i > 0 && m[i-1] == ' ' {
			break
		}
		count++
		end := i + 1
		if end < len(m) && m[end] >= '0' && m[end] <= '9' {
			continue
		}
		if end < len(m) && m[end] == ')' {
			end++
		}
		if count >= 8 && count <= 15 {
			best = end
		}
	}
	// It may stop short of the match only at a space: a match that runs on with a hyphen, a dot or a
	// slash is one token, such as a UUID, not a phone number followed by another.
	if best == 0 || (best < len(m) && m[best] != ' ') || germanDate.MatchString(m[:best]) || fileModes.MatchString(m[:best]) {
		return 0
	}
	return best
}

// ibanPrefix is the longest prefix of 15 to 34 characters, spaces aside, that passes the ISO 13616
// checksum, or 0.
func ibanPrefix(m string) int {
	for end := len(m); end > 0; end-- {
		if m[end-1] == ' ' {
			continue
		}
		s := strings.ToUpper(strings.ReplaceAll(m[:end], " ", ""))
		if len(s) >= 15 && len(s) <= 34 && ibanValid(s) {
			return end
		}
	}
	return 0
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

// luhnValid is the payment card checksum, on a string of digits.
func luhnValid(d string) bool {
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

// ibanValid is the ISO 13616 checksum on an upper-case IBAN without spaces: the first four characters
// moved to the end, letters as numbers from 10, and the whole taken modulo 97 must be 1.
func ibanValid(s string) bool {
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
