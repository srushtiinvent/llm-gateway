package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/lib/pq" // Postgres driver
)

// Postgres stores keys and logs in a PostgreSQL database (for example Neon).
type Postgres struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
	id          BIGSERIAL PRIMARY KEY,
	name        TEXT        NOT NULL,
	prefix      TEXT        NOT NULL,
	key_hash    TEXT        NOT NULL UNIQUE,
	rpm_limit   INTEGER     NOT NULL DEFAULT 0,
	daily_limit INTEGER     NOT NULL DEFAULT 0,
	revoked     BOOLEAN     NOT NULL DEFAULT FALSE,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS request_logs (
	id                BIGSERIAL PRIMARY KEY,
	ts                TIMESTAMPTZ NOT NULL DEFAULT now(),
	key_id            BIGINT      NOT NULL DEFAULT 0,
	provider          TEXT        NOT NULL DEFAULT '',
	model             TEXT        NOT NULL DEFAULT '',
	requested_model   TEXT        NOT NULL DEFAULT '',
	status            TEXT        NOT NULL,
	cache_hit         BOOLEAN     NOT NULL DEFAULT FALSE,
	stream            BOOLEAN     NOT NULL DEFAULT FALSE,
	latency_ms        INTEGER     NOT NULL DEFAULT 0,
	prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
	completion_tokens INTEGER     NOT NULL DEFAULT 0,
	attempts          INTEGER     NOT NULL DEFAULT 0,
	tried             TEXT        NOT NULL DEFAULT '',
	error             TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS request_logs_ts_idx ON request_logs (ts);
CREATE INDEX IF NOT EXISTS request_logs_key_ts_idx ON request_logs (key_id, ts);
`

// NewPostgres connects and creates the tables if they do not exist yet.
func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(pctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Postgres{db: db}, nil
}

func (p *Postgres) CreateKey(ctx context.Context, name, prefix, hash string, rpm, daily int) (Key, error) {
	k := Key{Name: name, Prefix: prefix, Hash: hash, RPMLimit: rpm, DailyLimit: daily}
	err := p.db.QueryRowContext(ctx,
		`INSERT INTO api_keys (name, prefix, key_hash, rpm_limit, daily_limit) VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		name, prefix, hash, rpm, daily).Scan(&k.ID, &k.CreatedAt)
	return k, err
}

func (p *Postgres) FindKeyByHash(ctx context.Context, hash string) (*Key, error) {
	var k Key
	err := p.db.QueryRowContext(ctx,
		`SELECT id, name, prefix, rpm_limit, daily_limit, revoked, created_at FROM api_keys WHERE key_hash=$1`, hash).
		Scan(&k.ID, &k.Name, &k.Prefix, &k.RPMLimit, &k.DailyLimit, &k.Revoked, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.Hash = hash
	return &k, nil
}

func (p *Postgres) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, name, prefix, rpm_limit, daily_limit, revoked, created_at FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.RPMLimit, &k.DailyLimit, &k.Revoked, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *Postgres) RevokeKey(ctx context.Context, id int64) error {
	res, err := p.db.ExecContext(ctx, `UPDATE api_keys SET revoked=TRUE WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) InsertLog(ctx context.Context, l RequestLog) error {
	if l.TS.IsZero() {
		l.TS = time.Now()
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO request_logs (ts, key_id, provider, model, requested_model, status, cache_hit, stream,
			latency_ms, prompt_tokens, completion_tokens, attempts, tried, error)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		l.TS, l.KeyID, l.Provider, l.Model, l.RequestedModel, l.Status, l.CacheHit, l.Stream,
		l.LatencyMs, l.PromptTokens, l.CompletionTokens, l.Attempts, l.Tried, l.Error)
	return err
}

func (p *Postgres) RecentLogs(ctx context.Context, limit int) ([]RequestLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, ts, key_id, provider, model, requested_model, status, cache_hit, stream,
			latency_ms, prompt_tokens, completion_tokens, attempts, tried, error
		 FROM request_logs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestLog
	for rows.Next() {
		var l RequestLog
		if err := rows.Scan(&l.ID, &l.TS, &l.KeyID, &l.Provider, &l.Model, &l.RequestedModel, &l.Status, &l.CacheHit,
			&l.Stream, &l.LatencyMs, &l.PromptTokens, &l.CompletionTokens, &l.Attempts, &l.Tried, &l.Error); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (p *Postgres) CountProviderCalls(ctx context.Context, keyID int64, since time.Time) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM request_logs WHERE key_id=$1 AND NOT cache_hit AND ts >= $2`, keyID, since).Scan(&n)
	return n, err
}

