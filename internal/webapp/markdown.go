package webapp

import (
	"bytes"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"gopkg.in/yaml.v3"
)

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
		// 150 is load-bearing: below goldmark's link parser (200) so `[[`
		// is claimed before the ordinary link syntax sees the first `[`,
		// above its code span parser (100) so backticks still swallow
		// their contents whole.
		parser.WithInlineParsers(util.Prioritized(wikiParser{}, 150)),
	),
)

// wikiParser expands Obsidian-style [[target]] and [[target|label]] to
// <a href="wiki:target">, which the frontend resolves against the file tree
// by basename (FileView.tsx).
//
// It is an inline parser rather than a rewrite of the raw source so that code
// never sees it. goldmark runs no inline parser over a fenced or indented code
// block, and a code span outranks this one, so `[[x]]` inside backticks or a
// ``` block survives verbatim — which is the whole point: Mermaid's subroutine
// node shape is written B[[label]], and rewriting it handed Mermaid a diagram
// that could not parse.
//
// Two consequences of parsing instead of rewriting, both matching Obsidian:
// a label is literal text ([[t|*v*]] renders *v*, not <em>v</em>), and a
// wikilink cannot span a newline.
type wikiParser struct{}

func (wikiParser) Trigger() []byte { return []byte{'['} }

// Parse matches [[target]] / [[target|label]] on the current line. Neither
// half may contain `]` — the first `]` must be the closer — which is what
// keeps a destination-breaking target like [[a) [pwn](javascript:...)]]
// literal text, exactly as the regex this replaced left it.
func (wikiParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, seg := block.PeekLine()
	if len(line) < 5 || line[1] != '[' { // [[x]] is the shortest form
		return nil
	}
	body := line[2:]
	i := bytes.IndexByte(body, ']')
	if i < 1 || i+1 >= len(body) || body[i+1] != ']' {
		return nil
	}
	target, label := body[:i], body[:i]
	if j := bytes.IndexByte(target, '|'); j >= 0 {
		target, label = target[:j], target[j+1:]
	}
	if len(target) == 0 || len(label) == 0 {
		return nil
	}
	// The label is a segment into the source, not a new string, so goldmark's
	// own text renderer escapes it the way it escapes every other text node.
	// It is a suffix of body[:i], so its offset falls out of the lengths.
	labelStart := seg.Start + 2 + i - len(label)
	block.Advance(i + 4) // "[[" + body[:i] + "]]"
	link := ast.NewLink()
	link.Destination = []byte("wiki:" + url.PathEscape(string(target)))
	link.AppendChild(link, ast.NewTextSegment(text.NewSegment(labelStart, labelStart+len(label))))
	return link
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
	if err := md.Convert(body, &buf); err != nil {
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
