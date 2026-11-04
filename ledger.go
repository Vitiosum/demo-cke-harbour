package main

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The harbour ledger lives in a Clever Cloud managed PostgreSQL, outside the cluster.
// Every container unloaded by a crane (a pod) is written here, so the history
// survives rolling updates, scale-downs and pod restarts.
type ledger struct {
	pool *pgxpool.Pool
}

// cargoSeed is the fictional catalogue of goods arriving in the harbour.
var cargoSeed = []struct {
	goods, origin string
	tonnes        float64
}{
	{"Coffee beans", "Santos", 21.5},
	{"Bicycle frames", "Kaohsiung", 8.2},
	{"Sheet music", "Hamburg", 3.1},
	{"Solar panels", "Ningbo", 17.9},
	{"Darjeeling tea", "Kolkata", 12.4},
	{"Merino wool", "Fremantle", 14.0},
	{"Vinyl records", "Rotterdam", 6.6},
	{"Olive oil", "Piraeus", 19.3},
	{"Cocoa", "Abidjan", 22.8},
	{"Wind turbine bolts", "Esbjerg", 24.0},
	{"Library books", "Montreal", 9.7},
	{"Ceramic tiles", "Valencia", 23.1},
	{"Laptops", "Busan", 7.4},
	{"Running shoes", "Ho Chi Minh City", 5.9},
	{"Maple syrup", "Halifax", 18.2},
	{"Basmati rice", "Mundra", 22.0},
	{"Rubber ducks", "Shenzhen", 2.3},
	{"Upright pianos", "Antwerp", 11.6},
	{"Vanilla pods", "Toamasina", 1.8},
	{"Telescope mirrors", "Genoa", 4.5},
}

func openLedger(ctx context.Context, uri string) (*ledger, error) {
	cfg, err := pgxpool.ParseConfig(uri)
	if err != nil {
		return nil, err
	}
	// 3 connections per pod: 6 pods at most (HPA) + 1 during a rollout = 21,
	// well below the 45 connections of the xxs_sml plan.
	cfg.MaxConns = 3
	cfg.MaxConnIdleTime = time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &ledger{pool: pool}, nil
}

// migrate creates the tables and the cargo catalogue. It is idempotent and safe
// when several pods start at the same time (advisory lock).
func (l *ledger) migrate(ctx context.Context) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	steps := []string{
		`SELECT pg_advisory_xact_lock(6346)`,
		`CREATE TABLE IF NOT EXISTS cargo (
			id     int PRIMARY KEY,
			goods  text NOT NULL,
			origin text NOT NULL,
			tonnes numeric(4,1) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ledger (
			id          bigserial PRIMARY KEY,
			unloaded_at timestamptz NOT NULL DEFAULT now(),
			container   text NOT NULL,
			cargo_id    int NOT NULL REFERENCES cargo(id),
			crane       text NOT NULL,
			version     text NOT NULL,
			node        text NOT NULL)`,
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	for i, c := range cargoSeed {
		if _, err := tx.Exec(ctx,
			`INSERT INTO cargo (id, goods, origin, tonnes) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO NOTHING`,
			i+1, c.goods, c.origin, c.tonnes); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type entry struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Container string    `json:"container"`
	Goods     string    `json:"goods"`
	Origin    string    `json:"origin"`
	Tonnes    float64   `json:"tonnes"`
	Crane     string    `json:"crane"`
	Version   string    `json:"version"`
	Node      string    `json:"node"`
}

// record writes one unloaded container and returns the full ledger line.
func (l *ledger) record(ctx context.Context, crane, version, node string) (entry, error) {
	e := entry{Container: newContainerCode(), Crane: crane, Version: version, Node: node}
	err := l.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO ledger (container, cargo_id, crane, version, node)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, unloaded_at, cargo_id)
		SELECT ins.id, ins.unloaded_at, cargo.goods, cargo.origin, cargo.tonnes::float8
		FROM ins JOIN cargo ON cargo.id = ins.cargo_id`,
		e.Container, rand.IntN(len(cargoSeed))+1, crane, version, node,
	).Scan(&e.ID, &e.At, &e.Goods, &e.Origin, &e.Tonnes)
	return e, err
}

type craneTally struct {
	Crane   string    `json:"crane"`
	Version string    `json:"version"`
	Count   int64     `json:"count"`
	Last    time.Time `json:"last"`
}

type ledgerView struct {
	Total   int64            `json:"total"`
	Tonnes  float64          `json:"tonnes"`
	Recent  []entry          `json:"recent"`
	Cranes  []craneTally     `json:"cranes"`
	Version map[string]int64 `json:"byVersion"`
}

// recent returns the last lines of the ledger and what each crane has unloaded.
func (l *ledger) recent(ctx context.Context) (ledgerView, error) {
	v := ledgerView{Version: map[string]int64{}}
	if err := l.pool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(cargo.tonnes), 0)::float8 FROM ledger JOIN cargo ON cargo.id = ledger.cargo_id`,
	).Scan(&v.Total, &v.Tonnes); err != nil {
		return v, err
	}
	rows, err := l.pool.Query(ctx, `
		SELECT ledger.id, unloaded_at, container, goods, origin, tonnes::float8, crane, version, node
		FROM ledger JOIN cargo ON cargo.id = ledger.cargo_id
		ORDER BY ledger.id DESC LIMIT 12`)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.At, &e.Container, &e.Goods, &e.Origin, &e.Tonnes, &e.Crane, &e.Version, &e.Node); err != nil {
			return v, err
		}
		v.Recent = append(v.Recent, e)
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	rows, err = l.pool.Query(ctx, `
		SELECT crane, version, count(*), max(unloaded_at) FROM ledger
		GROUP BY crane, version ORDER BY max(unloaded_at) DESC LIMIT 60`)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var t craneTally
		if err := rows.Scan(&t.Crane, &t.Version, &t.Count, &t.Last); err != nil {
			return v, err
		}
		v.Cranes = append(v.Cranes, t)
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	rows, err = l.pool.Query(ctx, `SELECT version, count(*) FROM ledger GROUP BY version`)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return v, err
		}
		v.Version[name] = n
	}
	return v, rows.Err()
}

func (l *ledger) ping(ctx context.Context) error {
	var one int
	return l.pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
}
