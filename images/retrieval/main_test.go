package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func intp(n int) *int { return &n }

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		r  request
		ok bool
	}{
		{request{Query: "q"}, true},
		{request{Query: "q", Role: "technician", K: intp(20), Scan: "exact"}, true},
		{request{Query: "  "}, false},
		{request{Query: strings.Repeat("x", maxQuery+1)}, false},
		{request{Query: "q", Role: "Engineer"}, false},
		{request{Query: "q", Role: "ingest"}, false},
		{request{Query: "q", Role: "engineer; RESET ROLE"}, false},
		{request{Query: "q", K: intp(21)}, false},
		{request{Query: "q", K: intp(-1)}, false},
		{request{Query: "q", K: intp(0)}, false},
		{request{Query: "q", Scan: "seq"}, false},
	} {
		r := c.r
		if err := r.validate(); (err == nil) != c.ok {
			t.Errorf("%+v: got %v", c.r, err)
		}
	}
	// An explicit 0 in JSON is refused; a missing k is the default.
	var zero request
	if err := json.Unmarshal([]byte(`{"query":"q","k":0}`), &zero); err != nil || zero.validate() == nil {
		t.Errorf(`{"k":0} was accepted`)
	}
	r := request{Query: " q "}
	if err := r.validate(); err != nil || r.k != defaultK || r.Scan != "hnsw" || r.Query != "q" {
		t.Errorf("defaults: %+v, %v", r, err)
	}
}

// The role must only ever be set for the transaction. A plain SET ROLE, a RESET, or set_config on the
// role, in any SQL this program sends, would be a bug that row-level security cannot catch (ADR 0012).
// Each string literal in the program's source is checked, so comments may name what is forbidden. A
// statement assembled from pieces at run time would not be caught; the guard in search() is the
// backstop for that.
func TestOnlySetLocalRole(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile(`(?i)(^|[^a-z])(SET\s+(SESSION\s+)?ROLE|RESET\s+ROLE|SET\s+SESSION\s+AUTHORIZATION|RESET\s+ALL)\b|set_config\s*\(\s*'role'`)
	literals := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				literals++
				if m := plain.FindString(lit.Value); m != "" {
					t.Errorf("%s sends %q; use SET LOCAL ROLE", f, lit.Value)
				}
			}
			return true
		})
	}
	if literals == 0 {
		t.Fatal("no string literals found: the check is not looking at the program")
	}
	for role, stmt := range setRole {
		if stmt != "SET LOCAL ROLE "+role {
			t.Errorf("%s: %q", role, stmt)
		}
	}
	// The check itself: it must catch the statements it exists to stop.
	for _, bad := range []string{`"SET ROLE engineer"`, `"reset role"`, `"SELECT set_config('role', 'engineer', false)"`} {
		if !plain.MatchString(bad) {
			t.Errorf("the pattern misses %s", bad)
		}
	}
	if plain.MatchString(`"SET LOCAL ROLE engineer"`) {
		t.Error("the pattern refuses SET LOCAL ROLE")
	}
	// Every scan forces a custom plan, so a cached plan from another scan's settings is never reused.
	for name, stmts := range scans {
		if len(stmts) == 0 || stmts[0] != customPlan {
			t.Errorf("scan %s does not start with %q", name, customPlan)
		}
	}
}

func TestVectorText(t *testing.T) {
	if got := vectorText([]float32{0.5, -1, 0.1}); got != "[0.5,-1,0.1]" {
		t.Fatalf("got %s", got)
	}
}

// The tests below need the schema from deploy/retrieval/postgres.yaml. RETRIEVAL_TEST_DSN connects as
// the retrieval login and INGEST_TEST_DSN as the ingest login. Without them, they are skipped.
func database(t *testing.T) (*server, func(string) string) {
	t.Helper()
	dsn, ingestDSN := os.Getenv("RETRIEVAL_TEST_DSN"), os.Getenv("INGEST_TEST_DSN")
	if dsn == "" || ingestDSN == "" {
		t.Skip("RETRIEVAL_TEST_DSN and INGEST_TEST_DSN are not set")
	}
	ctx := context.Background()
	ingest, err := pgx.Connect(ctx, ingestDSN)
	if err != nil {
		t.Fatal(err)
	}
	vec := func(x float32) string {
		v := make([]float32, dimensions)
		v[0], v[1] = 1, x
		return vectorText(v)
	}
	clean := "DELETE FROM rag.chunks WHERE source = 'retrieval-test'"
	if _, err := ingest.Exec(ctx, clean); err != nil {
		t.Fatal(err)
	}
	// Searches cover the whole table, so other rows would change the results.
	var others int
	if err := ingest.QueryRow(ctx, "SELECT count(*) FROM rag.chunks").Scan(&others); err != nil {
		t.Fatal(err)
	}
	if others != 0 {
		t.Fatalf("these tests need a table with no other rows; it has %d", others)
	}
	for i, label := range []string{"public", "restricted", "public", "restricted"} {
		if _, err := ingest.Exec(ctx, `INSERT INTO rag.chunks (source, path, chunk_index, classification, content, content_sha256, embedding)
			VALUES ('retrieval-test', $1, 0, $2, $1, 'none', $3::vector)`, label+strconv.Itoa(i), label, vec(float32(i)/10)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = ingest.Exec(ctx, clean); _ = ingest.Close(ctx) })

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1 // every request reuses the same connection
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &server{pool: pool}, func(string) string { return vec(0) }
}

