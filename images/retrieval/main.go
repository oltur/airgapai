// Retrieval answers one question: which chunks of the corpus are nearest to this query, for this
// reader role. Row-level security in the database decides what each role may see (ADR 0012); this
// service only picks the role for the transaction, with SET LOCAL, and never with a plain SET ROLE,
// which would outlast the request on a pooled connection. It connects with the retrieval login, using
// the standard PG* environment variables.
//
// It serves POST /search, and POST /chunks, which returns given chunks' text for a role, so the
// gateway can show a proposal's sources with the reader's own permissions. The caller names the role.
// Nothing here authenticates the caller: that is the gateway's job, and the network policy decides who
// can call. See ADRs 0014 and 0018.
package main

import (
	"bytes"
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	dimensions = 768
	maxQuery   = 2000
	maxK       = 20
	defaultK   = 5
	// Nomic's task prefix for queries. Documents were embedded with "search_document: ".
	queryPrefix = "search_query: "
	// The login, which must be the current user whenever a transaction starts.
	login = "retrieval"
)

// The only role statements this service sends. Each is a constant, so no input reaches the SQL.
var setRole = map[string]string{
	"technician": "SET LOCAL ROLE technician",
	"engineer":   "SET LOCAL ROLE engineer",
}

// Scans. "hnsw" is the default; the other two exist to measure it against. On a table this small the
// planner prefers a sequential scan, which is exact, so the index scans turn sequential scans off:
// otherwise the index would never be used and nothing about it would be measured. See ADR 0014.
//
// Each starts by forcing a custom plan. The query is a prepared statement, and after a few runs
// PostgreSQL may reuse one generic plan, built under whichever scan's settings came first, since
// changing these settings does not invalidate it.
var scans = map[string][]string{
	// Iterative scans keep reading the index until enough rows pass the role's policy.
	"hnsw":        {customPlan, "SET LOCAL enable_seqscan = off", "SET LOCAL hnsw.iterative_scan = relaxed_order"},
	"hnsw-strict": {customPlan, "SET LOCAL enable_seqscan = off", "SET LOCAL hnsw.iterative_scan = off"},
	"exact":       {customPlan, "SET LOCAL enable_indexscan = off", "SET LOCAL enable_bitmapscan = off"},
}

const customPlan = "SET LOCAL plan_cache_mode = force_custom_plan"

// relaxed_order may return rows slightly out of order, so they are sorted again after the limit.
const searchSQL = `WITH nearest AS MATERIALIZED (
  SELECT path, chunk_index, classification, content, content_sha256, embedding <=> $1::vector AS distance
  FROM rag.chunks ORDER BY distance LIMIT $2
) SELECT path, chunk_index, classification, content, content_sha256, distance FROM nearest ORDER BY distance, path, chunk_index`

// chunkSQL finds one chunk by its identity and hash. Row-level security still applies, so a chunk the
// role may not read is not found, exactly like one that does not exist.
const chunkSQL = `SELECT classification, content FROM rag.chunks WHERE path = $1 AND chunk_index = $2 AND content_sha256 = $3`

// maxChunks bounds one /chunks request.
const maxChunks = 20

type request struct {
	Query string `json:"query"`
	Role  string `json:"role"`
	// A pointer, so that a missing k takes the default and an explicit 0 is refused.
	K    *int   `json:"k"`
	Scan string `json:"scan"`
	k    int
}

type result struct {
	Path           string  `json:"path"`
	ChunkIndex     int     `json:"chunk_index"`
	Classification string  `json:"classification"`
	Distance       float64 `json:"distance"`
	Content        string  `json:"content"`
	ContentSHA256  string  `json:"content_sha256"`
}

type chunkRef struct {
	Path          string `json:"path"`
	ChunkIndex    int    `json:"chunk_index"`
	ContentSHA256 string `json:"content_sha256"`
}

type chunksRequest struct {
	Role   string     `json:"role"`
	Chunks []chunkRef `json:"chunks"`
}

// chunk is one requested chunk: its text, or found false when the role cannot read it, it does not
// exist, or its text has changed since the hash was taken.
type chunk struct {
	chunkRef
	Found          bool   `json:"found"`
	Classification string `json:"classification,omitempty"`
	Content        string `json:"content,omitempty"`
}

type response struct {
	Role    string   `json:"role"`
	Scan    string   `json:"scan"`
	Results []result `json:"results"`
}

type embedFunc func(ctx context.Context, text string) ([]float32, error)

type server struct {
	pool  *pgxpool.Pool
	embed embedFunc
}

