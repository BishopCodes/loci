package htmlmd

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func parse(t *testing.T, s string) *html.Node {
	t.Helper()
	n, err := html.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConvertBasics(t *testing.T) {
	doc := `<html><body>
		<h1>Title</h1>
		<p>Hello <strong>bold</strong> and <em>ital</em> <a href="https://a.test/x">link</a>.</p>
		<ul><li>one</li><li>two</li></ul>
		<ol><li>first</li><li>second</li></ol>
		<pre><code class="language-go">fmt.Println("x")</code></pre>
		<blockquote>quoted</blockquote>
		<table><tr><th>h1</th><th>h2</th></tr><tr><td>a</td><td>b|c</td></tr></table>
		<script>var evil=1;</script><style>p{color:red}</style>
	</body></html>`
	out := Convert(parse(t, doc))
	for _, want := range []string{
		"# Title", "**bold**", "*ital*", "[link](https://a.test/x)",
		"- one", "1. first", "```go", "fmt.Println(\"x\")", "```",
		"> quoted", "| h1 | h2 |", "b\\|c",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"var evil", "color:red"} {
		if strings.Contains(out, bad) {
			t.Errorf("script/style leaked: %q", bad)
		}
	}
}

func TestPlainMode(t *testing.T) {
	doc := `<h2>Section</h2><p>See <a href="https://x.test">the docs</a> for <strong>more</strong>.</p>`
	out := Plain(parse(t, doc))
	if strings.Contains(out, "#") || strings.Contains(out, "[") || strings.Contains(out, "**") || strings.Contains(out, "x.test") {
		t.Errorf("markup leaked into plain: %q", out)
	}
	for _, want := range []string{"Section", "the docs", "more"} {
		if !strings.Contains(out, want) {
			t.Errorf("plain missing %q: %q", want, out)
		}
	}
}
