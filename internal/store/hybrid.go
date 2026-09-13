package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Hit is a retrieval result enriched with provenance.
type Hit struct {
	ChunkID   int64   `json:"chunk_id"`
	DocID     int64   `json:"doc_id"`
	Text      string  `json:"text"`
	URL       string  `json:"url"`
	Title     string  `json:"title,omitempty"`
	DocType   string  `json:"doc_type,omitempty"`
	FetchedAt string  `json:"fetched_at,omitempty"`
	SHA       string  `json:"sha256,omitempty"`
	MatchedBy string  `json:"matched_by"` // keyword | vector | both
	Score     float64 `json:"score"`
	Rank      int     `json:"-"`
}

// KeywordSearch runs BM25 via FTS5. Returns chunk ids ordered by relevance.
func (db *DB) KeywordSearch(ctx context.Context, query string, limit int) ([]int64, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sqlDB.QueryContext(ctx,
		`SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY rank LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// VectorSearch ranks stored chunks by cosine similarity to qv.
func (db *DB) VectorSearch(ctx context.Context, qv []float32, limit int) ([]int64, error) {
	if len(qv) == 0 {
		return nil, nil
	}
	db.vecMu.RLock()
	err := db.loadVecCacheLocked()
	db.vecMu.RUnlock()
	if err != nil {
		return nil, err
	}
	type scored struct {
		id    int64
		score float64
	}
	var all []scored
	db.vecMu.RLock()
	for id, v := range db.vecCache {
		all = append(all, scored{id, cosine(qv, v)})
	}
	db.vecMu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	ids := make([]int64, 0, limit)
	for _, s := range all[:limit] {
		if s.score <= 0 {
			break
		}
		ids = append(ids, s.id)
	}
	return ids, nil
}

// Enrich loads chunk rows plus their document provenance.
func (db *DB) Enrich(ids []int64) ([]Hit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	hits := make([]Hit, 0, len(ids))
	for _, id := range ids {
		var h Hit
		err := db.sqlDB.QueryRow(
			`SELECT c.id, c.doc_id, c.text, d.url, d.title, d.doc_type,
			        datetime(d.fetched_at), d.sha256
			 FROM chunks c JOIN docs d ON d.id = c.doc_id WHERE c.id = ?`, id).
			Scan(&h.ChunkID, &h.DocID, &h.Text, &h.URL, &h.Title, &h.DocType, &h.FetchedAt, &h.SHA)
		if err != nil {
			continue
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// Hybrid combines BM25 and vector rankings with Reciprocal Rank Fusion.
// Either list may be empty. k is the RRF constant (default 60).
func Hybrid(keywordIDs, vectorIDs []int64, k int, top int) []Hit {
	if k <= 0 {
		k = 60
	}
	scores := map[int64]float64{}
	matched := map[int64]string{}
	for rank, id := range keywordIDs {
		scores[id] += 1.0 / float64(k+rank+1)
		matched[id] = "keyword"
	}
	for rank, id := range vectorIDs {
		scores[id] += 1.0 / float64(k+rank+1)
		if m, ok := matched[id]; ok && m == "keyword" {
			matched[id] = "both"
		} else if _, ok := matched[id]; !ok {
			matched[id] = "vector"
		}
	}
	type kv struct {
		id    int64
		score float64
	}
	all := make([]kv, 0, len(scores))
	for id, s := range scores {
		all = append(all, kv{id, s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if top > 0 && top < len(all) {
		all = all[:top]
	}
	hits := make([]Hit, 0, len(all))
	for i, e := range all {
		hits = append(hits, Hit{ChunkID: e.id, Score: e.score, MatchedBy: matched[e.id], Rank: i + 1})
	}
	return hits
}

// ftsQuery converts free text into a safe FTS5 MATCH expression: quoted,
// lower-cased alphanumeric words OR-ed together and ranked by BM25, max 10 words.
func ftsQuery(q string) string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r >= 0x80)
	})
	if len(fields) > 10 {
		fields = fields[:10]
	}
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, `""`)
		quoted = append(quoted, `"`+f+`"`)
	}
	return strings.Join(quoted, " OR ")
}
