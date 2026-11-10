package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Entry is one decision in the audit log. Detail holds identifiers and hashes, never document text.
type Entry struct {
	Actor    string
	Action   string
	Decision string
	Detail   map[string]any
}

// One lock for the whole log, held to the end of the writing transaction, so rows are chained in order.
const auditLock = 7_100_002

// genesis is the previous hash of the first row.
const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// rowHash covers the previous row's hash and every field of this row.
func rowHash(prev string, seq int64, at time.Time, e Entry, detail string) string {
	fields, _ := json.Marshal([]any{seq, at.UTC().Format(time.RFC3339Nano), e.Actor, e.Action, e.Decision, detail})
	return Hash(prev + "\n" + string(fields))
}

// appendAudit adds an entry in the caller's transaction. If it fails, the transaction must fail too.
func appendAudit(ctx context.Context, tx pgx.Tx, at time.Time, e Entry) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", auditLock); err != nil {
		return err
	}
	var seq int64
	prev := genesis
	err := tx.QueryRow(ctx, "SELECT seq, sha256 FROM gateway.audit ORDER BY seq DESC LIMIT 1").Scan(&seq, &prev)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	seq++
	raw, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	detail, err := Canonical(raw)
	if err != nil {
		return err
	}
	at = at.UTC().Truncate(time.Microsecond) // what timestamptz keeps
	_, err = tx.Exec(ctx, `INSERT INTO gateway.audit (seq, at, actor, action, decision, detail, prev_sha256, sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		seq, at, e.Actor, e.Action, e.Decision, detail, prev, rowHash(prev, seq, at, e, detail))
	return err
}

// Verify walks the whole log and returns the first row whose chain does not hold, or 0 if all do.
// It shows that rows were not changed, removed or reordered after the fact, as long as the last
// hash is also kept somewhere the database cannot reach. It is tamper-evident, not tamper-proof.
func Verify(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (int64, int64, error) {
	rows, err := q.Query(ctx, "SELECT seq, at, actor, action, decision, detail, prev_sha256, sha256 FROM gateway.audit ORDER BY seq")
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	prev, want, count := genesis, int64(1), int64(0)
	for rows.Next() {
		var seq int64
		var at time.Time
		var e Entry
		var detail, rowPrev, sum string
		if err := rows.Scan(&seq, &at, &e.Actor, &e.Action, &e.Decision, &detail, &rowPrev, &sum); err != nil {
			return 0, count, err
		}
		if seq != want || rowPrev != prev || rowHash(prev, seq, at, e, detail) != sum {
			return seq, count, nil
		}
		prev, want, count = sum, want+1, count+1
	}
	if err := rows.Err(); err != nil {
		return 0, count, fmt.Errorf("reading the audit log: %w", err)
	}
	return 0, count, nil
}
