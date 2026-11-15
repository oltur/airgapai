# 0023: Pattern-based redaction of personal data

**Status:** accepted
**Date:** 2026-11-15

## Context

The plan asks for pattern-based PII redaction on input and logs, tested on synthetic data. The corpus
holds no personal data, but users type questions, and a technician reporting a fault may well write a
phone number or an email address into one. That text goes to the model, can be copied into a ticket
that two teams read, and can end up in a log line through an error message.

## Decision

- **A `redact` package in the gateway finds four kinds of personal data:**
  - **Email addresses:** a local part, then a domain with a dot.
  - **Phone numbers:** starting with `+` and a country code, or with a `0` trunk prefix, possibly in
    brackets, with 8 to 15 digits in all.
  - **IBANs:** only if they pass the ISO 13616 checksum.
  - **Payment card numbers:** 13 to 19 digits starting with 2 to 6, only if they pass the Luhn
    checksum.

  Each find is replaced by a placeholder such as `[email address removed]`. Cards and IBANs are
  checked before phone numbers, whose pattern their digits could also match.
- **The checksums and the digit counts are what keep ordinary text intact.** This project's text is
  full of numbers: ports, versions, digests, UUIDs, IP addresses, dates and file modes. The tests check
  a set of these is left as it is. A card number must also start with 2 to 6, as card numbers do: the
  all-zero UUID's first 16 digits pass the Luhn check.
- **Applied in three places:**
  - **The question,** as soon as the form is read, before the model, the page or the log sees it. The
    page shows the redacted question, and says how many of each kind were removed.
  - **Every text field of a proposed ticket,** before it is proposed, so the user confirms, and the
    executor files, the redacted text. Numbers keep their exact form. Arguments the approval core
    would refuse, such as ones with a duplicate key, are passed on unchanged so that it does refuse
    them, since decoding them into a map would quietly merge the keys.
  - **Every gateway log line,** through one function, `logf`. A test checks the gateway's sources for
    any other way of logging.
- **A stub scenario, `echo`, copies everything the model receives into a ticket,** so the VM test can
  see what reached the model.

## Alternatives

- **A named-entity model for names and addresses.** It would add a model to judge text, with its own
  errors and memory, and the plan asks for patterns. Names and postal addresses are a stated gap.
- **Redacting the model's answers.** The model only sees the redacted question and the corpus, which
  holds no personal data. An answer could hold personal data only if the model made it up, so answers
  are not redacted. The citation check (ADR 0022) withholds answers that do not cite.
- **Refusing questions with personal data.** It would stop a technician from reporting a fault in their
  own words. Removing the data keeps the question usable.

## Consequences

- **Some things are removed that are not personal data:**
  - An SSH remote such as `git@github.com:…` reads as an email address. A test pins this.
  - An IBAN with a bad checksum is never removed as an IBAN, but its digits can still match another
    rule. Of two tested, one passed the card checksum and the other read as a phone number dialled
    with `00`. Bank details are removed either way.
  - A long digit string starting with `0` and holding 8 to 15 digits reads as a phone number.
- **Some personal data is not found:** names, postal addresses, ID numbers without a checksum, phone
  numbers written without a `+` or a leading `0`, and email addresses obscured as "jane at example dot
  com".
- **The user sees the redacted question,** and cannot get the original back from the gateway.

### What this does not prove

- **That no personal data reaches the model or a ticket.** It shows that these four forms, as tested,
  do not.
- **That logs hold no personal data.** Every gateway log line passes through redaction. The other
  services' logs were not changed: the executor logs Forgejo statuses, and retrieval does not log
  queries.

## Measurements

`make test-db` and the package tests, on 2026-11-15, all on made-up data:

- **Removed:** two email addresses; five phone numbers in German and US forms, with spaces, a
  slash, brackets and a hyphen; three IBANs, with and without spaces; and three card numbers, with
  spaces, hyphens or neither. The IBANs and card numbers are published test values.
- **Left as they were:** an image reference with a digest; ports, replica counts, CPU and memory; a
  version, a timestamp, a file mode and a cron schedule; two UUIDs including the all-zero one; an
  issue number and a 12-digit job number; `DE00 1234`; a card number with a bad checksum and one
  starting with 1; short numbers; and a jsonpath.
- **Idempotent:** a second pass changed nothing, and no part of the values was left.
- **In a turn:** a question with an email address and a phone number reached the model with both
  removed, and the page said so. A ticket with an IBAN and a card number was proposed with both
  removed. A ticket with a duplicate key was still refused. A log line with both was written redacted.

Each rule and each place was broken in turn, and a test failed every time:
- the question, the ticket fields, the duplicate-key guard and the log
- the IBAN checksum, the Luhn checksum, the phone number's digit count and the card's first digit

In the VM, `make test-adversarial` asks the stub's `echo` scenario with a made-up email address, phone
number, IBAN and card number. It checks that the proposal holds the four placeholders and none of the
values, and that the page said what was removed. It also checks that the copy of the gateway logged
none of the values during the turn. That last check would also pass if nothing were logged, so the
evidence for the logs is the unit test and the check that `logf` is the only way the gateway logs.

The VM results follow.
