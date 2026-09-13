// Package htmlmd converts cleaned HTML (scripts/styles already removed) into
// Markdown with a deliberately small surface: headings, paragraphs, links,
// lists, code, blockquotes, tables, images-as-refs, hr.
package htmlmd

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "iframe": true,
	"form": true, "button": true, "input": true, "select": true, "textarea": true,
	"nav": true, "footer": true, "aside": true, "dialog": true, "canvas": true,
	"template": true, "video": true, "audio": true, "source": true, "track": true,
}

var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true,
	"header": true, "blockquote": true, "pre": true, "ul": true, "ol": true,
	"table": true, "figure": true, "figcaption": true, "br": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

type writer struct {
	sb      bytes.Buffer
	inPre   int
	links   []string
	list    []listFrame
	inTable int
	plain   bool
}

type listFrame struct {
	ordered bool
	index   int
	depth   int
}

// Convert renders the subtree rooted at n as Markdown.
func Convert(n *html.Node) string { return convert(n, false) }

// Plain renders the subtree as flattened text (no markup).
func Plain(n *html.Node) string { return convert(n, true) }

func convert(n *html.Node, plain bool) string {
	w := &writer{plain: plain}
	w.walk(n, 0)
	out := w.sb.String()
	out = strings.ReplaceAll(out, "\u00a0", " ")
	out = collapseBlank(out)
	return strings.TrimSpace(out)
}

func (w *writer) walk(n *html.Node, depth int) {
	if n == nil {
		return
	}
	switch n.Type {
	case html.CommentNode:
		return
	case html.TextNode:
		w.text(n.Data)
		return
	case html.ElementNode:
		tag := strings.ToLower(n.Data)
		if skipTags[tag] {
			return
		}
		w.element(tag, n, depth)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c, depth)
	}
}

func (w *writer) element(tag string, n *html.Node, depth int) {
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level, _ := strconv.Atoi(tag[1:])
		w.newBlock()
		if !w.plain {
			w.sb.WriteString(strings.Repeat("#", level) + " ")
		}
		w.children(n, depth)
		w.endBlock()
	case "p":
		w.newBlock()
		w.children(n, depth)
		w.endBlock()
	case "br":
		w.sb.WriteString("  \n")
	case "hr":
		if w.plain {
			return
		}
		w.newBlock()
		w.sb.WriteString("---")
		w.endBlock()
	case "a":
		if w.plain {
			w.children(n, depth)
			return
		}
		href := attr(n, "href")
		start := w.sb.Len()
		w.children(n, depth)
		text := strings.TrimSpace(w.sb.String()[start:])
		if href == "" || strings.EqualFold(href, "javascript:void(0)") {
			return
		}
		w.sb.Truncate(start)
		if text == "" || text == href {
			w.sb.WriteString("<" + href + ">")
		} else {
			w.sb.WriteString("[" + nl2space(text) + "](" + href + ")")
		}
	case "img":
		if w.plain {
			return
		}
		alt := attr(n, "alt")
		src := attr(n, "src")
		if alt == "" {
			alt = "image"
		}
		if src != "" {
			w.sb.WriteString(fmt.Sprintf("![%s](%s)", alt, src))
		}
	case "strong", "b":
		w.wrap(n, depth, "**")
	case "em", "i":
		w.wrap(n, depth, "*")
	case "del", "s", "strike":
		w.wrap(n, depth, "~~")
	case "code":
		if w.inPre > 0 || w.plain {
			w.children(n, depth)
			return
		}
		w.sb.WriteString("`")
		w.children(n, depth)
		w.sb.WriteString("`")
	case "pre":
		if w.plain {
			w.newBlock()
			w.children(n, depth)
			w.endBlock()
			return
		}
		w.newBlock()
		w.inPre++
		lang := ""
		if c := findPreCode(n); c != nil {
			lang = codeLang(c)
			w.sb.WriteString("```" + lang + "\n")
			w.children(c, depth)
		} else {
			w.sb.WriteString("```\n")
			w.children(n, depth)
		}
		w.inPre--
		full := strings.TrimRight(w.sb.String(), " ")
		w.sb.Reset()
		w.sb.WriteString(full + "\n```")
		w.endBlock()
		return
	case "blockquote":
		w.newBlock()
		start := w.sb.Len()
		w.children(n, depth)
		block := w.sb.String()[start:]
		w.sb.Truncate(start)
		for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
			w.sb.WriteString("> " + line + "\n")
		}
		w.endBlock()
		return
	case "ul", "ol":
		lf := listFrame{ordered: tag == "ol", index: 1, depth: listDepth(w)}
		w.list = append(w.list, lf)
		w.children(n, depth)
		w.list = w.list[:len(w.list)-1]
		w.endBlock()
		return
	case "li":
		w.newBlock()
		ind := 0
		if len(w.list) > 0 {
			ind = (len(w.list) - 1)
		}
		w.sb.WriteString(strings.Repeat("    ", ind))
		if len(w.list) > 0 && w.list[len(w.list)-1].ordered {
			w.sb.WriteString(fmt.Sprintf("%d. ", w.list[len(w.list)-1].index))
			w.list[len(w.list)-1].index++
		} else {
			w.sb.WriteString("- ")
		}
		w.children(n, depth)
		w.endBlock()
		return
	case "table":
		w.inTable++
		w.newBlock()
		w.table(n, depth)
		w.endBlock()
		w.inTable--
		return
	case "tr":
		w.children(n, depth)
		return
	default:
		if blockTags[tag] {
			needBreak := tag == "div" || tag == "section"
			if needBreak && !w.atBlockStart() {
				w.newBlock()
				w.children(n, depth)
				if !w.atBlockStart() {
					w.endBlock()
				}
				return
			}
		}
		w.children(n, depth)
	}
}

