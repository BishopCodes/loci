package chunk

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitBoundaries(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "## section %d\n", i)
		sb.WriteString(strings.Repeat("lorem ipsum dolor sit amet. ", 60) + "\n\n")
	}
	text := sb.String()
	pieces := Split(text, 3200, 400)
	if len(pieces) < 4 {
		t.Fatalf("expected multiple chunks, got %d", len(pieces))
	}
	for _, p := range pieces {
		if len([]rune(p.Text)) > 3200 {
			t.Errorf("chunk %d exceeds max: %d runes", p.Seq, len([]rune(p.Text)))
		}
		if strings.Contains(p.Text, "##") && strings.Count(p.Text, "\n##") > len(pieces) {
			t.Errorf("chunk %d oddly cut", p.Seq)
		}
	}
	// coverage: concatenating windows must contain everything
	joined := strings.Join(texts(pieces), "\n")
	for _, probe := range []string{"section 0", "section 59"} {
		if !strings.Contains(joined, probe) {
			t.Errorf("chunking lost %q", probe)
		}
	}
}

func TestSplitSmall(t *testing.T) {
	if got := Split("short text", 3200, 400); len(got) != 1 || got[0].Text != "short text" {
		t.Fatalf("bad split: %+v", got)
	}
	if got := Split("", 3200, 400); got != nil {
		t.Fatalf("expected nil for empty, got %+v", got)
	}
}

func texts(cs []Chunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Text
	}
	return out
}
