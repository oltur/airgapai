package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"airgapai/gateway/approval"
)

// page is what every template gets. Only the fields a template uses are set.
type page struct {
	Title    string
	Session  *session
	Message  string
	Issue    int64
	IssueURL string
	Proposal *proposalView
}

// User and Role let templates read the embedded identity, whose fields are unexported.
func (s *session) User() string { return s.user }
func (s *session) Role() string { return s.role }
func (s *session) CSRF() string { return s.csrf }

type field struct{ Name, Value string }

// proposalView is the proposal as the page shows it, built only from what the database holds.
type proposalView struct {
	ID, Tool, ArgsSHA256 string
	Fields               []field // written by the model
	Sources              []approval.Source
	Expires              string
	Expired              bool
}

// fieldOrder puts a known tool's fields in reading order. Others are shown in canonical order.
var fieldOrder = map[string][]string{"create_ticket": {"title", "priority", "body"}}

func (s *server) view(p approval.Proposal, sess session) (page, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(p.Args), &args); err != nil {
		return page{}, err
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	slices.Sort(names)
	order := fieldOrder[p.Tool]
	slices.SortStableFunc(names, func(a, b string) int {
		ia, ib := slices.Index(order, a), slices.Index(order, b)
		if ia < 0 {
			ia = len(order)
		}
		if ib < 0 {
			ib = len(order)
		}
		return ia - ib
	})
	v := &proposalView{ID: p.ID.String(), Tool: p.Tool, ArgsSHA256: p.ArgsSHA256, Sources: p.Sources,
		Expires: p.ExpiresAt.UTC().Format(time.RFC3339), Expired: !s.sessions.now().Before(p.ExpiresAt)}
	for _, k := range names {
		value, ok := args[k].(string)
		if !ok {
			raw, err := json.Marshal(args[k])
			if err != nil {
				return page{}, err
			}
			value = string(raw)
		}
		v.Fields = append(v.Fields, field{k, value})
	}
	return page{Title: "Confirm a proposal", Session: &sess, Proposal: v}, nil
}

// visible shows characters that would otherwise hide or reorder text, such as bidirectional overrides
// and zero-width spaces, as their code points, so what the page shows is what will be filed. Newlines
// and tabs are kept as they are.
func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || !(unicode.Is(unicode.Cf, r) || unicode.IsControl(r) || r == unicode.ReplacementChar) {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "⟨U+%04X⟩", r)
	}
	return b.String()
}

var templates = template.Must(template.New("").Funcs(template.FuncMap{"visible": visible}).Parse(`
{{define "top"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 1rem;color:#1b1b1b;background:#fff}
header{display:flex;justify-content:space-between;align-items:center;gap:1rem;border-bottom:1px solid #ccc;padding-bottom:.5rem}
.model{border:2px dashed #b45309;background:#fffbeb;padding:.5rem .75rem;margin:.75rem 0}
.model .tag{font-size:.8rem;font-weight:600;color:#92400e;text-transform:uppercase;letter-spacing:.03em}
pre{white-space:pre-wrap;word-break:break-word;margin:.25rem 0 0;font:15px/1.45 ui-monospace,monospace}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.25rem 1rem}dt{color:#555}dd{margin:0;word-break:break-all}
button{font:inherit;padding:.5rem 1rem;cursor:pointer}.confirm{background:#14532d;color:#fff;border:0;border-radius:4px}
.note{color:#555;font-size:.9rem}
@media (prefers-color-scheme:dark){body{background:#121212;color:#e8e8e8}.model{background:#2a1f0a;border-color:#d97706}.model .tag{color:#fbbf24}dt,.note{color:#aaa}}
</style></head><body>
<header><strong>Maintenance confirmations</strong>
{{with .Session}}<form method="post" action="/logout">{{.User}}{{with .Role}} ({{.}}){{else}} (no role){{end}}
<input type="hidden" name="csrf" value="{{.CSRF}}"> <button>Sign out</button></form>{{end}}</header>
<main><h1>{{.Title}}</h1>{{end}}
{{define "bottom"}}</main></body></html>{{end}}

{{define "home"}}{{template "top" .}}
{{if .Session}}<p>You are signed in. Open a proposal from the link you were given to review and confirm it.</p>
{{else}}<p><a href="/login">Sign in with Forgejo</a> to review and confirm proposals.</p>{{end}}
{{template "bottom"}}{{end}}

{{define "message"}}{{template "top" .}}<p>{{.Message}}</p><p><a href="/">Start page</a></p>{{template "bottom"}}{{end}}

{{define "filed"}}{{template "top" .}}<p>The executor filed it as <a href="{{.IssueURL}}">ticket #{{.Issue}}</a>.</p>{{template "bottom"}}{{end}}

{{define "proposal"}}{{template "top" .}}{{with .Proposal}}
<p>A model proposed this call for you. Nothing runs unless you confirm it. Read the marked fields
yourself: the model wrote them, and what is shown is exactly what will be filed.</p>
<dl><dt>Tool</dt><dd>{{.Tool}}</dd><dt>Proposal</dt><dd>{{.ID}}</dd>
<dt>Arguments SHA-256</dt><dd>{{.ArgsSHA256}}</dd><dt>Expires</dt><dd>{{.Expires}}</dd></dl>
{{range .Fields}}<div class="model"><div class="tag">{{.Name}} · written by the model</div><pre>{{visible .Value}}</pre></div>{{end}}
<h2>Sources</h2>
{{if .Sources}}<ul>{{range .Sources}}<li>{{.Path}}, chunk {{.ChunkIndex}}, SHA-256 {{.ContentHash}}</li>{{end}}</ul>
{{else}}<p class="note">The proposal names no sources.</p>{{end}}
<p class="note">The executor files the ticket in maintenance/tickets as itself, with a first line naming you and this proposal.
It checks your team in Forgejo again before filing.</p>
{{if .Expired}}<p><strong>This proposal has expired and can no longer be confirmed.</strong></p>
{{else}}<form method="post" action="/proposals/{{.ID}}/confirm"><input type="hidden" name="csrf" value="{{$.Session.CSRF}}">
<button class="confirm">Confirm and file</button></form>{{end}}
{{end}}{{template "bottom"}}{{end}}
`))

func (s *server) render(w http.ResponseWriter, status int, name string, p page) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, p); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "page error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
