// Ingest cuts the Markdown corpus into chunks, embeds them and writes them to rag.chunks, so that
// the table matches the corpus. It is idempotent: a chunk whose text and label are unchanged is not
// embedded or written again, a chunk whose label alone changed is relabelled without a new
// embedding, and rows for chunks that no longer exist are deleted. It connects with the ingest login,
// using the standard PG* environment variables.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	dimensions = 768
	batchSize  = 8
	// Nomic's task prefix for documents. Queries use "search_query: ".
	documentPrefix = "search_document: "
	// Held for the whole run, so two runs never interleave.
	lockID = 7_100_001
)

type config struct {
	corpus, source, embedURL string
	restricted               []string
	wait                     time.Duration
}

type chunk struct {
	path           string
	index          int
	classification string
	content, hash  string
}

type stats struct{ files, chunks, inserted, updated, relabelled, unchanged, deleted, reused int }

func main() {
	var cfg config
	var restricted string
	flag.StringVar(&cfg.corpus, "corpus", "/corpus", "directory holding the Markdown files")
	flag.StringVar(&cfg.source, "source", "kubernetes-docs", "value of the source column for these rows")
	flag.StringVar(&cfg.embedURL, "embed-url", "http://embed.inference.svc:8080", "OpenAI-compatible embedding server")
	flag.StringVar(&restricted, "restricted", "", "comma-separated path prefixes labelled restricted; everything else is public")
	flag.DurationVar(&cfg.wait, "wait", 10*time.Minute, "how long to wait for the database and the embedding server")
	dryRun := flag.Bool("dry-run", false, "cut the corpus into chunks and print counts, without the database or embeddings")
	flag.Parse()
	for _, p := range strings.Split(restricted, ",") {
		if p = strings.TrimSpace(p); p != "" {
			cfg.restricted = append(cfg.restricted, p)
		}
	}
	if *dryRun {
		if err := summary(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "ingest:", err)
			os.Exit(1)
		}
		return
	}
	start := time.Now()
	s, err := run(context.Background(), cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ingest:", err)
		os.Exit(1)
	}
	fmt.Printf("files=%d chunks=%d inserted=%d updated=%d relabelled=%d unchanged=%d deleted=%d reused=%d seconds=%.1f\n",
		s.files, s.chunks, s.inserted, s.updated, s.relabelled, s.unchanged, s.deleted, s.reused, time.Since(start).Seconds())
}

// classify applies the synthetic label rule: a path under a restricted prefix is restricted.
func classify(path string, restricted []string) string {
	for _, p := range restricted {
		if strings.HasPrefix(path, p) {
			return "restricted"
		}
	}
	return "public"
}

// load reads every Markdown file under the corpus root, in a fixed order, and cuts it into chunks.
func load(cfg config) (map[string][]chunk, error) {
	pages := map[string][]chunk{}
	err := filepath.WalkDir(cfg.corpus, func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(file, ".md") {
			return err
		}
		rel, err := filepath.Rel(cfg.corpus, file)
		if err != nil {
			return err
		}
		text, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		label := classify(filepath.ToSlash(rel), cfg.restricted)
		var cs []chunk
		for i, c := range Chunks(string(text)) {
			sum := sha256.Sum256([]byte(c))
			cs = append(cs, chunk{filepath.ToSlash(rel), i, label, c, hex.EncodeToString(sum[:])})
		}
		pages[filepath.ToSlash(rel)] = cs
		return nil
	})
	if err == nil && len(pages) == 0 {
		err = fmt.Errorf("no Markdown files under %s", cfg.corpus)
	}
	return pages, err
}

// summary prints, for each label, the number of files and chunks and the largest chunk.
func summary(cfg config) error {
	pages, err := load(cfg)
	if err != nil {
		return err
	}
	files, chunks, largest := map[string]int{}, map[string]int{}, 0
	for _, cs := range pages {
		for i, c := range cs {
			if i == 0 {
				files[c.classification]++
			}
			chunks[c.classification]++
			largest = max(largest, len([]rune(c.content)))
		}
	}
	for _, label := range []string{"public", "restricted"} {
		fmt.Printf("%s files=%d chunks=%d\n", label, files[label], chunks[label])
	}
	fmt.Printf("largest chunk: %d characters\n", largest)
	return nil
}

