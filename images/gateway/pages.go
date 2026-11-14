package main

import (
	"bytes"
	"context"
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
	Turn     *turn
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
	Sources              []sourceView
	SourcesError         bool
	Expires              string
	Expired              bool
}

// fieldOrder puts a known tool's fields in reading order. Others are shown in canonical order.
var fieldOrder = map[string][]string{"create_ticket": {"title", "priority", "body"}}

func (s *server) view(p approval.Proposal, sess session) (page, error) {
	// Raw values, so a number or an object is shown exactly as the canonical text holds it, and so as
	// it will be filed, not as Go would re-encode it.
	var args map[string]json.RawMessage
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
	v := &proposalView{ID: p.ID.String(), Tool: p.Tool, ArgsSHA256: p.ArgsSHA256,
		Expires: p.ExpiresAt.UTC().Format(time.RFC3339), Expired: !s.now().Before(p.ExpiresAt)}
	for _, k := range names {
		value := string(args[k])
		var text string
		if json.Unmarshal(args[k], &text) == nil {
			value = text
		}
		v.Fields = append(v.Fields, field{k, value})
	}
	return page{Title: "Confirm a proposal", Session: &sess, Proposal: v}, nil
}

// sourceView is one source as the page shows it: its identity, and its text if the viewer's role
// can read it and it has not changed since it was found.
type sourceView struct {
	approval.Source
	Found          bool
	Classification string
	Text           string
}

// sourceText fills in the sources' text, read through the retrieval service with the viewer's own
// role. Without it the sources are still listed, by identity.
func (s *server) sourceText(ctx context.Context, v *proposalView, sess session, sources []approval.Source) {
	for _, src := range sources {
		v.Sources = append(v.Sources, sourceView{Source: src})
	}
	if len(sources) == 0 {
		return
	}
	found, err := s.retrieval.chunks(ctx, sess.role, sources)
	if err == nil && len(found) != len(sources) {
		err = fmt.Errorf("asked for %d sources, got %d", len(sources), len(found))
	}
	if err != nil {
		log.Printf("source text: %v", err)
		v.SourcesError = true
		return
	}
	for i, f := range found {
		if f.Found && f.Path == sources[i].Path && f.ChunkIndex == sources[i].ChunkIndex {
			v.Sources[i].Found, v.Sources[i].Classification, v.Sources[i].Text = true, f.Classification, f.Content
		}
	}
}

// visible shows characters that would otherwise hide, disguise or reorder text as their code points,
// so the user reads every character that will be filed. Newlines and tabs are kept. Shown are:
// controls and format characters, such as bidirectional overrides, zero-width spaces and tag
// characters; variation selectors; other default-ignorable characters, such as Hangul fillers; line
// and paragraph separators; private-use and unassigned code points; and the replacement character.
// Lookalike letters from other scripts are visible characters and are not flagged.
func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || !hidden(r) {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "⟨U+%04X⟩", r)
	}
	return b.String()
}

// hidden: anything Go does not count as graphic, which takes in controls, format characters,
// separators other than the space, private-use, surrogate and unassigned code points; and the graphic
// ones that render as nothing or as a stand-in.
func hidden(r rune) bool {
	return !unicode.IsGraphic(r) || unicode.In(r, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) ||
		r == unicode.ReplacementChar
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
.source{border-left:3px solid #2563eb;padding:.25rem .75rem;margin:.25rem 0 .75rem}
@media (prefers-color-scheme:dark){body{background:#121212;color:#e8e8e8}.model{background:#2a1f0a;border-color:#d97706}.model .tag{color:#fbbf24}dt,.note{color:#aaa}}
</style></head><body>
<header><strong>Maintenance confirmations</strong>
{{with .Session}}<form method="post" action="/logout">{{.User}}{{with .Role}} ({{.}}){{else}} (no role){{end}}
<input type="hidden" name="csrf" value="{{.CSRF}}"> <button>Sign out</button></form>{{end}}</header>
<main><h1>{{.Title}}</h1>{{end}}
{{define "bottom"}}</main></body></html>{{end}}

{{define "home"}}{{template "top" .}}
{{if .Session}}<p>You are signed in. <a href="/chat">Ask the manuals</a>, or open a proposal from its link to review and confirm it.</p>
{{else}}<p><a href="/login">Sign in with Forgejo</a> to review and confirm proposals.</p>{{end}}
{{template "bottom"}}{{end}}

{{define "message"}}{{template "top" .}}<p>{{.Message}}</p><p><a href="/">Start page</a></p>{{template "bottom"}}{{end}}

{{define "chat"}}{{template "top" .}}
{{with .Message}}<p><strong>{{.}}</strong></p>{{end}}
{{with .Turn}}<h2>Question</h2><pre>{{visible .Question}}</pre>
{{range .Steps}}{{if eq .Kind "search"}}<p class="note">Searched the manuals for “{{visible .Query}}”: {{.Found}} passages.</p>
{{else if eq .Kind "proposal"}}<p><strong>A ticket was proposed, not filed.</strong> <a href="/proposals/{{.Proposal}}">Review and confirm it</a>.</p>
{{else}}<p class="note">A tool call was refused: {{.Reason}}</p>{{end}}{{end}}
{{if .Answer}}<div class="model"><div class="tag">answer · written by the model</div><pre>{{visible .Answer}}</pre></div>{{end}}
{{if .Stopped}}<p><strong>The model kept calling tools without answering, and was stopped.</strong></p>{{end}}
{{if .Sources}}<h2>Passages found</h2><ol>{{range .Sources}}<li>{{.Path}}, chunk {{.ChunkIndex}}, {{.Classification}}</li>{{end}}</ol>{{end}}
{{end}}
{{with .Session}}<form method="post" action="/chat"><input type="hidden" name="csrf" value="{{.CSRF}}">
<p><textarea name="question" rows="4" cols="60" required maxlength="2000" placeholder="Ask about the manuals, or describe a fault to file a ticket"></textarea></p>
<p><button class="confirm">Ask</button> <span class="note">The page waits while the model, on CPU, answers.</span></p></form>{{end}}
{{template "bottom"}}{{end}}

{{define "filed"}}{{template "top" .}}<p>The executor filed it as <a href="{{.IssueURL}}">ticket #{{.Issue}}</a>.</p>{{template "bottom"}}{{end}}

{{define "proposal"}}{{template "top" .}}{{with .Proposal}}
<p>A model proposed this call for you. Nothing runs unless you confirm it. Read the marked fields
yourself: the model wrote them, and what is shown is exactly what will be filed.</p>
<dl><dt>Tool</dt><dd>{{.Tool}}</dd><dt>Proposal</dt><dd>{{.ID}}</dd>
<dt>Arguments SHA-256</dt><dd>{{.ArgsSHA256}}</dd><dt>Expires</dt><dd>{{.Expires}}</dd></dl>
{{range .Fields}}<div class="model"><div class="tag">{{.Name}} · written by the model</div><pre>{{visible .Value}}</pre></div>{{end}}
<h2>Sources</h2>
{{if .Sources}}<p class="note">The passages the model had found when it proposed this, read with your role.</p>
{{if .SourcesError}}<p class="note">Their text could not be loaded.</p>{{end}}
<ol>{{range .Sources}}<li><span class="note">{{.Path}}, chunk {{.ChunkIndex}}, SHA-256 {{.ContentHash}}{{with .Classification}}, {{.}}{{end}}</span>
{{if .Found}}<div class="source"><pre>{{visible .Text}}</pre></div>{{else if not $.Proposal.SourcesError}}<p class="note">Not readable with your role, or changed since.</p>{{end}}</li>{{end}}</ol>
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
