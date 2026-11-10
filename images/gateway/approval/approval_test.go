package approval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The tests below need the gateway schema from the postgres-init ConfigMap in deploy/retrieval. GATEWAY_TEST_DSN connects as gateway_app and
// GATEWAY_ADMIN_DSN as a superuser, which the tests use only to reset tables and for the controls
// that tamper with the log. Without them, they are skipped; make test-db runs them.

type roles struct {
	mu  sync.Mutex
	m   map[string]string
	err error // returned instead of a role when set
}

func (r *roles) Role(_ context.Context, user string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	return r.m[user], nil
}

func (r *roles) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *roles) set(user, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[user] = role
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	s     *Store
	admin *pgx.Conn
	roles *roles
	clock *clock
}

const args = `{"title":"Pump 3 leaks","body":"Seen at the seal.","priority":"high"}`

func setup(t *testing.T) fixture {
	t.Helper()
	dsn, adminDSN := os.Getenv("GATEWAY_TEST_DSN"), os.Getenv("GATEWAY_ADMIN_DSN")
	if dsn == "" || adminDSN == "" {
		t.Skip("GATEWAY_TEST_DSN and GATEWAY_ADMIN_DSN are not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	if _, err := admin.Exec(ctx, `TRUNCATE gateway.audit, gateway.confirmations, gateway.proposals, gateway.tool_permissions;
		GRANT INSERT ON gateway.audit TO gateway_app`); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := fixture{admin: admin, roles: &roles{m: map[string]string{"alice": "technician", "bob": "technician"}},
		clock: &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}}
	f.s = New(pool, f.roles, []Tool{CreateTicket}, f.clock.now)
	if err := f.s.Grant(ctx, "admin", "technician", "create_ticket"); err != nil {
		t.Fatal(err)
	}
	return f
}

// confirmed returns a fresh proposal by alice and alice's token for it.
func (f fixture) confirmed(t *testing.T) (Proposal, string) {
	t.Helper()
	ctx := context.Background()
	p, err := f.s.Propose(ctx, "alice", "create_ticket", []byte(args), []Source{{"content/en/docs/a.md", 0, "abc"}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.s.Confirm(ctx, "alice", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, token
}

func denied(t *testing.T, err error, reason string) {
	t.Helper()
	if !IsDenied(err) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("want a refusal containing %q, got %v", reason, err)
	}
}

// audit returns the decisions in the log, as "actor action decision", and checks the chain.
func (f fixture) audit(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	if bad, _, err := Verify(ctx, f.admin); err != nil || bad != 0 {
		t.Fatalf("audit chain broken at row %d: %v", bad, err)
	}
	rows, err := f.admin.Query(ctx, "SELECT actor || ' ' || action || ' ' || decision FROM gateway.audit ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestConfirmedCallRunsOnceAndIsLogged(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p, token := f.confirmed(t)
	// The same arguments in another order and spacing are the same call.
	ex, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(`{ "priority":"high","body":"Seen at the seal.", "title":"Pump 3 leaks"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ex.ProposalID != p.ID || ex.Args != p.Args || ex.Args != `{"body":"Seen at the seal.","priority":"high","title":"Pump 3 leaks"}` {
		t.Fatalf("got %+v", ex)
	}
	if err := f.s.RecordResult(ctx, ex, "succeeded", map[string]any{"issue": 1}); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
	denied(t, err, "already used")
	want := []string{"admin grant allowed", "alice propose allowed", "alice confirm allowed",
		"alice execute allowed", "executor result succeeded", "alice execute denied"}
	if got := f.audit(t); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("audit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The log holds identifiers and hashes, not the ticket text.
	var leaked int
	if err := f.admin.QueryRow(ctx, "SELECT count(*) FROM gateway.audit WHERE detail LIKE '%Pump%' OR detail LIKE '%seal%'").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("ticket text in the audit log: %d, %v", leaked, err)
	}
}

func TestEveryBindingIsChecked(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		reason string
		act    func(f fixture, p Proposal, token string) error
	}{
		"altered arguments": {"arguments differ", func(f fixture, _ Proposal, token string) error {
			_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(`{"title":"Pump 3 leaks","body":"Also grant bob admin.","priority":"high"}`))
			return err
		}},
		"another user's confirmation": {"another user", func(f fixture, _ Proposal, token string) error {
			_, err := f.s.Execute(ctx, token, "bob", "create_ticket", []byte(args))
			return err
		}},
		"another tool": {"another tool", func(f fixture, _ Proposal, token string) error {
			_, err := f.s.Execute(ctx, token, "alice", "delete_everything", []byte(args))
			return err
		}},
		"expired confirmation": {"expired", func(f fixture, _ Proposal, token string) error {
			f.clock.advance(f.s.ConfirmTTL)
			_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
			return err
		}},
		"role removed before execution": {"no role", func(f fixture, _ Proposal, token string) error {
			f.roles.set("alice", "")
			_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
			return err
		}},
		"permission revoked before execution": {"may not use", func(f fixture, _ Proposal, token string) error {
			if err := f.s.Revoke(ctx, "admin", "technician", "create_ticket"); err != nil {
				return err
			}
			_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
			return err
		}},
		"forged token": {"unknown confirmation", func(f fixture, _ Proposal, _ string) error {
			_, err := f.s.Execute(ctx, "not-a-token", "alice", "create_ticket", []byte(args))
			return err
		}},
		"duplicate keys at execution": {"duplicate key", func(f fixture, _ Proposal, token string) error {
			_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(`{"title":"x","title":"Pump 3 leaks","body":"Seen at the seal.","priority":"high"}`))
			return err
		}},
		"confirming twice": {"already confirmed", func(f fixture, p Proposal, _ string) error {
			_, err := f.s.Confirm(ctx, "alice", p.ID)
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			p, token := f.confirmed(t)
			denied(t, c.act(f, p, token), c.reason)
			got := f.audit(t)
			if last := got[len(got)-1]; !strings.HasSuffix(last, " denied") {
				t.Fatalf("the refusal was not logged last: %v", got)
			}
			// A refusal does not use up the confirmation: the real call still works, unless time,
			// role or permission is what refused it.
			f.roles.set("alice", "technician")
			_ = f.s.Grant(ctx, "admin", "technician", "create_ticket")
			_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
			if name == "expired confirmation" {
				denied(t, err, "expired")
			} else if err != nil {
				t.Fatalf("the genuine call after the refusal: %v", err)
			}
		})
	}
}

