-- 0001_init: content store and synchronisation audit trail.

CREATE TABLE contents (
  id                   TEXT PRIMARY KEY,
  provider             TEXT NOT NULL,
  external_id          TEXT NOT NULL,
  type                 TEXT NOT NULL CHECK (type IN ('video', 'text')),
  title                TEXT NOT NULL,
  tags                 TEXT[] NOT NULL DEFAULT '{}',
  -- Space-joined copy of tags maintained by the application: array_to_string
  -- is only STABLE, so it cannot feed the generated search_vector below.
  tags_text            TEXT NOT NULL DEFAULT '',
  published_at         TIMESTAMPTZ NOT NULL,
  views                BIGINT,
  likes                BIGINT,
  duration_seconds     BIGINT,
  reading_time_minutes BIGINT,
  reactions            BIGINT,
  comments             BIGINT,
  base_score           DOUBLE PRECISION NOT NULL,
  type_coefficient     DOUBLE PRECISION NOT NULL,
  freshness_score      DOUBLE PRECISION NOT NULL,
  engagement_score     DOUBLE PRECISION NOT NULL,
  score                DOUBLE PRECISION NOT NULL,
  freshness_expires_at TIMESTAMPTZ,
  scored_at            TIMESTAMPTZ NOT NULL,
  raw                  TEXT,
  raw_format           TEXT,
  first_seen_at        TIMESTAMPTZ NOT NULL,
  last_seen_at         TIMESTAMPTZ NOT NULL,
  deleted_at           TIMESTAMPTZ,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Titles weigh more than tags. translate() turns separators such as the
  -- slash in "CI/CD" into spaces so the parser yields "ci" and "cd" instead
  -- of one file-path token.
  search_vector        TSVECTOR GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', coalesce(translate(title, '/_.', '   '), '')), 'A') ||
    setweight(to_tsvector('simple', coalesce(tags_text, '')), 'B')
  ) STORED,
  UNIQUE (provider, external_id)
);

-- Every query of the API excludes soft-deleted rows, so the indexes do too.
CREATE INDEX contents_search_vector_idx ON contents USING GIN (search_vector) WHERE deleted_at IS NULL;
CREATE INDEX contents_score_idx ON contents (score DESC, published_at DESC, id) WHERE deleted_at IS NULL;
CREATE INDEX contents_type_score_idx ON contents (type, score DESC) WHERE deleted_at IS NULL;
CREATE INDEX contents_published_at_idx ON contents (published_at DESC, id) WHERE deleted_at IS NULL;
CREATE INDEX contents_type_published_at_idx ON contents (type, published_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX contents_provider_idx ON contents (provider) WHERE deleted_at IS NULL;
CREATE INDEX contents_freshness_expires_at_idx ON contents (freshness_expires_at)
  WHERE deleted_at IS NULL AND freshness_expires_at IS NOT NULL;

CREATE TABLE sync_runs (
  id               BIGSERIAL PRIMARY KEY,
  batch_id         TEXT NOT NULL,
  trigger          TEXT NOT NULL,
  provider         TEXT NOT NULL,
  status           TEXT NOT NULL,
  started_at       TIMESTAMPTZ NOT NULL,
  finished_at      TIMESTAMPTZ,
  pages            INT NOT NULL DEFAULT 0,
  fetched          INT NOT NULL DEFAULT 0,
  inserted         INT NOT NULL DEFAULT 0,
  updated          INT NOT NULL DEFAULT 0,
  skipped          INT NOT NULL DEFAULT 0,
  deactivated      INT NOT NULL DEFAULT 0,
  retries          INT NOT NULL DEFAULT 0,
  rate_limit_waits INT NOT NULL DEFAULT 0,
  error            TEXT NOT NULL DEFAULT '',
  skipped_reasons  JSONB NOT NULL DEFAULT '[]',
  flags            TEXT[] NOT NULL DEFAULT '{}'
);

CREATE INDEX sync_runs_started_at_idx ON sync_runs (started_at DESC);
