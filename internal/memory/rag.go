package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	_ "modernc.org/sqlite"
)

type RAG interface {
	IndexPath(ctx context.Context, root string) (int, error)
	Retrieve(ctx context.Context, query string, topK int) ([]RAGChunk, error)
	Close() error
}

type RAGChunk struct {
	ID       int64
	Path     string
	ChunkIdx int
	Text     string
	Score    float64
}

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

type OllamaEmbedder struct {
	Client *api.Client
	Model  string // e.g. "nomic-embed-text"
}

func (e *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.Client.Embed(ctx, &api.EmbedRequest{
		Model: e.Model,
		Input: text,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	return resp.Embeddings[0], nil
}

type SQLiteRAG struct {
	db       *sql.DB
	embedder Embedder
}

func NewSQLiteRAG(path string, embedder Embedder) (*SQLiteRAG, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	r := &SQLiteRAG{db: db, embedder: embedder}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

func (r *SQLiteRAG) migrate() error {
	_, err := r.db.Exec(`
CREATE TABLE IF NOT EXISTS chunks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  path TEXT NOT NULL,
  chunk_idx INTEGER NOT NULL,
  text TEXT NOT NULL,
  embedding TEXT NOT NULL,
  UNIQUE(path, chunk_idx)
);
CREATE INDEX IF NOT EXISTS idx_chunks_path ON chunks(path);
`)
	return err
}

func (r *SQLiteRAG) Close() error { return r.db.Close() }

// IndexPath walks root and indexes text-like files.
func (r *SQLiteRAG) IndexPath(ctx context.Context, root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".md", ".txt", ".json", ".go", ".py", ".yaml", ".yml", ".toml":
			// ok
		default:
			return nil
		}
		n, err := r.indexFile(ctx, path)
		if err != nil {
			return nil // skip bad files
		}
		count += n
		return nil
	})
	return count, err
}

func (r *SQLiteRAG) indexFile(ctx context.Context, path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if !utf8.Valid(data) || len(data) == 0 {
		return 0, nil
	}
	text := string(data)
	chunks := chunkText(text, 1200, 200)
	for i, ch := range chunks {
		emb, err := r.embedder.Embed(ctx, ch)
		if err != nil {
			return i, err
		}
		b, _ := json.Marshal(emb)
		_, err = r.db.ExecContext(ctx, `
INSERT INTO chunks(path, chunk_idx, text, embedding) VALUES(?,?,?,?)
ON CONFLICT(path, chunk_idx) DO UPDATE SET text=excluded.text, embedding=excluded.embedding
`, path, i, ch, string(b))
		if err != nil {
			return i, err
		}
	}
	return len(chunks), nil
}

func (r *SQLiteRAG) Retrieve(ctx context.Context, query string, topK int) ([]RAGChunk, error) {
	if topK <= 0 {
		topK = 5
	}
	qEmb, err := r.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}

	rows, err := r.db.QueryContext(ctx, `SELECT id, path, chunk_idx, text, embedding FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		c     RAGChunk
		score float64
	}
	var all []scored
	for rows.Next() {
		var id int64
		var path, text, embJSON string
		var idx int
		if err := rows.Scan(&id, &path, &idx, &text, &embJSON); err != nil {
			continue
		}
		var emb []float32
		if err := json.Unmarshal([]byte(embJSON), &emb); err != nil {
			continue
		}
		score := cosine(qEmb, emb)
		all = append(all, scored{
			c:     RAGChunk{ID: id, Path: path, ChunkIdx: idx, Text: text, Score: score},
			score: score,
		})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > topK {
		all = all[:topK]
	}
	out := make([]RAGChunk, 0, len(all))
	for _, s := range all {
		out = append(out, s.c)
	}
	return out, nil
}

func chunkText(s string, size, overlap int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 {
		overlap = 0
	}
	var chunks []string
	runes := []rune(s)
	for start := 0; start < len(runes); {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[start:end]))
		if end == len(runes) {
			break
		}
		start = end - overlap
		if start < 0 {
			start = 0
		}
	}
	return chunks
}

func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// In-memory stub
type InMemoryRAG struct{}

func (r *InMemoryRAG) IndexPath(ctx context.Context, root string) (int, error) { return 0, nil }
func (r *InMemoryRAG) Retrieve(ctx context.Context, query string, topK int) ([]RAGChunk, error) {
	return nil, nil
}
func (r *InMemoryRAG) Close() error { return nil }

var _ RAG = (*SQLiteRAG)(nil)
var _ RAG = (*InMemoryRAG)(nil)
