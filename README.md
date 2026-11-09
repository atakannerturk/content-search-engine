# Content Search Engine

[![CI](https://github.com/atakannerturk/content-search-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/atakannerturk/content-search-engine/actions/workflows/ci.yml)

A Go service that aggregates content from several providers (a JSON feed and an XML feed out of the box), normalises it into one model, scores every item with a transparent formula, stores it in PostgreSQL and exposes a search API with keyword search, type filtering, popularity/relevance ordering and pagination. A small embedded dashboard sits on top of the API.

Language: **Go 1.26**. Database: **PostgreSQL 17**. Optional cache backend: **Redis 7**. No web framework, no ORM; the standard library plus `pgx`, `x/time/rate`, `x/sync` and `go-redis`.

## Quick start

```bash
git clone https://github.com/atakannerturk/content-search-engine.git
cd content-search-engine
docker compose up --build
```

Then open:

| URL | What |
| --- | --- |
| <http://localhost:8080/> | Dashboard (search, filter, sort, paginate, trigger a sync) |
| <http://localhost:8080/docs> | Swagger UI for the API |
| <http://localhost:8080/api/v1/contents?q=go&sort=score> | The search endpoint |
| <http://localhost:8080/api/v1/sync/runs> | Audit trail of synchronisation runs |

The API synchronises both providers on start-up (and every 10 minutes afterwards), so the dashboard is populated a second or two after the container reports healthy. Everything has a working default; nothing needs to be edited.

Running without Docker needs a PostgreSQL that matches the default `DATABASE_URL` (`docker compose up -d postgres redis` provides one):

```bash
go run ./cmd/api                 # API + dashboard on :8080, memory cache
go run ./cmd/api -sync-once      # synchronise once, print per-provider stats, exit 0/1
go run ./cmd/api -migrate-only   # apply migrations and exit
go run ./cmd/api -healthcheck    # probe a running instance (used as the container health check)
```

## Requirement coverage

| Requirement | Where |
| --- | --- |
| Keyword search, type filter, popularity and relevance ordering, pagination | `GET /api/v1/contents` — [`internal/search`](internal/search), [`internal/storage/postgres/search.go`](internal/storage/postgres/search.go) |
| Standard score from heterogeneous provider data, type coefficients, freshness and engagement | [`internal/scoring`](internal/scoring) (formula below, golden-tested against the real feeds) |
| Dashboard listing title, type, score, sortable by popularity/relevance | [`web/`](web) — vanilla JS, embedded in the binary |
| Two providers (JSON + XML), standard format conversion, pluggable providers | [`internal/provider`](internal/provider) with the [`jsonfeed`](internal/provider/jsonfeed) and [`xmlfeed`](internal/provider/xmlfeed) adapters |
| Request limit management | Per-provider token bucket + retries with `Retry-After` ([`internal/provider/client.go`](internal/provider/client.go)); per-client limiter on the API ([`internal/httpapi/ratelimit.go`](internal/httpapi/ratelimit.go)) |
| Persistence and data consistency | PostgreSQL schema with constraints, transactional upserts, soft deletion, audited runs ([`internal/storage/postgres`](internal/storage/postgres)) |
| Cache proposal | Generation-invalidated response cache with memory and Redis backends ([`internal/cache`](internal/cache)), proposal in [Caching](#caching) |
| Error handling | RFC 7807 `application/problem+json` everywhere ([`internal/httpapi/problem.go`](internal/httpapi/problem.go)), per-item isolation during ingestion |
| Tests | Unit tests per package, PostgreSQL/Redis integration tests, CI with race detector |
| API documentation | [`api/openapi.yaml`](api/openapi.yaml), served with Swagger UI at `/docs` |
| Setup instructions, architecture decisions, technology rationale | This document |

## Architecture

```mermaid
flowchart LR
  subgraph Providers
    P1["Provider 1<br/>JSON feed"]
    P2["Provider 2<br/>XML feed"]
  end
  subgraph Service["content-search-engine (one binary)"]
    ING["ingest<br/>scheduler, dedupe, scoring,<br/>soft delete, audit"]
    API["httpapi<br/>REST, problem+json,<br/>rate limit, docs"]
    SEARCH["search<br/>validation, cache"]
    DASH["web<br/>embedded dashboard"]
  end
  PG[("PostgreSQL<br/>contents, sync_runs,<br/>full-text index")]
  CACHE[("Redis or<br/>in-memory cache")]
  P1 -- "rate limited, retried" --> ING
  P2 -- "rate limited, retried" --> ING
  ING -- "upsert, deactivate, rescore" --> PG
  ING -- "bump generation" --> CACHE
  Client(["Browser / API client"]) --> API
  Client --> DASH --> API
  API --> SEARCH --> PG
  SEARCH <--> CACHE
```

One binary serves the API and the dashboard and runs the synchronisation scheduler. The packages form a straightforward layering:

| Package | Responsibility |
| --- | --- |
| `cmd/api` | Wiring, flags, graceful shutdown. The only place that knows concrete providers and backends. |
| `internal/config` | Environment configuration with validated defaults. |
| `internal/domain` | Provider-agnostic model: `Content`, `ScoreBreakdown`, `SearchQuery`, `SyncRun`, error types. No JSON or SQL tags. |
| `internal/scoring` | The scoring formula as a pure function with an injected clock. |
| `internal/provider` | `Provider` contract, cursor helpers, tolerant parsers and the shared rate-limited HTTP client. `jsonfeed` and `xmlfeed` implement the contract; `providertest` checks any implementation against it. |
| `internal/ingest` | Synchronisation: concurrent, isolated provider runs, de-duplication, scoring at write time, batch upsert, soft deletion, audit rows, incremental rescoring, scheduler, in-process and cross-instance locking. |
| `internal/search` | Search use case: parameter normalisation, canonical cache keys, request coalescing. |
| `internal/cache` | `Cache` contract with generation counter; memory (TTL + LRU) and Redis backends. |
| `internal/storage/postgres` | Migrations, repository, full-text query builder, advisory locks. |
| `internal/httpapi` | Routing, middleware, handlers, response DTOs, RFC 7807 mapping, Swagger UI. |
| `web` | Dashboard assets, embedded with `embed.FS`. |
| `api` | `openapi.yaml`, embedded and served at `/openapi.yaml`. |

Dependencies point inwards: `httpapi -> search/ingest -> domain/scoring`. Interfaces are declared where they are consumed (`search.Repository`, `ingest.ContentWriter`, `ingest.RunRecorder`, `ingest.Locker`, `httpapi.Searcher`, ...) and satisfied by the concrete packages, which keeps every layer testable with small fakes.

### A synchronisation run

1. The scheduler (or `POST /api/v1/sync`, or start-up) asks `ingest.Service` for a run. A `sync.Mutex.TryLock` rejects overlapping runs in the process; a PostgreSQL advisory lock held on a dedicated connection rejects overlapping runs across instances.
2. Every provider is fetched concurrently and in isolation: a failing provider yields a `failed` audit row while the others continue. Pages are followed through opaque cursors until the adapter reports the end; a page cap and a "no new ids" guard protect against providers that ignore pagination.
3. Items are validated (unknown type, empty title, unparseable date -> skipped and counted, never fatal), de-duplicated (later pages win) and scored as of the run start.
4. One transaction per provider upserts the batch (`ON CONFLICT (id) DO UPDATE`), reviving soft-deleted items that reappeared and stamping `last_seen_at`.
5. If the provider walk was complete, items it no longer returns are soft-deleted, unless that would remove more than `DEACTIVATE_MAX_RATIO` of its catalogue (a partial feed must not wipe the index).
6. Expired freshness bonuses are recomputed, the cache generation is bumped, and the per-provider `sync_runs` rows are finalised with counters, flags and skipped-item samples.

## Scoring

The formula from the assignment is applied verbatim in [`internal/scoring`](internal/scoring/scoring.go):

```
Final = Base * TypeCoefficient + Freshness + Engagement

Base            video: views / 1000 + likes / 100
                text:  reading_time + reactions / 50
TypeCoefficient video: 1.5      text: 1.0
Freshness       published <= 7 days ago: +5, <= 30 days: +3, <= 90 days: +1, else 0
Engagement      video: (likes / views) * 10
                text:  (reactions / reading_time) * 5
```

Decisions that the formula text leaves open, and how they are resolved:

- **Arithmetic is float64 end to end.** With integer division `likes / views` is 0 for every realistic video and `18500 / 1000` is 18; both would silently distort the ranking. Nothing is rounded before storage or ordering; the API presents six decimals (hiding binary artefacts such as `0.8400000000000001`) and the dashboard shows two.
- **Freshness windows are 7, 30 and 90 days, inclusive.** "One month" and "three months" are fixed-length windows so that a score does not depend on the calendar month of publication. Items dated in the future count as published now.
- **A zero denominator contributes 0** (no views, no reading time), negative metrics are clamped to 0 and a non-finite result is stored as 0, so a score can always be sorted and serialised.
- **The score is computed when an item is written**, in the same statement as the item, so there is never an unscored row. Freshness is the only time-dependent term and it changes at known instants, so every item carries `freshness_expires_at` and a small job (`RESCORE_INTERVAL`, default one minute) recomputes exactly the rows whose bonus has expired. Scores are therefore at most one interval stale around a boundary and the hot path sorts on an indexed column. Computing freshness in SQL at read time was rejected because it duplicates the tiers in a second language and defeats both the `score` index and the response cache.

Scores of the items the two mock providers return (as of November 2026, so no freshness bonus applies); the same numbers are pinned by `TestScoreGoldenMockData`:

| Item | Type | Inputs | Base | Coef. | Fresh. | Engagement | Final |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |
| provider2:a1 Clean Architecture in Go | text | 8 min, 450 reactions | 17.0 | 1.0 | 0 | 281.25 | **298.25** |
| provider1:v2 Advanced Go Concurrency Patterns | video | 25000 views, 2100 likes | 46.0 | 1.5 | 0 | 0.84 | **69.84** |
| provider2:v1 Introduction to Docker | video | 22000 views, 1800 likes | 40.0 | 1.5 | 0 | 0.8182 | **60.8182** |
| provider2:v2 Kubernetes for Beginners | video | 19500 views, 1600 likes | 35.5 | 1.5 | 0 | 0.8205 | **54.0705** |
| provider1:v3 Building RESTful APIs with Go | video | 18500 views, 1500 likes | 33.5 | 1.5 | 0 | 0.8108 | **51.0608** |
| provider2:v3 CI/CD Pipeline Implementation | video | 15800 views, 1250 likes | 28.3 | 1.5 | 0 | 0.7911 | **43.2411** |
| provider1:v1 Go Programming Tutorial | video | 15000 views, 1200 likes | 27.0 | 1.5 | 0 | 0.8 | **41.3** |
| provider1:v4 Go Testing Best Practices | video | 12000 views, 950 likes | 21.5 | 1.5 | 0 | 0.7917 | **33.0417** |

**An observation, not a bug:** the single article outscores every video by roughly four times. The engagement term mixes units (reactions per minute of reading, weighted 5, versus likes per view, weighted 10), so text content with a short reading time is favoured heavily. The implementation follows the specification exactly and exposes the breakdown (`score_breakdown` in the API, a tooltip in the dashboard) so the ranking is explainable; a normalised variant is listed under [future work](#trade-offs-limitations-and-future-work).

## Search

- **Keyword search** uses PostgreSQL full-text search over a stored, weighted `tsvector` (title weight A, tags weight B) with a GIN index. The user query is turned into a prefix query in Go (`go conc` -> `'go':* & 'conc':*`), so partial words match and the input can never produce a `tsquery` syntax error or injection. Titles have `/`, `_` and `.` turned into spaces before indexing so `CI/CD` is found by `ci`, `cd` or `ci/cd`.
- **Ordering:** `sort=score` (default, popularity: the persisted final score), `sort=relevance` (full-text rank, title matches above tag-only matches; requires `q`) and `sort=published_at`. Every ordering has a deterministic tie-break (`score`, `published_at`, `id`) so pages never overlap or skip.
- **Filters:** `type=video|text` (`article` is accepted as an alias of `text`), `provider=<name>`.
- **Pagination:** `page` (1..10000) and `per_page` (1..100, default 20). The total is counted with a separate query over the same predicate, so a page past the end still reports the real `total` and `total_pages`. Offset pagination can drift while a synchronisation lands between two page requests; keyset pagination over `(score, id)` is the upgrade path if that matters.
- Soft-deleted items never appear in search results unless `include_deleted=true` is passed; `GET /api/v1/contents/{id}` answers `410 Gone` for them.

## Provider integration

A provider is anything that implements

```go
type Provider interface {
    Name() string
    Fetch(ctx context.Context, cursor string) (Page, error)
}
```

`Fetch("")` returns the first page; the returned `Page.NextCursor` requests the next one and is empty at the end. Page-numbered feeds encode the number in the cursor (`provider.PageCursor`); a token- or link-based feed can encode that instead. Each adapter maps its own wire format to `domain.Content` and decides when the feed is exhausted, treating advertised totals as advisory. The two mock endpoints are static files that ignore `?page=` and advertise totals they do not deliver; the adapters stop after the first page because it holds fewer items than `per_page`, and the ingest loop additionally stops on a repeated page or at `PROVIDER_MAX_PAGES`.

The shared HTTP client ([`provider.Client`](internal/provider/client.go)) implements request limit management on the outbound side:

- a token bucket per provider (`PROVIDER_RATE_LIMIT_RPS`, `PROVIDER_BURST`) consulted before every attempt, retries included;
- bounded retries (`PROVIDER_MAX_RETRIES`) with exponential backoff and jitter for network errors, HTTP 429 and 5xx, honouring `Retry-After` up to a cap; 4xx responses are not retried;
- a per-request timeout and a response body limit (`PROVIDER_MAX_BODY_BYTES`);
- counters for retries and limiter waits, attributed to each synchronisation run and visible in `GET /api/v1/sync/runs`.

Adapters decode by their configured format, never by `Content-Type` (the mocks serve `text/plain`). Dates are parsed from several layouts and normalised to UTC (a bare date becomes midnight UTC), durations accept `mm:ss`, `h:mm:ss` or seconds, numeric fields tolerate strings, the content type is normalised through an alias map (`article` -> `text`) and every item keeps its original payload in `raw` for debugging.

### Adding a provider

1. Create `internal/provider/<name>/` with a type implementing `provider.Provider` (copy `jsonfeed` or `xmlfeed`; use `provider.ParseTime`, `provider.ParseDuration`, `provider.NormalizeTags`, `provider.HasMorePages`). Return invalid items in `Page.Skipped` with a reason instead of failing the page.
2. Add a test that runs `providertest.RunContract(t, yourProvider, minItems)` against a fixture server; it checks identifiers, UTC timestamps, canonical types and cursor termination.
3. Register an instance in `cmd/api/main.go` (`buildProviders`) with a fixed name and a configured URL.

## Data model and consistency

Two tables, created by the embedded migration ([`0001_init.sql`](internal/storage/postgres/migrations)):

- `contents` — one row per provider item. The primary key is the public identifier `<provider>:<external_id>`, which keeps colliding external ids apart (both mocks have a `v1`) and stays stable across database rebuilds. Constraints: `NOT NULL` on everything the formula needs, a `CHECK` on the type, `UNIQUE (provider, external_id)`. Score components, `freshness_expires_at`, `first_seen_at`/`last_seen_at`, `deleted_at`, the raw payload and a generated `search_vector` live alongside. Indexes are partial on `deleted_at IS NULL` for the sort columns, plus the GIN index and an index on `freshness_expires_at` for the rescoring job.
- `sync_runs` — one row per provider per run: trigger, status (`running`, `succeeded`, `failed`, `interrupted`, `skipped_locked`), counters (pages, fetched, inserted, updated, skipped, deactivated, retries, rate-limit waits), flags (`max_pages_reached`, `no_new_ids_stop`, `deactivation_skipped_ratio`, `provider_ignores_pagination`), the error and a sample of skipped items with reasons.

Consistency rules:

- Validation happens before the database; constraints are a backstop, not the filter. One bad item is skipped and counted, never the batch.
- Each provider batch is one transaction; ids are de-duplicated in memory first (PostgreSQL rejects a multi-row upsert that touches the same row twice).
- Items a provider stops returning are soft-deleted only after a complete, successful walk and only below the ratio guard; a reappearing item is revived.
- Migrations run inside a transaction under an advisory lock, so several instances can start at once. Runs left `running` by a crash are marked `interrupted` at the next start.

## Caching

The search endpoint caches serialised responses under a key derived from the canonical query (`q` trimmed and lower-cased, defaults filled in, fixed parameter order) and prefixed with a **generation** number. A synchronisation or rescoring that changed data bumps the generation once, which makes every cached page unreachable in O(1); the old entries expire through their TTL. Concurrent identical misses are coalesced with `singleflight`, so a popular query reaches PostgreSQL once per TTL. `X-Cache: HIT|MISS` on the response shows the cache working.

Two backends implement the same interface:

- **memory** (default locally): TTL with lazy expiry, LRU bound (`CACHE_MAX_ENTRIES`), no background goroutine. Single-instance by nature.
- **redis** (default in `docker-compose.yml`): the generation counter lives in Redis too, so every instance invalidates every other one.

Proposal for a wider deployment, in order of value: (1) this response cache on Redis, shared by all API replicas; (2) `Cache-Control`/`ETag` on the search responses once the dashboard is served from a CDN, so browsers and the edge revalidate instead of refetching; (3) conditional requests (`If-None-Match`) towards providers that support them, so unchanged feeds cost no bandwidth; (4) keep the database itself fast with the indexes already in place rather than caching individual rows. Deep pages (`page > 10`) and non-200 responses are deliberately not cached to keep the key space bounded.

## API

| Method and path | Purpose |
| --- | --- |
| `GET /api/v1/contents` | Search. Parameters: `q`, `type`, `provider`, `sort` (`score`, `relevance`, `published_at`), `order` (`asc`, `desc`), `page`, `per_page`, `include_deleted`. |
| `GET /api/v1/contents/{id}` | One item by `<provider>:<external_id>`. `404` unknown, `410` soft-deleted. |
| `POST /api/v1/sync` | Start a synchronisation in the background: `202` with `batch_id`, `409` when one is running, `401` when `SYNC_TOKEN` is set and missing. |
| `GET /api/v1/sync/runs?limit=` | Most recent per-provider runs with counters and flags. |
| `GET /healthz`, `GET /readyz` | Liveness; readiness of PostgreSQL and the cache (`503` with per-check detail when degraded). |
| `GET /openapi.yaml`, `GET /docs` | OpenAPI 3 document and Swagger UI. |
| `GET /` | Dashboard. |

Every error is an RFC 7807 problem:

```json
{
  "type": "about:blank",
  "title": "Bad Request",
  "status": 400,
  "detail": "invalid request parameters",
  "instance": "/api/v1/contents?per_page=1000",
  "errors": { "per_page": "must be an integer between 1 and 100" },
  "request_id": "4f1c2b0e9a7d3c55"
}
```

Examples:

```bash
curl 'localhost:8080/api/v1/contents?q=go&type=video&sort=score'
curl 'localhost:8080/api/v1/contents?q=kube&sort=relevance'
curl 'localhost:8080/api/v1/contents?type=text'            # provider2:a1, score 298.25
curl -i 'localhost:8080/api/v1/contents?q=docker'          # X-Cache: MISS, then HIT
curl 'localhost:8080/api/v1/contents/provider1:v1'
curl -X POST 'localhost:8080/api/v1/sync'                  # 202, or 409 while running
curl 'localhost:8080/api/v1/sync/runs?limit=5'
```

Responses carry a `Content-Security-Policy`, `X-Content-Type-Options: nosniff` and an `X-Request-ID` (echoed when the client sends one, otherwise generated) that also appears in problem bodies and server logs. The API applies a per-client token bucket (`API_RATE_LIMIT_RPS`, `API_RATE_LIMIT_BURST`) answering `429` with `Retry-After`; `X-Forwarded-For` is only trusted with `TRUST_PROXY=true`.

## Configuration

All settings are environment variables with working defaults (see [`.env.example`](.env.example)).

| Variable | Default | Meaning |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | `postgres://app:app@localhost:5432/content?sslmode=disable` | PostgreSQL DSN |
| `PROVIDER1_URL`, `PROVIDER2_URL` | the mock endpoints | Feed URLs of the two providers |
| `PROVIDER_RATE_LIMIT_RPS`, `PROVIDER_BURST` | `2`, `2` | Outbound token bucket per provider |
| `PROVIDER_TIMEOUT` | `10s` | Timeout of one provider request |
| `PROVIDER_MAX_PAGES` | `10` | Page cap per provider per run |
| `PROVIDER_MAX_RETRIES` | `3` | Retries after the first attempt |
| `PROVIDER_MAX_BODY_BYTES` | `10485760` | Response body limit |
| `SYNC_ON_START` | `true` | Synchronise when the process starts |
| `SYNC_INTERVAL` | `10m` | Scheduled synchronisation; `0` disables |
| `SYNC_TIMEOUT` | `2m` | Bound of one full run |
| `SYNC_TOKEN` | empty | Bearer token required on `POST /api/v1/sync` when set |
| `DEACTIVATE_MAX_RATIO` | `0.5` | Soft deletion is skipped above this share of vanished items |
| `RESCORE_INTERVAL` | `1m` | Freshness rescoring job; `0` disables |
| `CACHE_BACKEND` | `memory` | `memory` or `redis` |
| `CACHE_TTL` | `60s` | Lifetime of a cached search response; `0` disables caching |
| `CACHE_MAX_ENTRIES` | `10000` | Bound of the memory cache |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `API_RATE_LIMIT_RPS`, `API_RATE_LIMIT_BURST` | `20`, `40` | Inbound limiter per client |
| `TRUST_PROXY` | `false` | Use `X-Forwarded-For` as the client address |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `text` | Logging (`json` in the container) |

## Development

```bash
make up                 # postgres + redis in the background
make test               # unit tests
make test-integration   # repository tests against the local PostgreSQL
make test-race          # with the race detector (needs cgo)
make vet lint           # go vet, golangci-lint
make run                # go run ./cmd/api
make check-name         # the delivery rule about the issuing company's name
```

Plain commands work everywhere (the Makefile is a convenience): `go test ./...`, `TEST_DATABASE_URL=postgres://app:app@localhost:5432/content_test?sslmode=disable go test ./internal/storage/...`, `TEST_REDIS_ADDR=localhost:6379 go test ./internal/cache/...`. Integration tests skip themselves when those variables are unset.

### Testing strategy

- **Unit tests** cover the pure logic with table-driven cases: the scoring formula (golden values for every mock item plus boundary and edge cases), request normalisation, the `tsquery` builder, provider parsing with the real responses saved as fixtures and mutated copies (unknown types, missing metrics, bad dates, hostile titles), the HTTP client (retries, `Retry-After`, body limits, limiter waits, with an injected sleep so nothing actually waits), the ingest service (isolation, de-duplication, stop guards, soft-deletion rules, locking, cancellation) and every HTTP handler through fakes (envelopes, validation, problem responses, rate limiting, token checks, timeouts, panics).
- **Integration tests** run the migrations and the repository against a real PostgreSQL (upserts, revival, soft deletion, search semantics on the real titles, pagination totals, rescoring, audit rows, advisory lock) and the cache contract against Redis.
- **CI** runs gofmt, `go vet`, golangci-lint, the whole suite with `-race` against service containers, `govulncheck` and the Docker build.

## Technology choices

- **Go** for a small, statically linked binary with first-class concurrency: provider fetches run in parallel with per-provider isolation, and the scheduler, server and background jobs share one process with clean shutdown semantics.
- **Standard library HTTP** (Go 1.22 method-and-pattern routing) instead of a framework: the routing needs are modest and the middleware stays explicit and testable.
- **PostgreSQL** for ACID upserts, constraints and built-in full-text search with ranking. At this scale (hundreds to low millions of items) it removes the need for a separate search engine; the repository interface is the seam if OpenSearch/Elasticsearch ever becomes necessary.
- **pgx** as the native PostgreSQL driver (batching, arrays, `tsvector` friendly) rather than an ORM, keeping the SQL visible and reviewable.
- **Redis** as the shared cache backend because invalidation is a single `INCR` and every API replica sees it; the memory backend keeps local development dependency-free.
- **`x/time/rate`** for token buckets on both sides, **`x/sync/singleflight`** for request coalescing, **`log/slog`** for structured logs with request ids.
- **Docker Compose** so that a reviewer gets a populated dashboard with one command; **distroless** image running as non-root.

## Trade-offs, limitations and future work

- Stored scores are at most `RESCORE_INTERVAL` stale at a freshness boundary (versus always-fresh but unindexable read-time computation).
- Full-text search is prefix-based on word boundaries; it does not match the middle of words (`ubernetes`). `pg_trgm` would add that at the cost of a larger index.
- Offset pagination can drift under concurrent writes; keyset pagination is the upgrade path.
- The in-process mutex plus the PostgreSQL advisory lock serialise runs, but each replica still runs its own scheduler and would compete for the lock every interval; a leader election or a single worker replica would be cleaner at scale.
- Scoring follows the specification; a normalised engagement term (e.g. per-type z-scores) would make videos and texts comparable.
- Observability stops at structured logs and the `sync_runs` table; Prometheus metrics and tracing are natural next steps.
- Possible additions: per-provider schedules, cursor-based providers, provider-side conditional requests, language-aware stemming for non-English content.

## Project layout

```
cmd/api/                  entrypoint, wiring, flags
internal/config/          environment configuration
internal/domain/          shared model and errors
internal/scoring/         scoring formula
internal/provider/        provider contract, parsers, HTTP client
  jsonfeed/ xmlfeed/      the two adapters (+ testdata fixtures)
  providertest/           contract test kit for adapters
internal/ingest/          synchronisation service and scheduler
internal/search/          search use case and cache keys
internal/cache/           cache contract, memory and Redis backends
internal/storage/postgres/ migrations, repository, tsquery builder
internal/httpapi/         HTTP layer
web/                      dashboard (embedded)
api/                      OpenAPI document (embedded)
docker/                   PostgreSQL init script for the test database
```
