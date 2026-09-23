// Package store persists request records to SQLite with an async batch writer.
package store

import (
	"database/sql"
	"log"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Record is one request observation.
type Record struct {
	Ts        int64 // unix millis
	Key       string
	Model     string
	Provider  string
	Status    int
	Stream    bool
	TTFTms    float64
	DurMs     float64
	TokIn     int
	TokOut    int
	CacheRead int
	CacheWrt  int
	CostUSD   float64
	Err       string
	Attempts  int
}

type Store struct {
	db   *sql.DB
	ch   chan *Record
	wg   sync.WaitGroup
	done chan struct{}
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // ponytail: single writer conn; WAL + batching is plenty at this scale
	for _, ddl := range schema {
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db, ch: make(chan *Record, 4096), done: make(chan struct{})}
	s.wg.Add(1)
	go s.run()
	// the DB holds oauth tokens — owner-only perms (best effort for pre-existing files)
	_ = os.Chmod(path, 0o600)
	return s, nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		key_name TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 0,
		stream INTEGER NOT NULL DEFAULT 0,
		ttft_ms REAL NOT NULL DEFAULT 0,
		dur_ms REAL NOT NULL DEFAULT 0,
		tok_in INTEGER NOT NULL DEFAULT 0,
		tok_out INTEGER NOT NULL DEFAULT 0,
		cache_read INTEGER NOT NULL DEFAULT 0,
		cache_write INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		err TEXT NOT NULL DEFAULT '',
		attempts INTEGER NOT NULL DEFAULT 1
	)`,
	`CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts)`,
	`CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model)`,
	`CREATE INDEX IF NOT EXISTS idx_requests_provider ON requests(provider)`,
	`CREATE TABLE IF NOT EXISTS oauth_tokens (
		provider TEXT NOT NULL,
		acct TEXT NOT NULL,
		access_token TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '',
		expires_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, acct)
	)`,
}

func (s *Store) Submit(r *Record) {
	if s == nil {
		return
	}
	select {
	case s.ch <- r:
	default: // ponytail: drop under extreme backlog rather than block the hot path
	}
}

func (s *Store) run() {
	defer s.wg.Done()
	const batch = 200
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	buf := make([]*Record, 0, batch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		tx, err := s.db.Begin()
		if err != nil {
			log.Printf("store: tx begin: %v", err)
			buf = buf[:0]
			return
		}
		for _, r := range buf {
			_, err := tx.Exec(`INSERT INTO requests
				(ts,key_name,model,provider,status,stream,ttft_ms,dur_ms,tok_in,tok_out,cache_read,cache_write,cost_usd,err,attempts)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				r.Ts, r.Key, r.Model, r.Provider, r.Status, b2i(r.Stream), r.TTFTms, r.DurMs,
				r.TokIn, r.TokOut, r.CacheRead, r.CacheWrt, r.CostUSD, r.Err, r.Attempts)
			if err != nil {
				log.Printf("store: insert: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("store: commit: %v", err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case r := <-s.ch:
			buf = append(buf, r)
			if len(buf) >= batch {
				flush()
			}
		case <-tick.C:
			flush()
		case <-s.done:
			// drain everything already accepted, then final flush
			for {
				select {
				case r := <-s.ch:
					buf = append(buf, r)
					if len(buf) >= batch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// ---- oauth token persistence (survives restarts; refresh tokens rotate) ----

// LoadOAuth returns stored tokens for an account; zero values when absent.
func (s *Store) LoadOAuth(providerName, acct string) (access, refresh string, expiresAt int64) {
	if s == nil {
		return "", "", 0
	}
	err := s.db.QueryRow(`SELECT access_token, refresh_token, expires_at FROM oauth_tokens WHERE provider=? AND acct=?`,
		providerName, acct).Scan(&access, &refresh, &expiresAt)
	if err != nil {
		return "", "", 0
	}
	return access, refresh, expiresAt
}

// SaveOAuth persists refreshed tokens (called on the refresh path only — rare).
func (s *Store) SaveOAuth(providerName, acct, access, refresh string, expiresAt int64) {
	if s == nil {
		return
	}
	_, _ = s.db.Exec(`INSERT INTO oauth_tokens (provider,acct,access_token,refresh_token,expires_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(provider,acct) DO UPDATE SET access_token=excluded.access_token,
		refresh_token=excluded.refresh_token, expires_at=excluded.expires_at`,
		providerName, acct, access, refresh, expiresAt)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Close drains pending records and closes the DB.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	close(s.done)
	s.wg.Wait()
	return s.db.Close()
}

// ---- queries (admin API) ----

type Summary struct {
	Requests   int64         `json:"requests"`
	Errors     int64         `json:"errors"`
	TokIn      int64         `json:"tok_in"`
	TokOut     int64         `json:"tok_out"`
	CacheRead  int64         `json:"cache_read"`
	CacheWrite int64         `json:"cache_write"`
	CostUSD    float64       `json:"cost_usd"`
	TTFTp50    *float64      `json:"ttft_p50_ms"`
	TTFTp95    *float64      `json:"ttft_p95_ms"`
	AvgDurMs   float64       `json:"avg_dur_ms"`
	Bucket     string        `json:"bucket"`
	Series     []SeriesPoint `json:"series"`
}

type SeriesPoint struct {
	Bucket   int64   `json:"ts"` // unix millis of bucket start
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	TokIn    int64   `json:"tok_in"`
	TokOut   int64   `json:"tok_out"`
	Cost     float64 `json:"cost"`
}

// SummarySince aggregates requests in the last `d`, bucketed for the chart.
func (s *Store) SummarySince(d time.Duration, bucket string) (*Summary, error) {
	since := time.Now().Add(-d).UnixMilli()
	sum := &Summary{Bucket: bucket}
	var ttfts []float64
	err := s.db.QueryRow(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(tok_in),0), COALESCE(SUM(tok_out),0),
			COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
			COALESCE(SUM(cost_usd),0), COALESCE(AVG(dur_ms),0)
		FROM requests WHERE ts >= ?`, since).
		Scan(&sum.Requests, &sum.Errors, &sum.TokIn, &sum.TokOut,
			&sum.CacheRead, &sum.CacheWrite, &sum.CostUSD, &sum.AvgDurMs)
	if err != nil {
		return nil, err
	}
	// percentiles are insertion-sorted (O(n²)); skip collection entirely for
	// long windows (the 12-month Usage heatmap fetch) — don't even load the
	// ttft values. LatencyTab only ever asks for 24h. Same user-visible
	// contract as an empty window: cards render "–".
	if d <= 30*24*time.Hour {
		rows, err := s.db.Query(`SELECT ttft_ms FROM requests WHERE ts >= ? AND ttft_ms > 0`, since)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var v float64
			if rows.Scan(&v) == nil {
				ttfts = append(ttfts, v)
			}
		}
		rows.Close()
		sum.TTFTp50 = percentiles(ttfts, 0.50)[0]
		sum.TTFTp95 = percentiles(ttfts, 0.95)[0]
	}

	group := bucketExpr(bucket)
	rows, err := s.db.Query(`SELECT `+group+`, COUNT(*),
			SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),
			COALESCE(SUM(tok_in),0), COALESCE(SUM(tok_out),0), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? GROUP BY 1 ORDER BY 1`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.Errors, &p.TokIn, &p.TokOut, &p.Cost); err == nil {
			sum.Series = append(sum.Series, p)
		}
	}
	if sum.Series == nil {
		sum.Series = []SeriesPoint{}
	}
	return sum, nil
}

// bucketExpr maps a bucket name to its SQL grouping expression ("minute" |
// "hour" | "day"; anything else = hour). Shared by SummarySince and the
// per-model time series.
func bucketExpr(bucket string) string {
	switch bucket {
	case "minute":
		return "ts/60000*60000"
	case "day":
		return "ts/86400000*86400000"
	}
	return "ts/3600000*3600000"
}

// ModelSeriesPoint is one model's token/cost totals inside one time bucket.
type ModelSeriesPoint struct {
	Bucket int64   `json:"ts"` // unix millis of bucket start
	Model  string  `json:"model"`
	TokIn  int64   `json:"tok_in"`
	TokOut int64   `json:"tok_out"`
	Cost   float64 `json:"cost"`
}

// TimeSeriesByModel groups tok/cost by (bucket, model) — the Usage tab's
// top-models-over-time chart. TokIn stays cache-exclusive, matching SeriesPoint.
func (s *Store) TimeSeriesByModel(d time.Duration, bucket string) ([]ModelSeriesPoint, error) {
	since := time.Now().Add(-d).UnixMilli()
	rows, err := s.db.Query(`SELECT `+bucketExpr(bucket)+`, model,
			COALESCE(SUM(tok_in),0), COALESCE(SUM(tok_out),0), COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? GROUP BY 1, 2 ORDER BY 1`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelSeriesPoint{}
	for rows.Next() {
		var p ModelSeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Model, &p.TokIn, &p.TokOut, &p.Cost); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

type Row struct {
	ID        int64   `json:"id"`
	Ts        int64   `json:"ts"`
	Key       string  `json:"key"`
	Model     string  `json:"model"`
	Provider  string  `json:"provider"`
	Status    int     `json:"status"`
	Stream    bool    `json:"stream"`
	TTFTms    float64 `json:"ttft_ms"`
	DurMs     float64 `json:"dur_ms"`
	TokIn     int     `json:"tok_in"`
	TokOut    int     `json:"tok_out"`
	CacheRead int     `json:"cache_read"`
	CacheWrt  int     `json:"cache_write"`
	CostUSD   float64 `json:"cost_usd"`
	Err       string  `json:"err"`
	Attempts  int     `json:"attempts"`
}

func (s *Store) Recent(limit int) ([]Row, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,ts,key_name,model,provider,status,stream,ttft_ms,dur_ms,
		tok_in,tok_out,cache_read,cache_write,cost_usd,err,attempts
		FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Row, 0, limit)
	for rows.Next() {
		var r Row
		var stream int
		if err := rows.Scan(&r.ID, &r.Ts, &r.Key, &r.Model, &r.Provider, &r.Status, &stream,
			&r.TTFTms, &r.DurMs, &r.TokIn, &r.TokOut, &r.CacheRead, &r.CacheWrt,
			&r.CostUSD, &r.Err, &r.Attempts); err == nil {
			r.Stream = stream == 1
			out = append(out, r)
		}
	}
	return out, nil
}

type Breakdown struct {
	Name     string   `json:"name"`
	Requests int64    `json:"requests"`
	Errors   int64    `json:"errors"`
	TokIn    int64    `json:"tok_in"`
	TokOut   int64    `json:"tok_out"`
	CacheRd  int64    `json:"cache_read"`
	CacheWrt int64    `json:"cache_write"` // totalInput needs it: card/table reconciliation
	Cost     float64  `json:"cost"`
	TTFTp50  *float64 `json:"ttft_p50_ms"`
}

// BreakdownBy groups usage over a period by column ("model"|"provider"|"key_name").
func (s *Store) BreakdownBy(col string, d time.Duration) ([]Breakdown, error) {
	if col != "model" && col != "provider" && col != "key_name" {
		return nil, sql.ErrNoRows
	}
	since := time.Now().Add(-d).UnixMilli()
	rows, err := s.db.Query(`SELECT `+col+`, COUNT(*),
			SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END),
			COALESCE(SUM(tok_in),0), COALESCE(SUM(tok_out),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
			COALESCE(SUM(cost_usd),0)
		FROM requests WHERE ts >= ? GROUP BY 1 ORDER BY COUNT(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Breakdown, 0)
	for rows.Next() {
		var b Breakdown
		if err := rows.Scan(&b.Name, &b.Requests, &b.Errors, &b.TokIn, &b.TokOut, &b.CacheRd, &b.CacheWrt, &b.Cost); err == nil {
			out = append(out, b)
		}
	}
	// ttft p50 per group (small scale: one extra query)
	ttrows, err := s.db.Query(`SELECT `+col+`, ttft_ms FROM requests WHERE ts >= ? AND ttft_ms > 0`, since)
	if err != nil {
		return out, nil
	}
	defer ttrows.Close()
	byGroup := map[string][]float64{}
	for ttrows.Next() {
		var g string
		var v float64
		if ttrows.Scan(&g, &v) == nil {
			byGroup[g] = append(byGroup[g], v)
		}
	}
	for i := range out {
		out[i].TTFTp50 = percentiles(byGroup[out[i].Name], 0.5)[0]
	}
	return out, nil
}

func percentiles(v []float64, ps ...float64) []*float64 {
	out := make([]*float64, len(ps))
	if len(v) == 0 {
		return out
	}
	// insertion sort (n small per personal use)
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	for i, p := range ps {
		idx := int(p * float64(len(v)-1))
		x := v[idx]
		out[i] = &x
	}
	return out
}
