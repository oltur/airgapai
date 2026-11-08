package main

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Chunk sizes, in characters. The upper bound keeps each chunk well inside the embedding model's
// 2048-token window, even for code, which has more tokens per character than prose.
const (
	maxChars = 1500
	minChars = 400
)

var (
	shortcode   = regexp.MustCompile(`\{\{[<%]\s*(/?)([a-z_-]+)((?:[^}]|\}[^}])*?)\s*[>%]\}\}`)
	attribute   = regexp.MustCompile(`([a-z_]+)="([^"]*)"`)
	firstArg    = regexp.MustCompile(`^\s*"([^"]*)"`)
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTag     = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)
	mdLink      = regexp.MustCompile(`\[([^\]]+)\]\([^)\s]+\)`)
	dropped     = regexp.MustCompile(`(?s)\{\{<\s*(comment|mermaid)\s*>\}\}.*?\{\{<\s*/(comment|mermaid)\s*>\}\}`)
	headingLine = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
)

var headings = map[string]string{
	"whatsnext":     "What's next",
	"prerequisites": "Before you begin",
	"objectives":    "Objectives",
	"cleanup":       "Clean up",
}

// replaceShortcode turns a Hugo shortcode into the text a reader would see, or nothing.
func replaceShortcode(tag string) string {
	m := shortcode.FindStringSubmatch(tag)
	closing, name, args := m[1] == "/", m[2], m[3]
	attrs := map[string]string{}
	for _, a := range attribute.FindAllStringSubmatch(args, -1) {
		attrs[a[1]] = a[2]
	}
	if closing {
		return ""
	}
	switch name {
	case "glossary_tooltip":
		if attrs["text"] != "" {
			return attrs["text"]
		}
		return strings.ReplaceAll(attrs["term_id"], "-", " ")
	case "link":
		return attrs["text"]
	case "heading":
		if a := firstArg.FindStringSubmatch(args); a != nil {
			if h, ok := headings[a[1]]; ok {
				return h
			}
			return a[1]
		}
	case "note", "caution", "warning":
		return strings.ToUpper(name[:1]) + name[1:] + ": "
	case "code_sample", "example", "codenew":
		if attrs["file"] != "" {
			return "Example file: " + attrs["file"]
		}
	case "figure":
		for _, k := range []string{"alt", "caption", "title"} {
			if attrs[k] != "" {
				return "Figure: " + attrs[k]
			}
		}
	}
	return ""
}

// cleanProse removes HTML and link targets, which carry no meaning for retrieval. It is not applied
// inside code.
func cleanProse(line string) string {
	line = htmlTag.ReplaceAllString(line, "")
	return mdLink.ReplaceAllString(line, "$1")
}

// frontMatter splits Hugo's YAML front matter from the body and returns the page title.
func frontMatter(text string) (title, body string) {
	if !strings.HasPrefix(text, "---\n") {
		return "", text
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return "", text
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if v, ok := strings.CutPrefix(line, "title:"); ok {
			title = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return title, text[4+end+5:]
}

// blocks splits a body into paragraphs, headings and whole fenced code blocks, each cleaned.
func blocks(body string) []string {
	body = dropped.ReplaceAllString(body, "")
	body = htmlComment.ReplaceAllString(body, "")
	// Over the whole body, because a shortcode can span lines, and Hugo expands them inside code too.
	body = shortcode.ReplaceAllStringFunc(body, replaceShortcode)
	var out []string
	var cur []string
	fence := ""
	flush := func() {
		if text := strings.TrimSpace(strings.Join(cur, "\n")); text != "" {
			out = append(out, text)
		}
		cur = nil
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "":
			cur = append(cur, line)
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
				flush()
			}
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flush()
			fence = trimmed[:3]
			cur = append(cur, line)
		case trimmed == "":
			flush()
		case headingLine.MatchString(trimmed):
			flush()
			cur = append(cur, cleanProse(trimmed))
			flush()
		default:
			cur = append(cur, cleanProse(line))
		}
	}
	flush()
	return out
}

// split cuts a block longer than maxChars at line ends, or within a line if it must.
func split(block string) []string {
	var out []string
	cur := ""
	for _, line := range strings.Split(block, "\n") {
		for utf8.RuneCountInString(line) > maxChars {
			if cur != "" {
				out, cur = append(out, cur), ""
			}
			r := []rune(line)
			out, line = append(out, string(r[:maxChars])), string(r[maxChars:])
		}
		switch {
		case cur == "":
			cur = line
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(line) > maxChars:
			out, cur = append(out, cur), line
		default:
			cur += "\n" + line
		}
	}
	if strings.TrimSpace(cur) != "" {
		out = append(out, cur)
	}
	return out
}

// Chunks cuts one Markdown page into chunks of at most maxChars, each starting with the page title.
// A heading starts a new chunk once the current one has minChars, so short sections are merged.
// The same input always gives the same chunks.
func Chunks(text string) []string {
	title, body := frontMatter(strings.ReplaceAll(text, "\r\n", "\n"))
	var chunks []string
	cur := ""
	emit := func() {
		if strings.TrimSpace(cur) != "" {
			chunks = append(chunks, cur)
		}
		cur = ""
	}
	for _, b := range blocks(body) {
		if headingLine.MatchString(b) && utf8.RuneCountInString(cur) >= minChars {
			emit()
		}
		for _, part := range split(b) {
			if cur != "" && utf8.RuneCountInString(cur)+2+utf8.RuneCountInString(part) > maxChars {
				emit()
			}
			if cur == "" {
				cur = part
			} else {
				cur += "\n\n" + part
			}
		}
	}
	emit()
	if title == "" {
		return chunks
	}
	for i, c := range chunks {
		chunks[i] = title + "\n\n" + c
	}
	return chunks
}