func retry(ctx context.Context, limit time.Duration, what string, f func() error) error {
	deadline := time.Now().Add(limit)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %w", what, err)
		}
		fmt.Fprintf(os.Stderr, "waiting for %s: %v\n", what, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func run(ctx context.Context, cfg config) (stats, error) {
	var s stats
	pages, err := load(cfg)
	if err != nil {
		return s, err
	}
	var conn *pgx.Conn
	if err := retry(ctx, cfg.wait, "the database", func() (err error) {
		conn, err = pgx.Connect(ctx, "")
		return err
	}); err != nil {
		return s, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return s, err
	}
	embedder := &embedder{url: strings.TrimRight(cfg.embedURL, "/") + "/v1/embeddings", client: &http.Client{Timeout: 5 * time.Minute}}
	if err := retry(ctx, cfg.wait, "the embedding server", embedder.ready); err != nil {
		return s, err
	}

	paths := make([]string, 0, len(pages))
	for p := range pages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := syncPage(ctx, conn, embedder, cfg.source, p, pages[p], &s); err != nil {
			return s, fmt.Errorf("%s: %w", p, err)
		}
		s.files++
		s.chunks += len(pages[p])
	}
	// Pages that are no longer in the corpus.
	tag, err := conn.Exec(ctx, "DELETE FROM rag.chunks WHERE source = $1 AND NOT (path = ANY($2))", cfg.source, paths)
	if err != nil {
		return s, err
	}
	s.deleted += int(tag.RowsAffected())
	return s, nil
}

// syncPage makes one page's rows match its chunks, in one transaction. Only new or changed chunks
// are embedded.
func syncPage(ctx context.Context, conn *pgx.Conn, e *embedder, source, path string, chunks []chunk, s *stats) error {
	existing := map[int][2]string{}
	// Embeddings by text, so a chunk that only moved within the page keeps its vector.
	known := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT chunk_index, content_sha256, classification, embedding::text FROM rag.chunks WHERE source = $1 AND path = $2", source, path)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var hash, label, vector string
		if err := rows.Scan(&i, &hash, &label, &vector); err != nil {
			return err
		}
		existing[i] = [2]string{hash, label}
		known[hash] = vector
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Changed text needs an embedding, unless the same text is already in the page. A changed label
	// alone does not.
	var changed, relabelled []chunk
	for _, c := range chunks {
		old, ok := existing[c.index]
		switch {
		case ok && old == [2]string{c.hash, c.classification}:
			s.unchanged++
		case ok && old[0] == c.hash:
			relabelled = append(relabelled, c)
		default:
			changed = append(changed, c)
		}
	}
	vectors := make([]string, len(changed))
	var missing []int
	for i, c := range changed {
		if v, ok := known[c.hash]; ok {
			vectors[i] = v
			s.reused++
		} else {
			missing = append(missing, i)
		}
	}
	for i := 0; i < len(missing); i += batchSize {
		batch := missing[i:min(i+batchSize, len(missing))]
		texts := []string{}
		for _, j := range batch {
			texts = append(texts, documentPrefix+changed[j].content)
		}
		v, err := e.embed(ctx, texts)
		if err != nil {
			return err
		}
		for k, j := range batch {
			vectors[j] = vectorText(v[k])
		}
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after commit
	for i, c := range changed {
		if _, err := tx.Exec(ctx, `INSERT INTO rag.chunks (source, path, chunk_index, classification, content, content_sha256, embedding)
			VALUES ($1, $2, $3, $4, $5, $6, $7::vector)
			ON CONFLICT (source, path, chunk_index) DO UPDATE SET classification = EXCLUDED.classification,
			  content = EXCLUDED.content, content_sha256 = EXCLUDED.content_sha256, embedding = EXCLUDED.embedding`,
			source, path, c.index, c.classification, c.content, c.hash, vectors[i]); err != nil {
			return err
		}
		if _, ok := existing[c.index]; ok {
			s.updated++
		} else {
			s.inserted++
		}
	}
	for _, c := range relabelled {
		if _, err := tx.Exec(ctx, "UPDATE rag.chunks SET classification = $4 WHERE source = $1 AND path = $2 AND chunk_index = $3",
			source, path, c.index, c.classification); err != nil {
			return err
		}
		s.relabelled++
	}
	tag, err := tx.Exec(ctx, "DELETE FROM rag.chunks WHERE source = $1 AND path = $2 AND chunk_index >= $3", source, path, len(chunks))
	if err != nil {
		return err
	}
	s.deleted += int(tag.RowsAffected())
	return tx.Commit(ctx)
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

type embedder struct {
	url    string
	client *http.Client
}

func (e *embedder) ready() error {
	_, err := e.embed(context.Background(), []string{documentPrefix + "ready"})
	return err
}

// embed returns one vector per text, in the order given.
func (e *embedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"input": texts, "model": "nomic-embed-text-v1.5"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding server: %s: %.200s", resp.Status, raw)
	}
	var reply struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	if len(reply.Data) != len(texts) {
		return nil, fmt.Errorf("embedding server returned %d vectors for %d texts", len(reply.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range reply.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index] != nil {
			return nil, errors.New("embedding server returned a bad index")
		}
		if len(d.Embedding) != dimensions {
			return nil, fmt.Errorf("embedding has %d dimensions, want %d", len(d.Embedding), dimensions)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}
