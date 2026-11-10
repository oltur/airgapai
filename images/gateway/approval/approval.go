// Package approval decides whether a side-effecting tool call may run. A model can only propose a
// call. A person confirms the proposal in an authenticated session separate from chat and gets a
// one-time token that the model never sees. Execution needs that token, the same user, the same tool,
// arguments with the same canonical hash, an unexpired confirmation and a role that still permits the
// tool. The token is consumed in the same transaction that writes the audit entry, so a call either
// runs once and is logged, or does not run. See ADR 0015.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Denied is returned when a request is refused. Reason is a fixed phrase, safe to log. Hint may name
// what the caller sent, so it is shown to the caller and never logged.
type Denied struct{ Reason, Hint string }

func (d *Denied) Error() string {
	if d.Hint != "" {
		return "denied: " + d.Reason + ": " + d.Hint
	}
	return "denied: " + d.Reason
}

// IsDenied reports whether err is a refusal, as opposed to a failure.
func IsDenied(err error) bool {
	var d *Denied
	return errors.As(err, &d)
}

// Roles resolves a user's current role, or "" for none. It is asked again at every step.
type Roles interface {
	Role(ctx context.Context, user string) (string, error)
}

// Tool is a side-effecting tool. Validate checks decoded, canonical arguments. Its errors are shown
// to the caller and not logged, because they may quote what the model wrote.
type Tool struct {
	Name     string
	Validate func(args map[string]any) error
}

// Source is a chunk the proposal drew on: its identity and hash, never its text.
type Source struct {
	Path        string `json:"path"`
	ChunkIndex  int    `json:"chunk_index"`
	ContentHash string `json:"content_sha256"`
}

// Proposal is what a model asked for. It carries no token.
type Proposal struct {
	ID         uuid.UUID
	User       string
	Tool       string
	Args       string // canonical JSON
	ArgsSHA256 string
	Sources    []Source
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// Execution is what the executor may do: exactly the confirmed tool and canonical arguments.
type Execution struct {
	ProposalID uuid.UUID
	User       string
	Tool       string
	Args       string
}

type Store struct {
	pool        *pgxpool.Pool
	roles       Roles
	tools       map[string]Tool
	now         func() time.Time
	ProposalTTL time.Duration // how long a proposal may wait for confirmation
	ConfirmTTL  time.Duration // how long a confirmation may wait for execution
}

func New(pool *pgxpool.Pool, roles Roles, tools []Tool, now func() time.Time) *Store {
	s := &Store{pool: pool, roles: roles, tools: map[string]Tool{}, now: now,
		ProposalTTL: 15 * time.Minute, ConfirmTTL: 5 * time.Minute}
	for _, t := range tools {
		s.tools[t.Name] = t
	}
	return s
}

// toolName is what the log may say about a tool name the caller sent: the name if it is a known
// tool, and otherwise only that it was not one, since the text came from the model.
func (s *Store) toolName(tool string) string {
	if _, ok := s.tools[tool]; ok {
		return tool
	}
	return "(unknown)"
}

// permitted resolves the user's role now and checks it against the permissions table, in tx.
func (s *Store) permitted(ctx context.Context, tx pgx.Tx, user, tool string) error {
	role, err := s.roles.Role(ctx, user)
	if err != nil {
		return err
	}
	if role == "" {
		return &Denied{Reason: "the user has no role"}
	}
	var ok bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM gateway.tool_permissions WHERE role = $1 AND tool = $2)",
		role, tool).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return &Denied{Reason: "the user's role may not use this tool"}
	}
	return nil
}

// decide runs f in a transaction that also writes its audit entry, and logs every outcome:
//   - allowed: f's work and the entry commit together, so if the entry fails, so does the work;
//   - denied: f's work is rolled back and the refusal is logged on its own;
//   - failed: f's work is rolled back and the failure is logged on its own, without its message,
//     which may hold data.
//
// The audit lock is taken first and held to the end, so decisions run one at a time: a Revoke cannot
// commit between an Execute's role check and its commit.
func (s *Store) decide(ctx context.Context, actor, action string, f func(tx pgx.Tx, detail map[string]any) error) error {
	detail := map[string]any{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", auditLock); err != nil {
		return err
	}
	ferr := f(tx, detail)
	if ferr == nil {
		if err := appendAudit(ctx, tx, s.now(), Entry{actor, action, "allowed", detail}); err != nil {
			return fmt.Errorf("audit write failed, so the %s did not happen: %w", action, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("audit commit failed, so the %s did not happen: %w", action, err)
		}
		return nil
	}
	_ = tx.Rollback(ctx)
	decision := "failed"
	var d *Denied
	if errors.As(ferr, &d) {
		decision, detail["reason"] = "denied", d.Reason
	}
	logTx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.Join(ferr, err)
	}
	defer logTx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	if err := appendAudit(ctx, logTx, s.now(), Entry{actor, action, decision, detail}); err != nil {
		return errors.Join(ferr, fmt.Errorf("logging the %s %s failed: %w", decision, action, err))
	}
	if err := logTx.Commit(ctx); err != nil {
		return errors.Join(ferr, err)
	}
	return ferr
}

