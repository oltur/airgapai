package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Canonical returns one fixed text for a JSON object: keys sorted and no insignificant whitespace, so
// key order and spacing do not change the hash. Numbers are kept as written, so 1 and 1.0 hash
// differently, and a call that rewrites a number is refused: that fails closed. Duplicate keys are
// refused, because a reader and a parser could each pick a different one.
func Canonical(raw []byte) (string, error) {
	text, _, err := canonical(raw)
	return text, err
}

// canonical also returns the decoded object, so callers need not parse the text again.
func canonical(raw []byte) (string, map[string]any, error) {
	if err := noDuplicateKeys(raw); err != nil {
		return "", nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return "", nil, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	if v == nil {
		return "", nil, errors.New("arguments are not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", nil, errors.New("arguments have trailing data")
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // map keys are sorted by encoding/json
		return "", nil, err
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), v, nil
}

// Hash is the SHA-256 of a canonical text, in hex.
func Hash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func noDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				k := key.(string)
				if seen[k] {
					return fmt.Errorf("duplicate key %q", k)
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}