func (p *Postgres) Stats(ctx context.Context, since time.Time, bucket time.Duration) (Stats, error) {
	var st Stats
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE cache_hit),
		       count(*) FILTER (WHERE NOT cache_hit AND status='error'),
		       coalesce(sum(prompt_tokens)     FILTER (WHERE NOT cache_hit AND status='ok'), 0),
		       coalesce(sum(completion_tokens) FILTER (WHERE NOT cache_hit AND status='ok'), 0),
		       coalesce(sum(prompt_tokens)     FILTER (WHERE cache_hit), 0),
		       coalesce(sum(completion_tokens) FILTER (WHERE cache_hit), 0),
		       coalesce(avg(latency_ms) FILTER (WHERE NOT cache_hit AND status='ok'), 0),
		       coalesce(percentile_cont(0.5)  WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE NOT cache_hit AND status='ok'), 0),
		       coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE NOT cache_hit AND status='ok'), 0),
		       coalesce(avg(latency_ms) FILTER (WHERE cache_hit), 0)
		FROM request_logs WHERE ts >= $1`, since).Scan(
		&st.Requests, &st.CacheHits, &st.Errors, &st.PromptTokens, &st.CompletionTokens,
		&st.SavedPromptTokens, &st.SavedCompletionTokens, &st.AvgLatencyMs, &st.P50LatencyMs, &st.P95LatencyMs,
		&st.AvgCacheLatencyMs)
	if err != nil {
		return st, err
	}

	rows, err := p.db.QueryContext(ctx, `
		SELECT provider, count(*), count(*) FILTER (WHERE status='error'),
		       coalesce(avg(latency_ms) FILTER (WHERE status='ok'), 0),
		       coalesce(sum(prompt_tokens + completion_tokens), 0)
		FROM request_logs WHERE ts >= $1 AND NOT cache_hit
		GROUP BY provider ORDER BY 2 DESC`, since)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var ps ProviderStat
		if err := rows.Scan(&ps.Provider, &ps.Requests, &ps.Errors, &ps.AvgLatencyMs, &ps.Tokens); err != nil {
			rows.Close()
			return st, err
		}
		st.ByProvider = append(st.ByProvider, ps)
	}
	rows.Close()

	secs := bucket.Seconds()
	if secs < 1 {
		secs = 60
	}
	rows, err = p.db.QueryContext(ctx, `
		SELECT to_timestamp(floor(extract(epoch FROM ts) / $2::float8) * $2::float8) AS b,
		       count(*),
		       count(*) FILTER (WHERE cache_hit),
		       count(*) FILTER (WHERE NOT cache_hit AND status='error'),
		       coalesce(avg(latency_ms) FILTER (WHERE NOT cache_hit AND status='ok'), 0)
		FROM request_logs WHERE ts >= $1
		GROUP BY b ORDER BY b`, since, secs)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var pt Point
		if err := rows.Scan(&pt.T, &pt.Requests, &pt.CacheHits, &pt.Errors, &pt.AvgLatencyMs); err != nil {
			return st, err
		}
		st.Series = append(st.Series, pt)
	}
	return st, rows.Err()
}

func (p *Postgres) PurgeLogs(ctx context.Context, before time.Time) (int64, error) {
	res, err := p.db.ExecContext(ctx, `DELETE FROM request_logs WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (p *Postgres) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *Postgres) Kind() string                   { return "postgres" }
func (p *Postgres) Close() error                   { return p.db.Close() }

var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)