// canonicalArgs returns the canonical text and its decoded form, or a refusal whose hint, not its
// reason, says what was wrong.
func canonicalArgs(raw []byte) (string, map[string]any, error) {
	text, decoded, err := canonical(raw)
	if err != nil {
		return "", nil, &Denied{Reason: "the arguments are not a single JSON object without duplicate keys", Hint: err.Error()}
	}
	return text, decoded, nil
}

func canonicalSources(sources []Source) (string, error) {
	if sources == nil {
		sources = []Source{}
	}
	raw, err := json.Marshal(map[string]any{"sources": sources})
	if err != nil {
		return "", err
	}
	return Canonical(raw)
}

// Propose records what a model asked for, if the user's role may use the tool and the arguments are
// valid. It is the only thing a model's tool call can do.
func (s *Store) Propose(ctx context.Context, user, tool string, rawArgs []byte, sources []Source) (Proposal, error) {
	p := Proposal{ID: uuid.New(), User: user, Tool: tool, Sources: sources}
	err := s.decide(ctx, user, "propose", func(tx pgx.Tx, detail map[string]any) error {
		detail["proposal"], detail["tool"] = p.ID.String(), s.toolName(tool)
		t, ok := s.tools[tool]
		if !ok {
			return &Denied{Reason: "unknown tool"}
		}
		if err := s.permitted(ctx, tx, user, tool); err != nil {
			return err
		}
		args, decoded, err := canonicalArgs(rawArgs)
		if err != nil {
			return err
		}
		if err := t.Validate(decoded); err != nil {
			return &Denied{Reason: "the arguments do not fit the tool", Hint: err.Error()}
		}
		src, err := canonicalSources(sources)
		if err != nil {
			return err
		}
		p.Args, p.ArgsSHA256 = args, Hash(args)
		p.CreatedAt = s.now().UTC().Truncate(time.Microsecond)
		p.ExpiresAt = p.CreatedAt.Add(s.ProposalTTL)
		detail["args_sha256"], detail["sources"] = p.ArgsSHA256, len(sources)
		_, err = tx.Exec(ctx, `INSERT INTO gateway.proposals (id, user_id, tool, args, args_sha256, sources, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, p.ID, user, tool, args, p.ArgsSHA256, src, p.CreatedAt, p.ExpiresAt)
		return err
	})
	return p, err
}

// Proposal returns one of user's own proposals, for the confirmation page to render. Another user's
// proposal is refused as if it did not exist, so its arguments are never shown to them.
func (s *Store) Proposal(ctx context.Context, user string, id uuid.UUID) (Proposal, error) {
	p := Proposal{ID: id}
	var src string
	err := s.pool.QueryRow(ctx, `SELECT user_id, tool, args, args_sha256, sources, created_at, expires_at
		FROM gateway.proposals WHERE id = $1 AND user_id = $2`, id, user).Scan(&p.User, &p.Tool, &p.Args, &p.ArgsSHA256, &src, &p.CreatedAt, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, &Denied{Reason: "unknown proposal"}
	}
	if err != nil {
		return Proposal{}, err
	}
	var wrapped struct {
		Sources []Source `json:"sources"`
	}
	err = json.Unmarshal([]byte(src), &wrapped)
	p.Sources = wrapped.Sources
	return p, err
}

// Confirm records that user confirmed their own, unexpired, unconfirmed proposal, and returns the
// one-time token for executing it. Call it only from the authenticated confirmation session, never
// from anything a model can reach. The token is returned once and only its hash is stored.
func (s *Store) Confirm(ctx context.Context, user string, proposalID uuid.UUID) (string, error) {
	var token string
	err := s.decide(ctx, user, "confirm", func(tx pgx.Tx, detail map[string]any) error {
		detail["proposal"] = proposalID.String()
		var owner, tool string
		var expires time.Time
		// No row lock, which would need UPDATE on proposals: the unique proposal_id on confirmations
		// is what stops a second confirmation.
		err := tx.QueryRow(ctx, "SELECT user_id, tool, expires_at FROM gateway.proposals WHERE id = $1",
			proposalID).Scan(&owner, &tool, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Denied{Reason: "unknown proposal"}
		}
		if err != nil {
			return err
		}
		switch {
		case owner != user:
			return &Denied{Reason: "the proposal belongs to another user"}
		case !s.now().Before(expires):
			return &Denied{Reason: "the proposal has expired"}
		}
		if err := s.permitted(ctx, tx, user, tool); err != nil {
			return err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token = base64.RawURLEncoding.EncodeToString(raw)
		now := s.now().UTC().Truncate(time.Microsecond)
		_, err = tx.Exec(ctx, `INSERT INTO gateway.confirmations (token_sha256, proposal_id, user_id, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5)`, Hash(token), proposalID, user, now, now.Add(s.ConfirmTTL))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return &Denied{Reason: "the proposal is already confirmed"}
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Execute consumes a confirmation and returns what may run, if every binding holds: the token exists
// and is unused and unexpired, it was issued to this user for this tool, the arguments have the
// confirmed hash, and the user's role permits the tool now. The caller must run Execution's canonical
// arguments, not its own.
func (s *Store) Execute(ctx context.Context, token, user, tool string, rawArgs []byte) (Execution, error) {
	var ex Execution
	err := s.decide(ctx, user, "execute", func(tx pgx.Tx, detail map[string]any) error {
		tokenHash := Hash(token)
		detail["confirmation"], detail["tool"] = tokenHash[:16], s.toolName(tool)
		args, _, err := canonicalArgs(rawArgs)
		if err != nil {
			return err
		}
		detail["args_sha256"] = Hash(args)
		var owner, ptool, phash string
		var proposal uuid.UUID
		var expires time.Time
		var consumed *time.Time
		// The decision lock already serialises executions; the row lock keeps a token single-use even
		// if that lock is ever narrowed.
		err = tx.QueryRow(ctx, `SELECT c.user_id, c.expires_at, c.consumed_at, p.id, p.tool, p.args_sha256
			FROM gateway.confirmations c JOIN gateway.proposals p ON p.id = c.proposal_id
			WHERE c.token_sha256 = $1 FOR UPDATE OF c`, tokenHash).Scan(&owner, &expires, &consumed, &proposal, &ptool, &phash)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Denied{Reason: "unknown confirmation"}
		}
		if err != nil {
			return err
		}
		detail["proposal"] = proposal.String()
		switch {
		case consumed != nil:
			return &Denied{Reason: "the confirmation was already used"}
		case !s.now().Before(expires):
			return &Denied{Reason: "the confirmation has expired"}
		case owner != user:
			return &Denied{Reason: "the confirmation belongs to another user"}
		case ptool != tool:
			return &Denied{Reason: "the confirmation is for another tool"}
		case phash != Hash(args):
			return &Denied{Reason: "the arguments differ from the confirmed proposal"}
		}
		if err := s.permitted(ctx, tx, user, tool); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE gateway.confirmations SET consumed_at = $1 WHERE token_sha256 = $2",
			s.now().UTC(), tokenHash); err != nil {
			return err
		}
		ex = Execution{ProposalID: proposal, User: user, Tool: tool, Args: args}
		return nil
	})
	if err != nil {
		return Execution{}, err
	}
	return ex, nil
}

// RecordResult logs what happened when an execution ran. It refuses a proposal whose confirmation was
// never consumed, so no result can be logged for a call that was not allowed. The execution was logged
// before it ran, so a failure here loses only the outcome, which is the stated residual risk.
func (s *Store) RecordResult(ctx context.Context, ex Execution, outcome string, detail map[string]any) error {
	entry := maps.Clone(detail)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["proposal"], entry["outcome"] = ex.ProposalID.String(), outcome
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	var allowed bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM gateway.confirmations WHERE proposal_id = $1 AND consumed_at IS NOT NULL)",
		ex.ProposalID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return &Denied{Reason: "no allowed execution for this proposal"}
	}
	if err := appendAudit(ctx, tx, s.now(), Entry{"executor", "result", outcome, entry}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Grant and Revoke change which roles may use which tools. Each change is logged as the admin's.
func (s *Store) Grant(ctx context.Context, admin, role, tool string) error {
	return s.decide(ctx, admin, "grant", func(tx pgx.Tx, detail map[string]any) error {
		detail["role"], detail["tool"] = role, s.toolName(tool)
		if _, ok := s.tools[tool]; !ok {
			return &Denied{Reason: "unknown tool"}
		}
		_, err := tx.Exec(ctx, "INSERT INTO gateway.tool_permissions (role, tool) VALUES ($1, $2) ON CONFLICT DO NOTHING", role, tool)
		return err
	})
}

func (s *Store) Revoke(ctx context.Context, admin, role, tool string) error {
	return s.decide(ctx, admin, "revoke", func(tx pgx.Tx, detail map[string]any) error {
		detail["role"], detail["tool"] = role, s.toolName(tool)
		_, err := tx.Exec(ctx, "DELETE FROM gateway.tool_permissions WHERE role = $1 AND tool = $2", role, tool)
		return err
	})
}
