package webapp

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"gopkg.in/yaml.v3"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// wikiRe matches Obsidian-style [[target]] and [[target|label]] links.
var wikiRe = regexp.MustCompile(`\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)

// expandWikilinks rewrites [[target]] to a markdown link with a wiki: URL;
// the frontend resolves the target against the file tree by basename.
func expandWikilinks(src []byte) []byte {
	return wikiRe.ReplaceAllFunc(src, func(m []byte) []byte {
		g := wikiRe.FindSubmatch(m)
		target, label := g[1], g[2]
		if len(label) == 0 {
			label = target
		}
		return []byte("[" + string(label) + "](wiki:" + url.PathEscape(string(target)) + ")")
	})
}

// RenderMarkdown converts markdown to HTML (GFM + wikilinks). Raw HTML in
// the source is escaped by goldmark's safe default. A leading YAML
// frontmatter block renders as a small key/value table instead of the
// broken thematic-break soup goldmark would make of it.
//
// This is the PUBLIC SHARE PAGE's renderer (shares.go) and its output is
// what every link ever minted serves — the table stays. The viewer uses
// RenderMarkdownPairs instead, which hands the frontmatter to the client as
// data so it can live in a side panel.
func RenderMarkdown(src []byte) (string, error) {
	table, body := frontmatterTable(src)
	out, err := renderBody(body)
	if err != nil {
		return "", err
	}
	return table + out, nil
}

// RenderMarkdownPairs is RenderMarkdown for the viewer: the frontmatter
// comes back as ordered key/value data and the HTML is body-only. Values
// are raw text (never markup) — the client escapes them by rendering them
// as text nodes.
func RenderMarkdownPairs(src []byte) ([]FrontmatterPair, string, error) {
	pairs, body := frontmatterPairs(src)
	out, err := renderBody(body)
	if err != nil {
		return nil, "", err
	}
	return pairs, out, nil
}

func renderBody(body []byte) (string, error) {
	var buf bytes.Buffer
	if err := md.Convert(expandWikilinks(body), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// fmCloseRe matches a frontmatter closing fence on its own line.
var fmCloseRe = regexp.MustCompile(`(?m)^(---|\.\.\.)\s*$`)

// FrontmatterPair is one key/value row of a document's YAML frontmatter,
// in author order. Value is plain text, never markup; Code marks the values
// the table renders inside <code> (anything nested).
type FrontmatterPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Code  bool   `json:"code,omitempty"`
}

// frontmatterPairs splits a leading YAML frontmatter block off src and
// returns its keys and values in author order. Anything that isn't a
// well-formed YAML mapping falls through untouched — a stray --- line must
// keep rendering exactly as it always did — and empty frontmatter is
// dropped from the body with nothing to show for it.
func frontmatterPairs(src []byte) ([]FrontmatterPair, []byte) {
	rest, ok := bytes.CutPrefix(src, []byte("---\n"))
	if !ok {
		if rest, ok = bytes.CutPrefix(src, []byte("---\r\n")); !ok {
			return nil, src
		}
	}
	loc := fmCloseRe.FindIndex(rest)
	if loc == nil {
		return nil, src
	}
	fm, body := rest[:loc[0]], rest[loc[1]:]
	var doc yaml.Node
	if yaml.Unmarshal(fm, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, src
	}
	m := doc.Content[0]
	if len(m.Content) == 0 {
		return nil, body // empty frontmatter: hide it, nothing to tabulate
	}
	pairs := make([]FrontmatterPair, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		value, code := yamlValue(m.Content[i+1])
		pairs = append(pairs, FrontmatterPair{Key: m.Content[i].Value, Value: value, Code: code})
	}
	return pairs, body
}

// frontmatterTable is frontmatterPairs rendered as the HTML table the share
// page has always served (keys in author order, everything escaped).
func frontmatterTable(src []byte) (string, []byte) {
	pairs, body := frontmatterPairs(src)
	if len(pairs) == 0 {
		return "", body
	}
	var b strings.Builder
	b.WriteString(`<table class="frontmatter"><tbody>`)
	for _, p := range pairs {
		val := html.EscapeString(p.Value)
		if p.Code {
			val = "<code>" + val + "</code>"
		}
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th><td>%s</td></tr>`,
			html.EscapeString(p.Key), val)
	}
	b.WriteString(`</tbody></table>`)
	return b.String(), body
}

// yamlValue renders one frontmatter value: scalars as text, flat lists
// comma-joined, anything nested as compact YAML — with code true for that
// last case, where the table reaches for <code>. Always the raw string:
// escaping belongs to whoever emits it.
func yamlValue(n *yaml.Node) (string, bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value, false
	case yaml.SequenceNode:
		flat := true
		parts := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				flat = false
				break
			}
			parts = append(parts, c.Value)
		}
		if flat {
			return strings.Join(parts, ", "), false
		}
	}
	raw, err := yaml.Marshal(n)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}
