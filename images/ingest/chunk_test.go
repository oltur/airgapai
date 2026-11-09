package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

const page = `---
title: "Pod Security Standards"
weight: 15
---

<!-- overview -->

The {{< glossary_tooltip text="Pod" term_id="pod" >}} rules. See [the API](/docs/reference/).
Stored in {{<glossary_tooltip
term_id="etcd" text="etcd">}} and {{<
glossary_tooltip term_id="kube-apiserver" >}}.

{{< note >}}
A <strong>note</strong>.
{{< /note >}}

## {{% heading "whatsnext" %}}

{{% code_sample file="pods/simple-pod.yaml" %}}

{{< comment >}}
hidden text
{{< /comment >}}

` + "```yaml\nkind: Pod\n\nmetadata: {name: x}\n```\n"

func TestChunksCleansMarkup(t *testing.T) {
	got := Chunks(page)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1: %q", len(got), got)
	}
	c := got[0]
	for _, want := range []string{"Pod Security Standards\n\n", "The Pod rules. See the API.\nStored in etcd and kube-apiserver.", "Note: \nA note.",
		"## What's next", "Example file: pods/simple-pod.yaml", "```yaml\nkind: Pod\n\nmetadata: {name: x}\n```"} {
		if !strings.Contains(c, want) {
			t.Errorf("chunk lacks %q:\n%s", want, c)
		}
	}
	for _, unwanted := range []string{"{{", "<!--", "hidden text", "weight:", "<strong>"} {
		if strings.Contains(c, unwanted) {
			t.Errorf("chunk still has %q:\n%s", unwanted, c)
		}
	}
}

func TestChunksRespectSizeAndAreStable(t *testing.T) {
	var b strings.Builder
	b.WriteString("---\ntitle: Long\n---\n")
	for i := 0; i < 40; i++ {
		b.WriteString("## Section\n\n" + strings.Repeat("word ", 120) + "\n\n")
	}
	b.WriteString(strings.Repeat("x", 4000) + "\n")
	first, second := Chunks(b.String()), Chunks(b.String())
	if !reflect.DeepEqual(first, second) {
		t.Fatal("the same input gave different chunks")
	}
	if len(first) < 10 {
		t.Fatalf("got only %d chunks", len(first))
	}
	for i, c := range first {
		body := strings.TrimPrefix(c, "Long\n\n")
		if n := utf8.RuneCountInString(body); n > maxChars {
			t.Errorf("chunk %d has %d characters", i, n)
		}
	}
}

func TestPlaceholdersStay(t *testing.T) {
	got := Chunks("Run `kubectl logs <pod-name> -n <namespace>` in a <strong>shell</strong>.<br/>\n")
	if len(got) != 1 || got[0] != "Run `kubectl logs <pod-name> -n <namespace>` in a shell." {
		t.Fatalf("got %q", got)
	}
}

func TestHeadingStaysWithItsSection(t *testing.T) {
	text := strings.Repeat("a", 300) + "\n\n## Limits\n\n" + strings.Repeat("b", 1300) + "\n"
	got := Chunks(text)
	if len(got) != 2 || !strings.HasPrefix(got[1], "## Limits\n\nbbb") || strings.Contains(got[0], "Limits") {
		t.Fatalf("got %q", got)
	}
}

func TestFrontMatterEdges(t *testing.T) {
	for in, want := range map[string]string{
		"---\n---\nBody.\n":             "Body.",
		"---\ntitle: T\n---":            "",
		"---\ntitle: T\n---\nBody.":     "T\n\nBody.",
		"No front matter.\n---\nAfter.": "No front matter.\n---\nAfter.",
	} {
		got := strings.Join(Chunks(in), "|")
		if got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestShortSectionsAreMerged(t *testing.T) {
	got := Chunks("## A\n\nshort\n\n## B\n\nalso short\n")
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1: %q", len(got), got)
	}
}

func TestEmptyPageHasNoChunks(t *testing.T) {
	if got := Chunks("---\ntitle: Empty\n---\n\n<!-- overview -->\n"); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestClassify(t *testing.T) {
	restricted := []string{"content/en/docs/concepts/security/"}
	for path, want := range map[string]string{
		"content/en/docs/concepts/security/rbac-good-practices.md": "restricted",
		"content/en/docs/concepts/policy/limit-range.md":           "public",
		"content/en/docs/concepts/securityx.md":                    "public",
	} {
		if got := classify(path, restricted); got != want {
			t.Errorf("%s: got %s, want %s", path, got, want)
		}
	}
	if got := classify("content/en/docs/concepts/security/x.md", nil); got != "public" {
		t.Errorf("with no prefixes: got %s", got)
	}
}

func TestLoadKeepsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "content", "a", "page.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("---\ntitle: T\n---\n\nText.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pages, err := load(config{corpus: dir, restricted: []string{"content/a/"}})
	if err != nil {
		t.Fatal(err)
	}
	cs := pages["content/a/page.md"]
	if len(cs) != 1 || cs[0].classification != "restricted" || cs[0].content != "T\n\nText." || len(cs[0].hash) != 64 {
		t.Fatalf("got %+v", pages)
	}
}

func TestVectorText(t *testing.T) {
	if got := vectorText([]float32{0.5, -1, 0.1}); got != "[0.5,-1,0.1]" {
		t.Fatalf("got %s", got)
	}
}
