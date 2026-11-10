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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Denied is returned when a request is refused. Reason is safe to show the user and is logged.
type Denied struct{ Reason string }

func (d *Denied) Error() string { return "denied: " + d.Reason }

// IsDenied reports whether err is a refusal, as opposed to a failure.
func IsDenied(err error) bool {
	var d *Denied
	return errors.As(err, &d)
}

// Roles resolves a user's current role, or "" for none. It is asked again at every step.
type Roles interface {
	Role(ctx context.Context, user string) (string, error)
}

// Tool is a side-effecting tool. Validate checks decoded, canonical arguments.
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

// permitted resolves the user's role now and checks it against the permissions table, in tx.
func (s *Store) permitted(ctx context.Context, tx pgx.Tx, user, tool string) (string, error) {
	role, err := s.roles.Role(ctx, user)
	if err != nil {
		return "", err
	}
	if role == "" {
		return "", &Denied{"the user has no role"}
	}
	var ok bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM gateway.tool_permissions WHERE role = $1 AND tool = $2)",
		role, tool).Scan(&ok); err != nil {
		return "", err
	}
	if !ok {
		return "", &Denied{fmt.Sprintf("role %q may not use %s", role, tool)}
	}
	return role, nil
}

// decide runs f in a transaction that also writes its audit entry. On a refusal, f's work is rolled
// back and the refusal is logged in a transaction of its own. If the audit write fails, so does the
// call. f fills in the detail; a refusal's reason is added to it.
func (s *Store) decide(ctx context.Context, actor, action string, f func(tx pgx.Tx, detail map[string]any) error) error {
	detail := map[string]any{}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after commit
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
	if !IsDenied(ferr) {
		return ferr
	}
	detail["reason"] = ferr.(*Denied).Reason
	logTx, err := s.pool.Begin(ctx)
	if err != nil {
		return errors.Join(ferr, err)
	}
	defer logTx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	if err := appendAudit(ctx, logTx, s.now(), Entry{actor, action, "denied", detail}); err != nil {
		return errors.Join(ferr, fmt.Errorf("logging the refusal failed: %w", err))
	}
	if err := logTx.Commit(ctx); err != nil {
		return errors.Join(ferr, err)
	}
	return ferr
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
		detail["proposal"], detail["tool"] = p.ID.String(), tool
		t, ok := s.tools[tool]
		if !ok {
			return &Denied{"unknown tool"}
		}
		if _, err := s.permitted(ctx, tx, user, tool); err != nil {
			return err
		}
		args, err := Canonical(rawArgs)
		if err != nil {
			return &Denied{err.Error()}
		}
		var decoded map[string]any
		dec := json.NewDecoder(stringsReader(args))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		if err := t.Validate(decoded); err != nil {
			return &Denied{"invalid arguments: " + err.Error()}
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

// Proposal returns a stored proposal, for the confirmation page to render.
func (s *Store) Proposal(ctx context.Context, id uuid.UUID) (Proposal, error) {
	p := Proposal{ID: id}
	var src string
	err := s.pool.QueryRow(ctx, `SELECT user_id, tool, args, args_sha256, sources, created_at, expires_at
		FROM gateway.proposals WHERE id = $1`, id).Scan(&p.User, &p.Tool, &p.Args, &p.ArgsSHA256, &src, &p.CreatedAt, &p.ExpiresAt)
	if err != nil {
		return p, err
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
			return &Denied{"unknown proposal"}
		}
		if err != nil {
			return err
		}
		switch {
		case owner != user:
			return &Denied{"the proposal belongs to another user"}
		case !s.now().Before(expires):
			return &Denied{"the proposal has expired"}
		}
		if _, err := s.permitted(ctx, tx, user, tool); err != nil {
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
			return &Denied{"the proposal is already confirmed"}
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
		detail["confirmation"], detail["tool"] = tokenHash[:16], tool
		args, err := Canonical(rawArgs)
		if err != nil {
			return &Denied{err.Error()}
		}
		detail["args_sha256"] = Hash(args)
		var owner, ptool, phash string
		var proposal uuid.UUID
		var expires time.Time
		var consumed *time.Time
		err = tx.QueryRow(ctx, `SELECT c.user_id, c.expires_at, c.consumed_at, p.id, p.tool, p.args_sha256
			FROM gateway.confirmations c JOIN gateway.proposals p ON p.id = c.proposal_id
			WHERE c.token_sha256 = $1 FOR UPDATE OF c`, tokenHash).Scan(&owner, &expires, &consumed, &proposal, &ptool, &phash)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Denied{"unknown confirmation"}
		}
		if err != nil {
			return err
		}
		detail["proposal"] = proposal.String()
		switch {
		case consumed != nil:
			return &Denied{"the confirmation was already used"}
		case !s.now().Before(expires):
			return &Denied{"the confirmation has expired"}
		case owner != user:
			return &Denied{"the confirmation belongs to another user"}
		case ptool != tool:
			return &Denied{"the confirmation is for another tool"}
		case phash != Hash(args):
			return &Denied{"the arguments differ from the confirmed proposal"}
		}
		if _, err := s.permitted(ctx, tx, user, tool); err != nil {
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

// RecordResult logs what happened when an execution ran. The execution was logged before it ran, so
// a failure here loses only the outcome, which is the stated residual risk.
func (s *Store) RecordResult(ctx context.Context, ex Execution, outcome string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["proposal"], detail["outcome"] = ex.ProposalID.String(), outcome
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	if err := appendAudit(ctx, tx, s.now(), Entry{"executor", "result", outcome, detail}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Grant and Revoke change which roles may use which tools. Each change is logged as the admin's.
func (s *Store) Grant(ctx context.Context, admin, role, tool string) error {
	return s.decide(ctx, admin, "grant", func(tx pgx.Tx, detail map[string]any) error {
		detail["role"], detail["tool"] = role, tool
		if _, ok := s.tools[tool]; !ok {
			return &Denied{"unknown tool"}
		}
		_, err := tx.Exec(ctx, "INSERT INTO gateway.tool_permissions (role, tool) VALUES ($1, $2) ON CONFLICT DO NOTHING", role, tool)
		return err
	})
}

func (s *Store) Revoke(ctx context.Context, admin, role, tool string) error {
	return s.decide(ctx, admin, "revoke", func(tx pgx.Tx, detail map[string]any) error {
		detail["role"], detail["tool"] = role, tool
		_, err := tx.Exec(ctx, "DELETE FROM gateway.tool_permissions WHERE role = $1 AND tool = $2", role, tool)
		return err
	})
}
