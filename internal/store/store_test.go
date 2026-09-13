package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestIncrementalUpsert(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, changed, err := db.UpsertDoc(DocInput{URL: "https://a.test/x", SHA: "aaa", Title: "T"})
	if err != nil || !changed {
		t.Fatalf("first insert: changed=%v err=%v", changed, err)
	}
	if _, changed, err := db.UpsertDoc(DocInput{URL: "https://a.test/x", SHA: "aaa"}); err != nil || changed {
		t.Fatalf("same sha must be unchanged: changed=%v err=%v", changed, err)
	}
	if _, changed, err := db.UpsertDoc(DocInput{URL: "https://a.test/x", SHA: "bbb"}); err != nil || !changed {
		t.Fatalf("new sha must change: changed=%v err=%v", changed, err)
	}
	// chunks of changed doc invalidated
	if ids, _, _ := db.DocChunks(id); len(ids) != 0 {
		t.Fatalf("chunks should be cleared on content change, got %d", len(ids))
	}
	_ = ctx
}

func TestFTSAndHybrid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, _, err := db.UpsertDoc(DocInput{URL: "https://a.test/x", SHA: "s1", Title: "Go guide"})
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"Golang generics introduce type parameters for functions and types.",
		"Cgo allows calling C code from Go programs with the cgo directive.",
	}
	if err := db.ReplaceChunks(id, texts, []int{0, 55}); err != nil {
		t.Fatal(err)
	}
	ids, err := db.KeywordSearch(ctx, "generics", 10)
	if err != nil || len(ids) == 0 {
		t.Fatalf("keyword search failed: %v %v", ids, err)
	}
	hits, err := db.Enrich(ids)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Text, "generics") {
		t.Fatalf("enrich mismatch: %+v %v", hits, err)
	}
	if hits[0].URL != "https://a.test/x" {
		t.Fatalf("missing provenance: %+v", hits[0])
	}

	// vector path
	cids, _, _ := db.DocChunks(id)
	vecs := map[int64][]float32{}
	for i, cid := range cids {
		v := []float32{1, float32(i), 0}
		vecs[cid] = v
	}
	if err := db.SaveVectors("fake-model", vecs); err != nil {
		t.Fatal(err)
	}
	vhits, err := db.VectorSearch(ctx, []float32{1, 1, 0}, 10)
	if err != nil || len(vhits) != 2 {
		t.Fatalf("vector search: %v %v", vhits, err)
	}
	if vhits[0] != cids[1] {
		t.Errorf("expected closer vector first")
	}
	merged := Hybrid(ids, vhits, 60, 5)
	if len(merged) != 2 {
		t.Fatalf("hybrid should merge both lists, got %+v", merged)
	}
	if merged[0].Score < merged[1].Score {
		t.Error("hybrid not sorted by rrf score")
	}
}

func TestQueryEscaping(t *testing.T) {
	db := openTestDB(t)
	if _, _, err := db.UpsertDoc(DocInput{URL: "https://a.test/1", SHA: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceChunks(1, []string{`contains quotes "and" parens (fn)`}, nil); err != nil {
		t.Fatal(err)
	}
	ids, err := db.KeywordSearch(context.Background(), `quotes "and" parens`, 5)
	if err != nil {
		t.Fatalf("fts query must not error on hostile input: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected match, got %v", ids)
	}
}
