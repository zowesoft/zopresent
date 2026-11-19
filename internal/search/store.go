package search

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// Doc is one indexed document: a verse, a song title or a lyric section, with
// the vector that represents its text and the coordinates that let a hit open
// the stored content it points at.
type Doc struct {
	Key           string
	Domain        string
	Model         string
	Dims          int
	Hash          string
	Vector        []float32
	Label         string
	Text          string
	TranslationID string
	BookID        int
	Chapter       int
	Verse         int
	Testament     string
	SongID        string
	SectionID     string
}

// Store persists embeddings in the app database. Vectors are float32
// little-endian blobs; the model id travels with every row so a model change
// is a rebuild, never a silent mix of vector spaces.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const docColumns = `doc_key, domain, model, dims, content_hash, vector, label, text,
	translation_id, book_id, chapter, verse, testament, song_id, section_id`

func (s *Store) Upsert(ctx context.Context, docs []Doc) error {
	if len(docs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO search_embeddings (`+docColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range docs {
		if _, err := stmt.ExecContext(ctx,
			d.Key, d.Domain, d.Model, d.Dims, d.Hash, encodeVector(d.Vector), d.Label, d.Text,
			d.TranslationID, d.BookID, d.Chapter, d.Verse, d.Testament, d.SongID, d.SectionID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Load reads every indexed document for a model, vectors decoded, in key
// order. Retrieval ranks over this snapshot.
func (s *Store) Load(ctx context.Context, model string) ([]Doc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+docColumns+` FROM search_embeddings WHERE model = ? ORDER BY doc_key`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []Doc
	for rows.Next() {
		var d Doc
		var blob []byte
		if err := rows.Scan(&d.Key, &d.Domain, &d.Model, &d.Dims, &d.Hash, &blob, &d.Label, &d.Text,
			&d.TranslationID, &d.BookID, &d.Chapter, &d.Verse, &d.Testament, &d.SongID, &d.SectionID); err != nil {
			return nil, err
		}
		d.Vector, err = decodeVector(blob, d.Dims)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", d.Key, err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// Count reports how many documents a model has indexed.
func (s *Store) Count(ctx context.Context, model string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM search_embeddings WHERE model = ?`, model).Scan(&n)
	return n, err
}

// CountDomain reports how many documents of one domain a model has indexed,
// so scripture and songs can be built and reported independently.
func (s *Store) CountDomain(ctx context.Context, model, domain string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM search_embeddings WHERE model = ? AND domain = ?`, model, domain).Scan(&n)
	return n, err
}

// Keys returns the stored content hash per document key for a model — the
// diff an incremental rebuild needs.
func (s *Store) Keys(ctx context.Context, model string) (map[string]string, error) {
	return s.keys(ctx, model, "")
}

// KeysDomain returns the stored hashes for one domain only, so building the
// scripture index does not treat song rows as pending or stale.
func (s *Store) KeysDomain(ctx context.Context, model, domain string) (map[string]string, error) {
	return s.keys(ctx, model, domain)
}

func (s *Store) keys(ctx context.Context, model, domain string) (map[string]string, error) {
	query := `SELECT doc_key, content_hash FROM search_embeddings WHERE model = ?`
	args := []any{model}
	if domain != "" {
		query += ` AND domain = ?`
		args = append(args, domain)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make(map[string]string)
	for rows.Next() {
		var key, hash string
		if err := rows.Scan(&key, &hash); err != nil {
			return nil, err
		}
		keys[key] = hash
	}
	return keys, rows.Err()
}

// Prune removes vectors of other models and documents that no longer exist,
// so a deleted song or a replaced model leaves no stale ranking behind. Only
// the named domain is compared against keep, so building scripture cannot
// delete the song index (and the reverse).
func (s *Store) Prune(ctx context.Context, model, domain string, keep map[string]struct{}) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM search_embeddings WHERE model != ?`, model)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()

	rows, err := s.db.QueryContext(ctx,
		`SELECT doc_key FROM search_embeddings WHERE model = ? AND domain = ?`, model, domain)
	if err != nil {
		return int(removed), err
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return int(removed), err
		}
		if _, ok := keep[key]; !ok {
			stale = append(stale, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return int(removed), err
	}

	for start := 0; start < len(stale); start += 500 {
		end := min(start+500, len(stale))
		batch := stale[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, key := range batch {
			args[i] = key
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM search_embeddings WHERE doc_key IN (`+placeholders+`)`, args...)
		if err != nil {
			return int(removed), err
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	return int(removed), nil
}

func encodeVector(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(x))
	}
	return buf
}

func decodeVector(blob []byte, dims int) ([]float32, error) {
	if dims <= 0 || len(blob) != 4*dims {
		return nil, fmt.Errorf("vector blob is %d bytes, want %d", len(blob), 4*dims)
	}
	v := make([]float32, dims)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:]))
	}
	return v, nil
}
