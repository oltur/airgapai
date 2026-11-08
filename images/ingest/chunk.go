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
	// Only HTML tags. Placeholders such as <namespace> look like tags, and must stay.
	htmlTag     = regexp.MustCompile(`</?(?:a|b|br|caption|code|del|details|div|em|hr|i|img|li|ol|p|small|span|strong|sub|summary|sup|table|tbody|td|th|thead|tr|tt|u|ul)(?:\s[^>]*)?/?>`)
	mdLink      = regexp.MustCompile(`\[([^\]]+)\]\([^)\s]+\)`)
	dropped     = regexp.MustCompile(`(?s)\{\{<\s*(comment|mermaid)\s*>\}\}.*?\{\{<\s*/(comment|mermaid)\s*>\}\}`)
	headingLine = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	frontRegexp = regexp.MustCompile(`(?s)^---\n(?:(.*?)\n)?---(?:\n|$)`)
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
		return attrs["term_id"]
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
	m := frontRegexp.FindStringSubmatchIndex(text)
	if m == nil {
		return "", text
	}
	if m[2] >= 0 {
		for _, line := range strings.Split(text[m[2]:m[3]], "\n") {
			if v, ok := strings.CutPrefix(line, "title:"); ok {
				title = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return title, text[m[1]:]
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
// A heading starts a new chunk once the current one has minChars, so short sections are merged, and a
// heading is never left at the end of a chunk without its section. The same input always gives the
// same chunks.
func Chunks(text string) []string {
	title, body := frontMatter(strings.ReplaceAll(text, "\r\n", "\n"))
	var chunks []string
	var cur []string // the parts of the chunk being built
	size := func() int { return utf8.RuneCountInString(strings.Join(cur, "\n\n")) }
	emit := func() {
		if text := strings.Join(cur, "\n\n"); strings.TrimSpace(text) != "" {
			chunks = append(chunks, text)
		}
		cur = nil
	}
	for _, b := range blocks(body) {
		if headingLine.MatchString(b) && size() >= minChars {
			emit()
		}
		for _, part := range split(b) {
			if len(cur) > 0 && size()+2+utf8.RuneCountInString(part) > maxChars {
				// Headings at the end move to the next chunk, with the section they introduce.
				n := len(cur)
				for n > 0 && headingLine.MatchString(cur[n-1]) {
					n--
				}
				if n == 0 {
					n = len(cur)
				}
				carried := append([]string(nil), cur[n:]...)
				cur = cur[:n]
				emit()
				cur = carried
			}
			cur = append(cur, part)
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