func labels(rs []result) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Classification)
	}
	return strings.Join(out, ",")
}

func TestRolesOnOneConnection(t *testing.T) {
	s, vec := database(t)
	ctx := context.Background()
	run := func(role, scan string) string {
		r := request{Query: "q", Role: role, K: intp(10), Scan: scan}
		if err := r.validate(); err != nil {
			t.Fatal(err)
		}
		rs, err := s.search(ctx, r, vec("q"))
		if err != nil {
			t.Fatalf("%s/%s: %v", role, scan, err)
		}
		return labels(rs)
	}
	for _, scan := range []string{"hnsw", "hnsw-strict", "exact"} {
		// Alternating on one connection: no request may see the one before it.
		for range 5 {
			if got := run("engineer", scan); got != "public,restricted,public,restricted" {
				t.Errorf("engineer/%s: %s", scan, got)
			}
			if got := run("", scan); got != "" {
				t.Errorf("no role after engineer/%s: %s", scan, got)
			}
			if got := run("technician", scan); got != "public,public" {
				t.Errorf("technician/%s: %s", scan, got)
			}
		}
	}
}

// Each scan must use the plan it is named for: the index for both HNSW scans, and no index for exact.
func TestScansUseTheirPlans(t *testing.T) {
	s, vec := database(t)
	ctx := context.Background()
	for scan, want := range map[string]string{
		"hnsw": "Index Scan using chunks_embedding", "hnsw-strict": "Index Scan using chunks_embedding", "exact": "Seq Scan",
	} {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range append([]string{setRole["technician"]}, scans[scan]...) {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := tx.Query(ctx, "EXPLAIN "+searchSQL, vec("q"), 5)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(ctx)
		plan := strings.Join(lines, "\n")
		if !strings.Contains(plan, want) || (scan == "exact" && strings.Contains(plan, "chunks_embedding")) {
			t.Errorf("%s: plan lacks %q:\n%s", scan, want, plan)
		}
	}
}

// A role left on the connection, as a plain SET ROLE would leave it, must stop the next request.
func TestLeftoverRoleIsRefused(t *testing.T) {
	s, vec := database(t)
	ctx := context.Background()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SET ROLE engineer"); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	r := request{Query: "q"}
	_ = r.validate()
	if rs, err := s.search(ctx, r, vec("q")); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("got %d results and %v; want a refusal", len(rs), err)
	}
	// The connection was closed, so the next request gets a clean one.
	rs, err := s.search(ctx, r, vec("q"))
	if err != nil || len(rs) != 0 {
		t.Fatalf("after the refusal: %d results, %v", len(rs), err)
	}
}

// A chunk is returned only to a role that may read it, and only with its recorded hash.
func TestChunksFollowRoles(t *testing.T) {
	s, query := database(t)
	ctx := context.Background()
	refs := []chunkRef{{"public0", 0, "none"}, {"restricted1", 0, "none"}, {"public2", 0, "changed"}, {"missing", 0, "none"}}
	for role, want := range map[string]string{
		"technician": "true,false,false,false",
		"engineer":   "true,true,false,false",
		"":           "false,false,false,false",
	} {
		got, err := s.chunks(ctx, chunksRequest{Role: role, Chunks: refs})
		if err != nil {
			t.Fatal(err)
		}
		var found []string
		for i, c := range got {
			found = append(found, strconv.FormatBool(c.Found))
			if c.Found && (c.Content != refs[i].Path || c.Path != refs[i].Path) {
				t.Errorf("%s: chunk %d came back as %+v", role, i, c)
			}
			if !c.Found && c.Content != "" {
				t.Errorf("%s: an unreadable chunk carried text", role)
			}
		}
		if strings.Join(found, ",") != want {
			t.Errorf("role %q: found %s, want %s", role, strings.Join(found, ","), want)
		}
	}
	// Search results carry the hash the gateway records as a source.
	rs, err := s.search(ctx, request{Role: "technician", Scan: "exact", k: 2}, query(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) == 0 || rs[0].ContentSHA256 != "none" {
		t.Errorf("search results lack the content hash: %+v", rs)
	}
}

func TestChunksRequestChecks(t *testing.T) {
	s := &server{}
	for name, body := range map[string]string{
		"unknown role":  `{"role":"admin","chunks":[]}`,
		"unknown field": `{"role":"technician","chunks":[],"sql":"x"}`,
		"too many":      `{"role":"technician","chunks":[` + strings.Repeat(`{"path":"p","chunk_index":0,"content_sha256":"h"},`, maxChunks) + `{"path":"p","chunk_index":0,"content_sha256":"h"}]}`,
	} {
		w := httptest.NewRecorder()
		s.handleChunks(w, httptest.NewRequest(http.MethodPost, "/chunks", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}
