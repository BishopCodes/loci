package search

import (
	"strings"
	"testing"
)

const ddgHTMLFixture = `<html><body>
<div class="result"><div class="result__body"><h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fgo%2Fpage&rut=abc">Go Language</a></h2>
<a class="result__snippet" href="...">The Go programming language</a></div></div>
<div class="result"><div class="result__body"><h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fx">Another</a></h2>
<a class="result__snippet" href="...">Second snippet</a></div></div>
</body></html>`

const ddgLiteFixture = `<html><body><table>
<tr><td><a class="result-link" href="https://lite.duckduckgo.com/l/?uddg=https%3A%2F%2Fex.io%2Fp">Lite result</a></td></tr>
</table></body></html>`

const mojeekFixture = `<html><body><ul class="results-list">
<li><a class="ob" href="https://mo.example/art">Mojeek Result</a><p class="s">snippet here</p></li>
</ul></body></html>`

const bingFixture = `<html><body><ol id="b_results">
<li class="b_algo"><h2><a href="https://bing.example/page">Bing Hit</a></h2>
<div class="b_caption"><p>caption text</p></div></li>
</ol></body></html>`

const anomalyFixture = `<html><head><title>Blocked</title></head><body>
<div id="anomaly-modal">If this persists, please send us a message.<div class="challenge">x</div></div></body></html>`

func TestParseDDGHTMLScoping(t *testing.T) {
	res := parseDDGHTML(ddgHTMLFixture)
	if len(res) == 2 && res[1].Snippet != "Second snippet" {
		t.Errorf("snippet leaked across results: %+v", res[1])
	}
}

func TestParseDDGHTML(t *testing.T) {
	res := parseDDGHTML(ddgHTMLFixture)
	if len(res) != 2 {
		t.Fatalf("got %d results", len(res))
	}
	if res[0].URL != "https://example.com/go/page" {
		t.Errorf("uddg unwrap failed: %s", res[0].URL)
	}
	if res[1].Title != "Another" {
		t.Errorf("title: %s", res[1].Title)
	}
}

func TestParseDDGLite(t *testing.T) {
	res := parseDDGLite(ddgLiteFixture)
	if len(res) != 1 || res[0].URL != "https://ex.io/p" {
		t.Fatalf("bad parse: %+v", res)
	}
}

func TestParseMojeekBing(t *testing.T) {
	res := parseMojeek(mojeekFixture)
	if len(res) != 1 || res[0].URL != "https://mo.example/art" || res[0].Snippet != "snippet here" {
		t.Fatalf("mojeek: %+v", res)
	}
	rb := parseBing(bingFixture)
	if len(rb) != 1 || rb[0].URL != "https://bing.example/page" {
		t.Fatalf("bing: %+v", rb)
	}
}

func TestChallengeFixtureYieldsNothing(t *testing.T) {
	for _, fn := range []func(string) []Result{parseDDGHTML, parseDDGLite, parseMojeek} {
		if got := fn(anomalyFixture); len(got) != 0 {
			t.Errorf("challenge page yielded results: %+v", got)
		}
	}
	if !strings.Contains(anomalyFixture, "anomaly") {
		t.Fatal("fixture invalid")
	}
}

func TestDedupe(t *testing.T) {
	in := []Result{
		{URL: "https://www.a.test/x"},
		{URL: "https://a.test/x#frag"},
		{URL: "https://b.test/y"},
	}
	out := dedupe(in, 10)
	if len(out) != 2 {
		t.Fatalf("dedupe failed: %+v", out)
	}
}
