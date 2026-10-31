CREATE TABLE
  IF NOT EXISTS requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    endpoint TEXT NOT NULL,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    query TEXT NOT NULL DEFAULT '',
    headers TEXT NOT NULL, -- JSON object; see headers_b64, added in store.go
    body BLOB NOT NULL,
    remote_ip TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    forward_status INTEGER NOT NULL DEFAULT 0,
    forward_error TEXT NOT NULL DEFAULT '',
    forward_ms INTEGER NOT NULL DEFAULT 0
  );

CREATE INDEX IF NOT EXISTS idx_requests_created ON requests (created_at);

CREATE INDEX IF NOT EXISTS idx_requests_endpoint ON requests (endpoint, id);