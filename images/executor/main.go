// Executor is the only part of the gateway that holds the ticket credential. It runs a tool call only
// through approval.Execute, which consumes the user's one-time confirmation, and it files exactly the
// canonical arguments that were confirmed. See ADR 0016.
//
// It serves POST /execute and GET /ready. Its subcommands are for an administrator using kubectl
// exec, which already implies control of the cluster: grant, revoke and verify, plus propose, which
// stands in for the model's tool call until the chat loop exists. There is no confirm command: a
// confirmation is made only on the gateway's page, by a signed-in user, and its token stays there.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"airgapai/gateway/approval"

	"github.com/jackc/pgx/v5/pgxpool"
)

type executeRequest struct {
	Token string          `json:"token"`
	User  string          `json:"user"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args"`
}

type server struct {
	store   *approval.Store
	forgejo *forgejo
}

// ticketBody is what the issue says: where it came from, then the confirmed body. The provenance comes
// first, written by the executor, so text in the body cannot pose as it.
func ticketBody(ex approval.Execution, args map[string]string) string {
	return fmt.Sprintf("Filed for %s after their confirmation of proposal %s. Priority: %s.\n\n---\n\n%s",
		ex.User, ex.ProposalID, args["priority"], args["body"])
}

// finishTimeout bounds the work after a confirmation is consumed.
const finishTimeout = 30 * time.Second

// run executes one confirmed call. A refusal is returned as such; the token is consumed only if the
// call is allowed, and the outcome is logged either way.
func (s *server) run(ctx context.Context, r executeRequest) (int64, error) {
	ex, err := s.store.Execute(ctx, r.Token, r.User, r.Tool, r.Args)
	if err != nil {
		return 0, err
	}
	// The token is used up now, so finish even if the caller goes away: a cancelled request must not
	// leave a consumed confirmation with no attempt and no result.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	record := func(outcome string, detail map[string]any) {
		if err := s.store.RecordResult(ctx, ex, outcome, detail); err != nil {
			log.Printf("proposal %s %s, but its result was not logged: %v", ex.ProposalID, outcome, err)
		}
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(ex.Args), &args); err != nil {
		record("failed", map[string]any{"stage": "arguments"})
		return 0, err
	}
	number, err := s.forgejo.createIssue(ctx, args["title"], ticketBody(ex, args))
	var refused *refusedError
	switch {
	case errors.As(err, &refused):
		record("failed", map[string]any{"stage": "forgejo", "status": refused.status})
		return 0, err
	case err != nil:
		// The request may have reached Forgejo and created the issue before the answer was lost.
		record("unknown", map[string]any{"stage": "forgejo"})
		return 0, err
	}
	record("succeeded", map[string]any{"issue": number})
	return number, nil
}

func (s *server) handleExecute(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var r executeRequest
	dec := json.NewDecoder(io.LimitReader(req.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	number, err := s.run(req.Context(), r)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case approval.IsDenied(err):
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"denied": err.Error()})
	case err != nil:
		log.Printf("execute: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "execution failed"})
	default:
		_ = json.NewEncoder(w).Encode(map[string]int64{"issue": number})
	}
}

func (s *server) handleReady(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	fj := newForgejo(getenv("FORGEJO_URL", "http://forgejo.forgejo.svc:3000"), os.Getenv("FORGEJO_TOKEN"),
		getenv("TICKETS_ORG", "maintenance"), getenv("TICKETS_REPO", "tickets"))
	store := approval.New(pool, fj, []approval.Tool{approval.CreateTicket}, time.Now)
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		if err := admin(ctx, store, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	serve(store, fj)
}

func serve(store *approval.Store, fj *forgejo) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("listen", ":8080", "address to serve on")
	_ = fs.Parse(os.Args[min(2, len(os.Args)):])
	s := &server{store: store, forgejo: fj}
	mux := http.NewServeMux()
	mux.HandleFunc("/execute", s.handleExecute)
	mux.HandleFunc("/ready", s.handleReady)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// admin runs one administrator command. Output is JSON on stdout.
func admin(ctx context.Context, store *approval.Store, args []string) error {
	const actor = "cluster-admin"
	out := json.NewEncoder(os.Stdout)
	need := func(n int, usage string) error {
		if len(args) != n {
			return errors.New("usage: executor " + usage)
		}
		return nil
	}
	switch args[0] {
	case "grant", "revoke":
		if err := need(3, args[0]+" ROLE TOOL"); err != nil {
			return err
		}
		if args[0] == "grant" {
			return store.Grant(ctx, actor, args[1], args[2])
		}
		return store.Revoke(ctx, actor, args[1], args[2])
	case "verify":
		bad, rows, err := store.VerifyAudit(ctx)
		if err != nil {
			return err
		}
		_ = out.Encode(map[string]int64{"rows": rows, "first_bad_row": bad})
		if bad != 0 {
			return fmt.Errorf("the audit chain breaks at row %d", bad)
		}
		return nil
	case "propose": // stands in for the model until the chat loop exists
		if err := need(4, "propose USER TOOL ARGS-JSON"); err != nil {
			return err
		}
		p, err := store.Propose(ctx, args[1], args[2], []byte(args[3]), nil)
		if err != nil {
			return err
		}
		return out.Encode(map[string]string{"proposal": p.ID.String(), "args": p.Args, "args_sha256": p.ArgsSHA256})
	}
	return errors.New("commands: serve, grant, revoke, verify, propose")
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
