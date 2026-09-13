// Package extract turns fetched documents into canonical text + Markdown,
// stripping scripts/styles entirely and preferring main content.
package extract

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	"dsearch/internal/extract/htmlmd"
	"dsearch/internal/fetch"
)

// Document is the extracted, canonical form ready for chunking.
type Document struct {
	Title      string
	Markdown   string
	Plaintext  string
	DocType    string
	Links      []string // absolute http(s) links found (for crawling)
	Extraction string   // "readability" | "fallback" | "pdf" | "passthrough"
	Warning    string
}

// FromDoc extracts structured text from a fetched document.
func FromDoc(doc *fetch.Doc) (*Document, error) {
	switch doc.DocType {
	case fetch.TypeHTML:
		return fromHTML(doc.Body)
	case fetch.TypePDF:
		return fromPDF(doc.Body)
	case fetch.TypeMarkdown:
		txt := string(doc.Body)
		return &Document{Title: "", Markdown: txt, Plaintext: txt, DocType: doc.DocType, Extraction: "passthrough"}, nil
	case fetch.TypeText:
		txt := string(doc.Body)
		return &Document{Markdown: txt, Plaintext: txt, DocType: doc.DocType, Extraction: "passthrough"}, nil
	default:
		return nil, fmt.Errorf("unsupported document type %q", doc.DocType)
	}
}

func fromHTML(body []byte) (*Document, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	stripNoise(doc)

	d := &Document{DocType: fetch.TypeHTML}
	// links from the full (noise-stripped) tree, before readability trims
	d.Links = collectLinks(doc)
	d.Title = pageTitle(doc)

	md := ""
	if art, err := readability.FromReader(bytes.NewReader(body), nil); err == nil && art.Node != nil {
		stripNoise(art.Node)
		md = htmlmd.Convert(art.Node)
		if d.Title == "" {
			d.Title = art.Title()
		}
		if strings.TrimSpace(md) != "" && len([]rune(md)) > 40 {
			d.Extraction = "readability"
		}
	}
	if d.Extraction == "" {
		md = htmlmd.Convert(doc)
		d.Extraction = "fallback"
	}
	d.Markdown = md
	d.Plaintext = plainFromHTMLDoc(doc)
	return d, nil
}

// stripNoise removes scripts/styles/invisible nodes from a tree, in place.
func stripNoise(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode {
			tag := strings.ToLower(c.Data)
			switch tag {
			case "script", "style", "noscript", "svg", "template", "link", "meta",
				"iframe", "canvas", "form", "button", "nav", "footer":
				n.RemoveChild(c)
				c = next
				continue
			}
			if hasStyleDisplayNone(c) || hasAttr(c, "hidden") {
				n.RemoveChild(c)
				c = next
				continue
			}
		}
		if c.FirstChild != nil {
			stripNoise(c)
		}
		c = next
	}
}

func hasStyleDisplayNone(n *html.Node) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, "style") {
			s := strings.ToLower(strings.ReplaceAll(a.Val, " ", ""))
			if strings.Contains(s, "display:none") || strings.Contains(s, "visibility:hidden") {
				return true
			}
		}
	}
	return false
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func pageTitle(n *html.Node) string {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != nil {
			return
		}
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "title") {
			found = node
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	if found == nil {
		return ""
	}
	var sb strings.Builder
	for t := found.FirstChild; t != nil; t = t.NextSibling {
		if t.Type == html.TextNode {
			sb.WriteString(t.Data)
		}
	}
	return strings.TrimSpace(sb.String())
}

func collectLinks(n *html.Node) []string {
	var links []string
	gq := goquery.NewDocumentFromNode(rootClone(n))
	seen := map[string]bool{}
	gq.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		h, _ := s.Attr("href")
		h = strings.TrimSpace(h)
		if h == "" || strings.HasPrefix(h, "#") || strings.HasPrefix(strings.ToLower(h), "javascript:") {
			return
		}
		if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
			return
		}
		if !seen[h] {
			seen[h] = true
			links = append(links, h)
		}
	})
	return links
}

// rootClone re-parses the subtree into an independent document for goquery.
func rootClone(n *html.Node) *html.Node {
	var sb strings.Builder
	if err := html.Render(&sb, n); err != nil {
		return n
	}
	clone, err := html.Parse(bytes.NewReader([]byte(sb.String())))
	if err != nil {
		return n
	}
	return clone
}

func plainFromHTMLDoc(n *html.Node) string {
	return htmlmd.Plain(n)
}

func fromPDF(body []byte) (*Document, error) {
	d := &Document{DocType: fetch.TypePDF, Extraction: "pdf"}
	if txt, err := pdftotext(body); err == nil && strings.TrimSpace(txt) != "" {
		d.Plaintext = txt
		d.Markdown = txt
		return d, nil
	}
	txt, warning, err := gopdf(body)
	if err != nil {
		return nil, fmt.Errorf("pdf extraction failed: %w", err)
	}
	d.Plaintext = txt
	d.Markdown = txt
	d.Warning = warning
	return d, nil
}

func pdftotext(body []byte) (string, error) {
	path, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(path, "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w: %s", err, errb.String())
	}
	return out.String(), nil
}