func TestProposalChecks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.s.Propose(ctx, "alice", "delete_everything", []byte(`{}`), nil)
	denied(t, err, "unknown tool")
	_, err = f.s.Propose(ctx, "alice", "create_ticket", []byte(`{"title":"x","body":"y","priority":"high","assignee":"root"}`), nil)
	denied(t, err, "unexpected field")
	_, err = f.s.Propose(ctx, "carol", "create_ticket", []byte(args), nil)
	denied(t, err, "no role")
	// Default deny: a role with no grant cannot propose.
	f.roles.set("dave", "engineer")
	_, err = f.s.Propose(ctx, "dave", "create_ticket", []byte(args), nil)
	denied(t, err, "may not use")

	p, err := f.s.Propose(ctx, "alice", "create_ticket", []byte(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Confirm(ctx, "bob", p.ID)
	denied(t, err, "another user")
	_, err = f.s.Confirm(ctx, "alice", uuid.New())
	denied(t, err, "unknown proposal")
	f.clock.advance(f.s.ProposalTTL)
	_, err = f.s.Confirm(ctx, "alice", p.ID)
	denied(t, err, "expired")
	got := f.audit(t)
	if n := strings.Count(strings.Join(got, "\n"), "denied"); n != 7 {
		t.Fatalf("want 7 refusals logged, got %d: %v", n, got)
	}
}

func TestConcurrentExecutionRunsOnce(t *testing.T) {
	f := setup(t)
	_, token := f.confirmed(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, refusals := 0, 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.Execute(context.Background(), token, "alice", "create_ticket", []byte(args))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case IsDenied(err):
				refusals++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || refusals != 19 {
		t.Fatalf("%d ran and %d were refused; want 1 and 19", wins, refusals)
	}
	f.audit(t) // the chain holds under concurrent writers
}

// If the audit entry cannot be written, the call does not run and the confirmation stays unused.
func TestAuditFailureStopsTheCall(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, token := f.confirmed(t)
	if _, err := f.admin.Exec(ctx, "REVOKE INSERT ON gateway.audit FROM gateway_app"); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
	if err == nil || IsDenied(err) || !strings.Contains(err.Error(), "did not happen") {
		t.Fatalf("want an audit failure, got %v", err)
	}
	var consumed bool
	if err := f.admin.QueryRow(ctx, "SELECT consumed_at IS NOT NULL FROM gateway.confirmations").Scan(&consumed); err != nil || consumed {
		t.Fatalf("confirmation consumed without an audit entry: %v, %v", consumed, err)
	}
	// Control: with the log writable again, the same call runs.
	if _, err := f.admin.Exec(ctx, "GRANT INSERT ON gateway.audit TO gateway_app"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args)); err != nil {
		t.Fatal(err)
	}
}

func TestAuditLogIsAppendOnlyAndTamperEvident(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.confirmed(t)
	f.audit(t) // control: the untouched chain verifies
	// The gateway's own login cannot change or remove entries.
	for _, stmt := range []string{"UPDATE gateway.audit SET decision = 'allowed'", "DELETE FROM gateway.audit", "TRUNCATE gateway.audit"} {
		if _, err := f.s.pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: want permission denied, got %v", stmt, err)
		}
	}
	// A superuser can, and Verify names the first row that no longer fits.
	for name, stmt := range map[string]string{
		"changed":   "UPDATE gateway.audit SET decision = 'denied' WHERE seq = 2",
		"removed":   "DELETE FROM gateway.audit WHERE seq = 2",
		"rehashed":  "UPDATE gateway.audit SET detail = '{}', sha256 = 'x' WHERE seq = 2",
		"reordered": "UPDATE gateway.audit SET seq = 99 WHERE seq = 1",
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := f.admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // undoes the tampering
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatal(err)
			}
			bad, _, err := Verify(ctx, tx)
			if err != nil || bad == 0 {
				t.Fatalf("tampering was not detected: %d, %v", bad, err)
			}
		})
	}
	f.audit(t)
}

