// Package chunk splits extracted documents into embedding/retrieval-friendly
// pieces, preferring heading and paragraph boundaries.
package chunk

import "strings"

// Chunk is one retrieval unit of a document.
type Chunk struct {
	Seq       int
	Text      string
	StartByte int
}

// Split windows text into chunks of at most maxRunes runes with overlap
// runes, cutting at markdown headings, then blank lines, then spaces.
func Split(text string, maxRunes, overlap int) []Chunk {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = 3200
	}
	if overlap < 0 || overlap >= maxRunes {
		overlap = maxRunes / 8
	}
	var out []Chunk
	pos := 0
	for pos < len(runes) {
		end := pos + maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		if end < len(runes) {
			end = bestCut(runes, pos, end)
		}
		seg := strings.TrimSpace(string(runes[pos:end]))
		if seg != "" {
			out = append(out, Chunk{Seq: len(out), Text: seg, StartByte: runePosToByte(text, pos)})
		}
		if end == len(runes) {
			break
		}
		next := end - overlap
		if next <= pos { // guarantee progress
			next = end
		}
		pos = next
	}
	return out
}

// bestCut finds the latest good boundary within [pos+maxRunes/2, end].
func bestCut(runes []rune, pos, end int) int {
	floor := pos + (end-pos)/2
	window := string(runes[floor:end])
	// 1. markdown heading near the end
	if i := lastHeadingBoundary(window); i > 0 {
		return floor + i
	}
	// 2. paragraph break
	if i := strings.LastIndex(window, "\n\n"); i > 0 {
		return floor + i
	}
	// 3. any newline
	if i := strings.LastIndex(window, "\n"); i > 0 {
		return floor + i
	}
	// 4. word boundary
	if i := strings.LastIndex(window, " "); i > 0 {
		return floor + i
	}
	return end
}

func lastHeadingBoundary(window string) int {
	best := -1
	for _, nl := range []string{"\n## ", "\n# ", "\n### "} {
		if i := strings.LastIndex(window, nl); i > best {
			best = i + 1 // keep the newline
		}
	}
	return best
}

func runePosToByte(s string, runePos int) int {
	count := 0
	for i := range s {
		if count == runePos {
			return i
		}
		count++
	}
	return len(s)
}
