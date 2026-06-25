package store

import (
	"context"
	"math"
	"sort"
	"time"
)

const (
	coAccessWorkingSet  = 6         // star-associate against the last N session files
	coAccessHalfLifeSec = 7 * 86400 // 7-day forgetting half-life
	coAccessMaxWeight   = 10.0      // clamp to prevent unbounded growth
	coAccessMinWeight   = 0.05      // prune edges below this after decay
)

// RecordCoAccess star-associates path with the current session working set
// (last coAccessWorkingSet recently-touched files). Each pair gets an LTP
// reinforcement: the edge weight decays exponentially since the last access
// (forgetting curve) then a reinforcement of 1.0 is added.
//
// Edges are stored canonically (src_path < dst_path) to avoid duplicates.
// Called from SessionAddFile / SessionTrackFile; silently no-ops when
// co-access is disabled or no session exists.
func (s *Store) RecordCoAccess(ctx context.Context, path string) error {
	if s.opts.DisableCoAccess {
		return nil
	}

	// Fetch recent working set from the current session.
	ss, ok, err := s.SessionGet(ctx)
	if err != nil || !ok || len(ss.Files) == 0 {
		return nil
	}
	ws := recentWorkingSet(ss.Files, path, coAccessWorkingSet)
	if len(ws) == 0 {
		return nil
	}

	now := time.Now()
	nowNs := now.UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, peer := range ws {
		src, dst := canonicalize(path, peer)
		var oldWeight float64
		var oldReinforced int64
		err := tx.QueryRowContext(ctx,
			`SELECT weight, reinforced_at FROM co_access_edges WHERE src_path=? AND dst_path=?`,
			src, dst).Scan(&oldWeight, &oldReinforced)
		var newWeight float64
		if err == nil {
			elapsedSec := float64(nowNs-oldReinforced) / 1e9
			decay := math.Exp(-elapsedSec * math.Log(2) / coAccessHalfLifeSec)
			newWeight = math.Min(oldWeight*decay+1.0, coAccessMaxWeight)
		} else {
			newWeight = 1.0
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO co_access_edges(src_path, dst_path, weight, reinforced_at)
			   VALUES(?, ?, ?, ?)
			   ON CONFLICT(src_path, dst_path) DO UPDATE SET
			     weight=excluded.weight,
			     reinforced_at=excluded.reinforced_at`,
			src, dst, newWeight, nowNs); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// CoAccessNeighbor is one weighted entry returned by CoAccessNeighborsWeighted.
// Weight is the raw stored weight in [coAccessMinWeight, coAccessMaxWeight].
type CoAccessNeighbor struct {
	Path   string
	Weight float64
}

// CoAccessNeighbors returns up to n file paths that are the strongest
// co-access associates of the given seed paths, ordered by descending weight.
// Silently returns nil on store error.
func (s *Store) CoAccessNeighbors(ctx context.Context, seeds []string, n int) ([]string, error) {
	wn, err := s.CoAccessNeighborsWeighted(ctx, seeds, n)
	if err != nil || len(wn) == 0 {
		return nil, err
	}
	paths := make([]string, len(wn))
	for i, w := range wn {
		paths[i] = w.Path
	}
	return paths, nil
}

// CoAccessNeighborsWeighted is CoAccessNeighbors but returns the raw weight
// per neighbor too. Used by the primary-rank spreading-activation bonus
// (search_quality.cooccurNeighborBonus) — without weights, every neighbor
// would contribute the same boost, erasing the LTP signal the graph spent
// session-time accumulating.
func (s *Store) CoAccessNeighborsWeighted(ctx context.Context, seeds []string, n int) ([]CoAccessNeighbor, error) {
	if s.opts.DisableCoAccess || len(seeds) == 0 || n <= 0 {
		return nil, nil
	}
	args := make([]any, len(seeds)*2)
	for i, p := range seeds {
		args[i] = p
		args[len(seeds)+i] = p
	}
	ph := inPlaceholders(len(seeds))
	q := `SELECT neighbor, MAX(weight) AS w FROM (` + //nolint:gosec
		` SELECT dst_path AS neighbor, weight FROM co_access_edges WHERE src_path IN (` + ph + `) AND weight >= ` + coAccessMinWeightStr +
		` UNION ALL` +
		` SELECT src_path AS neighbor, weight FROM co_access_edges WHERE dst_path IN (` + ph + `) AND weight >= ` + coAccessMinWeightStr +
		`) GROUP BY neighbor ORDER BY w DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, n)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	seedSet := make(map[string]struct{}, len(seeds))
	for _, p := range seeds {
		seedSet[p] = struct{}{}
	}

	var out []CoAccessNeighbor
	for rows.Next() {
		var p string
		var w float64
		if err := rows.Scan(&p, &w); err != nil {
			return nil, err
		}
		if _, isSeed := seedSet[p]; !isSeed {
			out = append(out, CoAccessNeighbor{Path: p, Weight: w})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	return out, nil
}

// PruneCoAccess removes edges whose decayed weight has fallen below
// coAccessMinWeight. Cheap maintenance call — suitable for the watch-idle tick.
// Decay is computed in Go because SQLite math functions (exp) are not always
// available (requires SQLITE_ENABLE_MATH_FUNCTIONS).
func (s *Store) PruneCoAccess(ctx context.Context) error {
	nowNs := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx,
		`SELECT src_path, dst_path, weight, reinforced_at FROM co_access_edges`)
	if err != nil {
		return err
	}
	type edge struct{ src, dst string }
	var toDelete []edge
	for rows.Next() {
		var src, dst string
		var weight float64
		var reinforcedAt int64
		if err := rows.Scan(&src, &dst, &weight, &reinforcedAt); err != nil {
			_ = rows.Close()
			return err
		}
		elapsedSec := float64(nowNs-reinforcedAt) / 1e9
		decayed := weight * math.Exp(-elapsedSec*math.Log(2)/coAccessHalfLifeSec)
		if decayed < coAccessMinWeight {
			toDelete = append(toDelete, edge{src, dst})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, e := range toDelete {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM co_access_edges WHERE src_path=? AND dst_path=?`,
			e.src, e.dst); err != nil {
			return err
		}
	}
	return nil
}

// canonicalize returns (src, dst) with src <= dst lexicographically so each
// pair is stored in exactly one canonical direction.
func canonicalize(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// recentWorkingSet returns up to n paths from files that are different from
// current. Files are already ordered DESC by touched_at from SessionGet.
func recentWorkingSet(files []SessionFile, current string, n int) []string {
	var out []string
	seen := map[string]bool{current: true}
	for _, f := range files {
		if seen[f.Path] {
			continue
		}
		seen[f.Path] = true
		out = append(out, f.Path)
		if len(out) >= n {
			break
		}
	}
	return out
}

const coAccessMinWeightStr = "0.05"
