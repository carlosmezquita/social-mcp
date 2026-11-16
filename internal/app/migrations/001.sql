CREATE TABLE campaigns (
 id TEXT PRIMARY KEY, actor TEXT NOT NULL, request_key TEXT NOT NULL,
 fingerprint TEXT NOT NULL, created INTEGER NOT NULL, UNIQUE(actor, request_key)
);
CREATE TABLE deliveries (
 id TEXT PRIMARY KEY, campaign TEXT NOT NULL REFERENCES campaigns(id),
 platform TEXT NOT NULL, account TEXT NOT NULL, binding TEXT NOT NULL, payload BLOB NOT NULL,
 zone TEXT NOT NULL, scheduled INTEGER NOT NULL, due INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('scheduled','processing','retry_wait','published','uncertain','failed','cancelled')),
 lease INTEGER NOT NULL DEFAULT 0, tries INTEGER NOT NULL DEFAULT 0,
 external_id TEXT NOT NULL DEFAULT '', external_url TEXT NOT NULL DEFAULT '',
 failure TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL
);
CREATE INDEX deliveries_due ON deliveries(state,due);
CREATE INDEX deliveries_campaign ON deliveries(campaign,id);
CREATE TABLE attempts (
 delivery TEXT NOT NULL REFERENCES deliveries(id), number INTEGER NOT NULL,
 started INTEGER NOT NULL, finished INTEGER, outcome TEXT, detail TEXT,
 PRIMARY KEY(delivery,number)
);
CREATE TABLE media (
 id TEXT PRIMARY KEY, owner TEXT NOT NULL, mime TEXT NOT NULL, size INTEGER NOT NULL,
 sha256 TEXT NOT NULL, created INTEGER NOT NULL
);
CREATE TABLE credentials (account TEXT PRIMARY KEY, secret BLOB NOT NULL);
CREATE TABLE audit (
 id INTEGER PRIMARY KEY, at INTEGER NOT NULL, actor TEXT NOT NULL,
 operation TEXT NOT NULL, subject TEXT NOT NULL
);
CREATE TABLE oauth_state (
 hash TEXT PRIMARY KEY, account TEXT NOT NULL, verifier TEXT NOT NULL,
 browser TEXT NOT NULL, expires INTEGER NOT NULL
);
CREATE TABLE oauth_links (hash TEXT PRIMARY KEY, account TEXT NOT NULL, expires INTEGER NOT NULL);
CREATE TRIGGER delivery_media_insert BEFORE INSERT ON deliveries
WHEN (json_extract(CAST(NEW.payload AS TEXT),'$.media_id') IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM media WHERE id=json_extract(CAST(NEW.payload AS TEXT),'$.media_id')
)) OR (json_extract(CAST(NEW.payload AS TEXT),'$.custom_preview.image_id') IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM media WHERE id=json_extract(CAST(NEW.payload AS TEXT),'$.custom_preview.image_id')
))
BEGIN SELECT RAISE(ABORT,'missing media'); END;
CREATE TRIGGER delivery_media_update BEFORE UPDATE OF payload ON deliveries
WHEN (json_extract(CAST(NEW.payload AS TEXT),'$.media_id') IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM media WHERE id=json_extract(CAST(NEW.payload AS TEXT),'$.media_id')
)) OR (json_extract(CAST(NEW.payload AS TEXT),'$.custom_preview.image_id') IS NOT NULL AND NOT EXISTS (
 SELECT 1 FROM media WHERE id=json_extract(CAST(NEW.payload AS TEXT),'$.custom_preview.image_id')
))
BEGIN SELECT RAISE(ABORT,'missing media'); END;
