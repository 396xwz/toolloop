package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// stubEmbedder returns fixed vectors keyed by exact text so ranking is
// deterministic; texts without a vector fail, simulating an embed miss.
type stubEmbedder struct {
	vecs  map[string][]float32
	calls map[string]int
}

func (s *stubEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	s.calls[text]++
	if v, ok := s.vecs[text]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("no vector for %q", text)
}

func newTestMemory(t *testing.T, vecs map[string][]float32) (*SQLiteMemory, *stubEmbedder) {
	t.Helper()
	stub := &stubEmbedder{vecs: vecs, calls: map[string]int{}}
	m, err := SQLiteMemoryV2(filepath.Join(t.TempDir(), "mem.db"), stub)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, stub
}

func TestSearchRanksByCosineSimilarity(t *testing.T) {
	m, _ := newTestMemory(t, map[string][]float32{
		"auth refactor task result": {1, 0},
		"database schema notes":     {0, 1},
		"how do I fix the auth bug": {0.9, 0.1},
	})
	ctx := context.Background()
	if err := m.Save(ctx, "t1", "task_result", "auth refactor task result"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.Save(ctx, "t2", "note", "database schema notes"); err != nil {
		t.Fatalf("save: %v", err)
	}

	items, err := m.Search(ctx, "how do I fix the auth bug", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Content != "auth refactor task result" {
		t.Fatalf("top item = %q, want the semantically matching one", items[0].Content)
	}
	if !(items[0].Score > items[1].Score) {
		t.Fatalf("scores not descending: %v then %v", items[0].Score, items[1].Score)
	}
	if items[0].Score <= 0.9 || items[1].Score >= 0.9 {
		t.Fatalf("scores out of expected range: %v, %v", items[0].Score, items[1].Score)
	}

	top, err := m.Search(ctx, "how do I fix the auth bug", 1)
	if err != nil {
		t.Fatalf("search limit 1: %v", err)
	}
	if len(top) != 1 || top[0].Content != "auth refactor task result" {
		t.Fatalf("limit=1 got %+v, want only the top item", top)
	}
}

func TestSearchSkipsUnembeddedRows(t *testing.T) {
	m, _ := newTestMemory(t, map[string][]float32{
		"embedded item": {1, 0},
		"query":         {1, 0},
	})
	ctx := context.Background()
	if err := m.Save(ctx, "t1", "note", "embedded item"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.Save(ctx, "t2", "note", "no vector here"); err != nil {
		t.Fatalf("save: %v", err)
	}

	items, err := m.Search(ctx, "query", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 || items[0].Content != "embedded item" {
		t.Fatalf("got %+v, want only the embedded row", items)
	}
}

func TestSearchPropagatesQueryEmbedError(t *testing.T) {
	m, _ := newTestMemory(t, map[string][]float32{})
	if _, err := m.Search(context.Background(), "unknown query", 5); err == nil {
		t.Fatal("search: want error when the query cannot be embedded")
	}
}

func TestRecentUnaffectedByEmbeddings(t *testing.T) {
	m, _ := newTestMemory(t, map[string][]float32{
		"first":  {1, 0},
		"second": {0, 1},
	})
	ctx := context.Background()
	if err := m.Save(ctx, "t1", "note", "first"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.Save(ctx, "t2", "note", "second"); err != nil {
		t.Fatalf("save: %v", err)
	}

	items, err := m.Recent(ctx, 5)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Content != "second" || items[1].Content != "first" {
		t.Fatalf("recent order = %q, %q; want newest first", items[0].Content, items[1].Content)
	}
	for _, it := range items {
		if it.Score != 0 {
			t.Fatalf("recent item %q has score %v, want 0", it.Content, it.Score)
		}
	}
}

func TestBackfillEmbeddings(t *testing.T) {
	m, stub := newTestMemory(t, map[string][]float32{
		"legacy row": {0.5, 0.5},
	})
	ctx := context.Background()
	if _, err := m.db.Exec(`INSERT INTO memory(task_id, kind, content, created_at) VALUES('t1','note','legacy row','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := m.BackfillEmbeddings(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var emb string
	if err := m.db.QueryRowContext(ctx, `SELECT COALESCE(embedding,'') FROM memory WHERE content='legacy row'`).Scan(&emb); err != nil {
		t.Fatalf("read embedding: %v", err)
	}
	if emb == "" {
		t.Fatal("embedding still empty after backfill")
	}
	var vec []float32
	if err := json.Unmarshal([]byte(emb), &vec); err != nil {
		t.Fatalf("unmarshal embedding: %v", err)
	}
	if len(vec) != 2 || vec[0] != 0.5 || vec[1] != 0.5 {
		t.Fatalf("embedding = %v, want [0.5 0.5]", vec)
	}
	if stub.calls["legacy row"] != 1 {
		t.Fatalf("embed calls = %d, want 1", stub.calls["legacy row"])
	}

	// Second run is a no-op: the row now has an embedding.
	if err := m.BackfillEmbeddings(ctx); err != nil {
		t.Fatalf("backfill again: %v", err)
	}
	if stub.calls["legacy row"] != 1 {
		t.Fatalf("embed calls after re-run = %d, want still 1", stub.calls["legacy row"])
	}
}

func TestOpenBackfillsLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`
CREATE TABLE memory (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  content TEXT NOT NULL,
  created_at TEXT NOT NULL
);
INSERT INTO memory(task_id, kind, content, created_at) VALUES('t1','note','legacy note','2026-01-01T00:00:00Z');
`); err != nil {
		db.Close()
		t.Fatalf("seed legacy db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stub := &stubEmbedder{vecs: map[string][]float32{"legacy note": {1, 0}}, calls: map[string]int{}}
	m, err := SQLiteMemoryV2(path, stub)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	items, err := m.Search(context.Background(), "legacy note", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 1 || items[0].Content != "legacy note" {
		t.Fatalf("got %+v, want the backfilled legacy row", items)
	}
	if items[0].Score < 1-1e-9 {
		t.Fatalf("score = %v, want 1 (identical vectors)", items[0].Score)
	}
}