func (w *writer) table(n *html.Node, depth int) {
	var rows [][]string
	var cur []string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch strings.ToLower(c.Data) {
			case "tr":
				if len(cur) > 0 {
					rows = append(rows, cur)
				}
				cur = nil
				walk(c)
			case "td", "th":
				start := w.sb.Len()
				w.children(c, depth)
				cell := strings.TrimSpace(w.sb.String()[start:])
				w.sb.Truncate(start)
				cur = append(cur, strings.ReplaceAll(nl2space(cell), "|", "\\|"))
			default:
				walk(c)
			}
		}
	}
	walk(n)
	if len(cur) > 0 {
		rows = append(rows, cur)
	}
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	for i, r := range rows {
		for len(r) < width {
			r = append(r, "")
		}
		if w.plain {
			w.sb.WriteString(strings.Join(r, "  ") + "\n")
			continue
		}
		w.sb.WriteString("| " + strings.Join(r, " | ") + " |\n")
		if i == 0 {
			w.sb.WriteString("|" + strings.Repeat(" --- |", width))
		}
	}
}

func (w *writer) wrap(n *html.Node, depth int, mark string) {
	if w.plain {
		mark = ""
	}
	start := w.sb.Len()
	w.children(n, depth)
	text := w.sb.String()[start:]
	if strings.TrimSpace(text) == "" {
		return
	}
	w.sb.Truncate(start)
	w.sb.WriteString(mark + nl2space(text) + mark)
}

func (w *writer) children(n *html.Node, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.walk(c, depth)
	}
}

func (w *writer) text(s string) {
	if w.inPre > 0 {
		w.sb.WriteString(s)
		return
	}
	s = strings.ReplaceAll(s, "\u00a0", " ")
	if strings.TrimSpace(s) == "" {
		if !w.atBlockStart() && w.sb.Len() > 0 && !strings.HasSuffix(w.sb.String(), " ") &&
			!strings.HasSuffix(w.sb.String(), "\n") {
			w.sb.WriteString(" ")
		}
		return
	}
	s = wsRe.ReplaceAllString(s, " ")
	if w.atBlockStart() {
		s = strings.TrimLeft(s, " ")
	}
	w.sb.WriteString(s)
}

var wsRe = regexp.MustCompile(`[ \t\r\n\f\v]+`)

func (w *writer) newBlock() {
	t := w.sb.String()
	if w.atBlockStart() {
		return
	}
	if !strings.HasSuffix(t, "\n\n") {
		if strings.HasSuffix(t, "\n") {
			w.sb.WriteString("\n")
		} else if t != "" {
			w.sb.WriteString("\n\n")
		}
	}
}

func (w *writer) endBlock() {
	// blocks are separated by the next newBlock call; trim trailing spaces on line
	t := w.sb.String()
	if strings.HasSuffix(t, " ") {
		w.sb.Reset()
		w.sb.WriteString(strings.TrimRight(t, " "))
	}
}

func (w *writer) atBlockStart() bool {
	t := w.sb.String()
	return t == "" || strings.HasSuffix(t, "\n\n") || strings.HasSuffix(t, "\n")
}

func listDepth(w *writer) int { return len(w.list) }

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func findPreCode(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.EqualFold(c.Data, "code") {
			return c
		}
	}
	return nil
}

func codeLang(code *html.Node) string {
	cls := attr(code, "class")
	for _, tok := range strings.Fields(cls) {
		for _, p := range []string{"language-", "lang-", "highlight-"} {
			if strings.HasPrefix(tok, p) {
				return tok[len(p):]
			}
		}
	}
	return ""
}

func nl2space(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func collapseBlank(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}