func (r *request) validate() error {
	r.Query = strings.TrimSpace(r.Query)
	switch {
	case r.Query == "":
		return errors.New("query is empty")
	case len([]rune(r.Query)) > maxQuery:
		return fmt.Errorf("query is longer than %d characters", maxQuery)
	}
	if _, ok := setRole[r.Role]; !ok && r.Role != "" {
		return fmt.Errorf("unknown role %q", r.Role)
	}
	r.k = defaultK
	if r.K != nil {
		r.k = *r.K
	}
	if r.k < 1 || r.k > maxK {
		return fmt.Errorf("k must be between 1 and %d", maxK)
	}
	if r.Scan == "" {
		r.Scan = "hnsw"
	}
	if _, ok := scans[r.Scan]; !ok {
		return fmt.Errorf("unknown scan %q", r.Scan)
	}
	return nil
}

// asRole runs f in one transaction as role. With no role, no SET LOCAL is sent, so the login's own
// privileges apply and row-level security returns nothing.
func (s *server) asRole(ctx context.Context, role string, f func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	// A role left over on this connection would decide what this request sees. Refuse, and close the
	// connection so the pool does not hand it out again.
	var current string
	if err := tx.QueryRow(ctx, "SELECT current_user").Scan(&current); err != nil {
		return err
	}
	if current != login {
		_ = tx.Conn().Close(ctx)
		return fmt.Errorf("connection starts as %q, not %q: refusing it", current, login)
	}
	if role != "" {
		if _, err := tx.Exec(ctx, setRole[role]); err != nil {
			return err
		}
	}
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// search runs one query in one transaction, as the request's role.
func (s *server) search(ctx context.Context, r request, vector string) ([]result, error) {
	var results []result
	err := s.asRole(ctx, r.Role, func(tx pgx.Tx) error {
		for _, stmt := range scans[r.Scan] {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, searchSQL, vector, r.k)
		if err != nil {
			return err
		}
		results, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (result, error) {
			var res result
			err := row.Scan(&res.Path, &res.ChunkIndex, &res.Classification, &res.Content, &res.ContentSHA256, &res.Distance)
			return res, err
		})
		return err
	})
	return results, err
}

// chunks looks up each requested chunk in one transaction, as the request's role.
func (s *server) chunks(ctx context.Context, r chunksRequest) ([]chunk, error) {
	out := make([]chunk, 0, len(r.Chunks))
	err := s.asRole(ctx, r.Role, func(tx pgx.Tx) error {
		for _, ref := range r.Chunks {
			c := chunk{chunkRef: ref}
			err := tx.QueryRow(ctx, chunkSQL, ref.Path, ref.ChunkIndex, ref.ContentSHA256).Scan(&c.Classification, &c.Content)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			default:
				c.Found = true
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

func (s *server) handleChunks(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var r chunksRequest
	dec := json.NewDecoder(io.LimitReader(req.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := setRole[r.Role]; !ok && r.Role != "" {
		http.Error(w, fmt.Sprintf("unknown role %q", r.Role), http.StatusBadRequest)
		return
	}
	if len(r.Chunks) > maxChunks {
		http.Error(w, fmt.Sprintf("at most %d chunks", maxChunks), http.StatusBadRequest)
		return
	}
	out, err := s.chunks(req.Context(), r)
	if err != nil {
		log.Printf("chunks: %v", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"role": r.Role, "chunks": out})
}

func (s *server) handleSearch(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var r request
	dec := json.NewDecoder(io.LimitReader(req.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, err := s.embed(req.Context(), queryPrefix+r.Query)
	if err != nil {
		log.Printf("embed: %v", err)
		http.Error(w, "embedding failed", http.StatusBadGateway)
		return
	}
	results, err := s.search(req.Context(), r, vectorText(v))
	if err != nil {
		log.Printf("search: %v", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []result{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{Role: r.Role, Scan: r.Scan, Results: results})
}

func (s *server) handleReady(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok\n")
}

func vectorText(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// embedder calls an OpenAI-compatible embedding server.
func embedder(url string) embedFunc {
	client := &http.Client{Timeout: 30 * time.Second}
	return func(ctx context.Context, text string) ([]float32, error) {
		body, err := json.Marshal(map[string]any{"input": []string{text}, "model": "nomic-embed-text-v1.5"})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("embedding server: %s: %.200s", resp.Status, raw)
		}
		var reply struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return nil, err
		}
		if len(reply.Data) != 1 || len(reply.Data[0].Embedding) != dimensions {
			return nil, errors.New("embedding server returned an unexpected shape")
		}
		return reply.Data[0].Embedding, nil
	}
}

func main() {
	addr := flag.String("listen", ":8080", "address to serve on")
	embedURL := flag.String("embed-url", "http://embed.inference.svc:8080", "OpenAI-compatible embedding server")
	flag.Parse()
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{pool: pool, embed: embedder(strings.TrimRight(*embedURL, "/") + "/v1/embeddings")}
	mux := http.NewServeMux()
	mux.HandleFunc("/search", s.handleSearch)
	mux.HandleFunc("/chunks", s.handleChunks)
	mux.HandleFunc("/ready", s.handleReady)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second}
	// On SIGTERM, finish the requests in flight, then close the pool.
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
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pool.Close()
}
