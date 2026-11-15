package main

import (
	"path/filepath"
	"strings"
	"testing"

	"airgapai/gateway/approval"
)

// chain builds n valid rows with the approval core's own hash.
func chain(n int) []row {
	var rows []row
	prev := approval.Genesis
	for i := 1; i <= n; i++ {
		r := row{Seq: int64(i), AtMicros: 1794700000000000 + int64(i), Actor: "alice", Action: "propose", Decision: "allowed",
			Detail: `{"proposal":"p` + string(rune('0'+i)) + `"}`, Prev: prev}
		r.SHA = r.hash()
		rows = append(rows, r)
		prev = r.SHA
	}
	return rows
}

// rechain recomputes every hash from index i, as an attacker with the database would.
func rechain(rows []row, i int) {
	prev := approval.Genesis
	if i > 0 {
		prev = rows[i-1].SHA
	}
	for ; i < len(rows); i++ {
		rows[i].Prev = prev // first: the hash covers it
		rows[i].SHA = rows[i].hash()
		prev = rows[i].SHA
	}
}

func TestCheck(t *testing.T) {
	copied := chain(3)
	vm := chain(5)
	fresh, err := check(copied, vm)
	if err != nil || len(fresh) != 2 || fresh[0].Seq != 4 {
		t.Fatalf("honest growth: %v, %v", fresh, err)
	}
	if fresh, err := check(nil, chain(2)); err != nil || len(fresh) != 2 {
		t.Errorf("a first copy: %v, %v", fresh, err)
	}

	// Row 2 rewritten and every hash after it recomputed: the VM's own check still passes, the copy does not.
	forged := chain(5)
	forged[1].Detail = `{"proposal":"forged"}`
	rechain(forged, 1)
	prev := approval.Genesis
	for _, r := range forged {
		if r.Prev != prev || r.hash() != r.SHA {
			t.Fatal("the forged chain does not verify, so it tests nothing")
		}
		prev = r.SHA
	}
	if _, err := check(copied, forged); err == nil || !strings.Contains(err.Error(), "row 2 in the VM differs") {
		t.Errorf("a rewritten, rechained row: %v", err)
	}
	// Rows removed from the end.
	if _, err := check(copied, chain(2)); err == nil || !strings.Contains(err.Error(), "rows were removed") {
		t.Errorf("removed rows: %v", err)
	}
	// A new row that does not chain, or whose hash does not match.
	bad := chain(5)
	bad[3].Prev = strings.Repeat("a", 64)
	if _, err := check(copied, bad); err == nil || !strings.Contains(err.Error(), "row 4 does not chain") {
		t.Errorf("a new row off the chain: %v", err)
	}
	bad = chain(5)
	bad[4].Detail = `{"proposal":"x"}`
	if _, err := check(copied, bad); err == nil || !strings.Contains(err.Error(), "row 5's hash") {
		t.Errorf("a new row with a wrong hash: %v", err)
	}
	gap := append(chain(3), chain(5)[4])
	if _, err := check(copied, gap); err == nil || !strings.Contains(err.Error(), "row 5 follows row 3") {
		t.Errorf("a gap: %v", err)
	}
}

func TestAppendAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy.jsonl")
	rows := chain(4)
	if err := appendRows(path, rows[:2]); err != nil {
		t.Fatal(err)
	}
	if err := appendRows(path, rows[2:]); err != nil {
		t.Fatal(err)
	}
	got, err := load(path)
	if err != nil || len(got) != 4 || got[3] != rows[3] {
		t.Fatalf("loaded %v, %v", got, err)
	}
	if missing, err := load(filepath.Join(t.TempDir(), "none")); err != nil || missing != nil {
		t.Errorf("no copy yet: %v, %v", missing, err)
	}
}

func TestUpdatesQuoteValues(t *testing.T) {
	sql := updates([]row{{Seq: 2, Detail: `{"a":"it's"}`, Prev: "p", SHA: "s"}})
	if !strings.Contains(sql, `detail = '{"a":"it''s"}'`) || !strings.HasPrefix(sql, "BEGIN;") {
		t.Errorf("sql: %s", sql)
	}
}