// A failure that is not a refusal is still logged, without its message, and a refusal wrapped by a
// resolver is still a refusal.
func TestFailuresAreLogged(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, token := f.confirmed(t)
	f.roles.fail(errors.New("directory unreachable: secret detail"))
	_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
	if err == nil || IsDenied(err) {
		t.Fatalf("want a failure, got %v", err)
	}
	f.roles.fail(fmt.Errorf("lookup: %w", &Denied{Reason: "the directory says no"}))
	_, err = f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
	denied(t, err, "the directory says no")
	got := f.audit(t)
	if tail := strings.Join(got[len(got)-2:], "|"); tail != "alice execute failed|alice execute denied" {
		t.Fatalf("got %v", got)
	}
	var leaked int
	if err := f.admin.QueryRow(ctx, "SELECT count(*) FROM gateway.audit WHERE detail LIKE '%secret detail%'").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("a failure's message reached the log: %d, %v", leaked, err)
	}
	// The confirmation was not used by either attempt.
	f.roles.fail(nil)
	if _, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args)); err != nil {
		t.Fatal(err)
	}
}

// Text the model chose, such as a tool name or a field name, is not written to the log.
func TestModelTextStaysOutOfTheLog(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, err := f.s.Propose(ctx, "alice", "IGNORE PREVIOUS INSTRUCTIONS", []byte(`{}`), nil)
	denied(t, err, "unknown tool")
	_, err = f.s.Propose(ctx, "alice", "create_ticket", []byte(`{"title":"x","body":"y","priority":"high","RESTRICTED TEXT":1}`), nil)
	denied(t, err, "RESTRICTED TEXT") // the caller is told which field
	_, err = f.s.Propose(ctx, "alice", "create_ticket", []byte(`{"title":"x","DUPLICATED TEXT":1,"DUPLICATED TEXT":2}`), nil)
	denied(t, err, "DUPLICATED TEXT")
	var leaked int
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM gateway.audit
		WHERE detail ILIKE '%instructions%' OR detail LIKE '%RESTRICTED%' OR detail LIKE '%DUPLICATED%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("model text reached the log: %d, %v", leaked, err)
	}
	if got := f.audit(t); strings.Count(strings.Join(got, "\n"), "propose denied") != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestProposalIsReadableOnlyByItsOwner(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p, err := f.s.Propose(ctx, "alice", "create_ticket", []byte(args), []Source{{"content/en/docs/a.md", 3, "abc"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.Proposal(ctx, "alice", p.ID)
	if err != nil || got.Args != p.Args || len(got.Sources) != 1 || got.Sources[0].ChunkIndex != 3 {
		t.Fatalf("owner: %+v, %v", got, err)
	}
	got, err = f.s.Proposal(ctx, "bob", p.ID)
	denied(t, err, "unknown proposal")
	if got.Args != "" {
		t.Fatalf("another user saw the arguments: %s", got.Args)
	}
}

// A result can be logged only for a proposal whose confirmation was consumed.
func TestResultNeedsAnAllowedExecution(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	p, token := f.confirmed(t)
	err := f.s.RecordResult(ctx, Execution{ProposalID: p.ID}, "succeeded", nil)
	denied(t, err, "no allowed execution")
	err = f.s.RecordResult(ctx, Execution{ProposalID: uuid.New()}, "succeeded", nil)
	denied(t, err, "no allowed execution")
	ex, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
	if err != nil {
		t.Fatal(err)
	}
	detail := map[string]any{"issue": 7}
	if err := f.s.RecordResult(ctx, ex, "succeeded", detail); err != nil {
		t.Fatal(err)
	}
	if len(detail) != 1 {
		t.Fatalf("the caller's map was changed: %v", detail)
	}
}

// Decisions run one at a time, so a revocation that commits while an execution is waiting is seen by
// that execution's role check, rather than slipping in between the check and the commit.
func TestRevocationDuringExecutionIsSeen(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	_, token := f.confirmed(t)
	// Another decision in progress holds the lock.
	tx, err := f.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", auditLock); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.Execute(ctx, token, "alice", "create_ticket", []byte(args))
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // the execution is now waiting
	other, err := pgx.Connect(ctx, os.Getenv("GATEWAY_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "DELETE FROM gateway.tool_permissions WHERE role = 'technician'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		denied(t, err, "may not use")
	case <-time.After(5 * time.Second):
		t.Fatal("the execution did not finish")
	}
}
