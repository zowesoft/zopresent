-- Semantic search keeps one embedding per indexed document: every verse of
-- every installed translation, plus song titles and lyric sections. The model
-- that produced a vector travels with it, so a model change rebuilds the index
-- instead of silently ranking new queries against old vectors. content_hash
-- lets a rebuild re-embed only what actually changed — a song edit costs a few
-- documents, not the whole library again.
CREATE TABLE search_embeddings (
    doc_key        TEXT PRIMARY KEY,
    domain         TEXT NOT NULL,
    model          TEXT NOT NULL,
    dims           INTEGER NOT NULL,
    content_hash   TEXT NOT NULL,
    vector         BLOB NOT NULL,
    label          TEXT NOT NULL,
    text           TEXT NOT NULL,
    translation_id TEXT NOT NULL DEFAULT '',
    book_id        INTEGER NOT NULL DEFAULT 0,
    chapter        INTEGER NOT NULL DEFAULT 0,
    verse          INTEGER NOT NULL DEFAULT 0,
    testament      TEXT NOT NULL DEFAULT '',
    song_id        TEXT NOT NULL DEFAULT '',
    section_id     TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX search_embeddings_model_domain ON search_embeddings (model, domain);