package app

import (
	"errors"
	"strconv"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Stock sweep cursors (tick). Empty means the next tick starts a new pass.
const (
	curStockLoc   = "tick.stock.loc"
	curStockAfter = "tick.stock.after"
)

// sweepStock reads inventory levels and submits the available quantity of each
// tracked variant at each location as an observation. The channel's number is
// an observation; Universal Till decides (contract section 2). It runs in the
// tick, after the orders, because the host accepts stock_observed in a tick
// (not in a pull, ut-docs#6821), and it reads at most maxTickStockPages pages
// while the ask's cost budget has room. It resumes from tick.stock.* and,
// once a pass has read every location, clears them: the next tick starts a
// new pass, a rolling reconciliation whose records are deterministic (a
// re-observation of an unchanged level is a host no-op).
func sweepStock() (observedN int, err error) {
	if spent.cost+qLocations.Cost+qLevels.Cost > askCostBudget {
		return 0, nil // orders used the ask: the stock waits for the next tick
	}
	truncated := false
	var ld struct {
		Locations struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"locations"`
	}
	if err := run(qLocations, map[string]any{"first": maxLocations + 1}, "", &ld); err != nil {
		return observedN, err
	}
	if len(ld.Locations.Nodes) > maxLocations {
		// Never drop stock silently: the shop has more locations than one
		// sweep reads. The first maxLocations are swept (logged once per pass).
		truncated = true
		ld.Locations.Nodes = ld.Locations.Nodes[:maxLocations]
	}
	var locs []string
	for _, n := range ld.Locations.Nodes {
		if okGID(tLocation, n.ID) {
			locs = append(locs, n.ID)
		}
	}
	idx, after := 0, ""
	cur := cursor(curStockLoc, gidRes[tLocation])
	for i, l := range locs {
		if cur != "" && l == cur {
			idx, after = i, cursor(curStockAfter, cursorRe)
		}
	}
	if truncated && cur == "" {
		hostLog(channel.LevelWarn, "stock.locations_truncated", map[string]any{"read": maxLocations})
	}
	for pages := 0; idx < len(locs); {
		if pages >= maxTickStockPages || spent.cost+qLevels.Cost > askCostBudget {
			return observedN, nil // the place was saved after the last page
		}
		pages++
		vars := map[string]any{"id": locs[idx], "first": stockPage}
		if after != "" {
			vars["after"] = after
		}
		var d struct {
			Location *struct {
				Levels struct {
					PageInfo pageInfoT `json:"pageInfo"`
					Nodes    []struct {
						UpdatedAt string `json:"updatedAt"`
						Item      struct {
							Tracked bool `json:"tracked"`
							Variant *struct {
								ID string `json:"id"`
							} `json:"variant"`
						} `json:"item"`
						Quantities []struct {
							Quantity int64 `json:"quantity"`
						} `json:"quantities"`
					} `json:"nodes"`
				} `json:"inventoryLevels"`
			} `json:"location"`
		}
		if err := run(qLevels, vars, "", &d); err != nil {
			return observedN, err
		}
		after = ""
		if d.Location != nil {
			var obs []channel.StockObserved
			for _, n := range d.Location.Levels.Nodes {
				if !n.Item.Tracked || n.Item.Variant == nil || !okGID(tVariant, n.Item.Variant.ID) || len(n.Quantities) != 1 ||
					n.Quantities[0].Quantity < 0 || !timeRe.MatchString(n.UpdatedAt) {
					continue
				}
				obs = append(obs, observed(n.Item.Variant.ID, locs[idx], n.Quantities[0].Quantity, n.UpdatedAt))
			}
			if len(obs) > 0 {
				if err := submitObserved(obs); err != nil {
					return observedN, err
				}
				observedN += len(obs)
			}
			if pi := d.Location.Levels.PageInfo; pi.HasNextPage {
				if !cursorRe.MatchString(pi.EndCursor) {
					return observedN, errors.New("channel sent a page cursor that is not a cursor")
				}
				after = pi.EndCursor
			}
		}
		if after == "" {
			idx++
		}
		// Save the place after EVERY page: a later failure (a throttle the
		// host cannot clear, a bad cursor) must not make the next tick read
		// this page again and fail at the same spot.
		if idx < len(locs) {
			if err := cursorSet(curStockLoc, []byte(locs[idx])); err != nil {
				return observedN, err
			}
			if err := cursorSet(curStockAfter, []byte(after)); err != nil {
				return observedN, err
			}
		}
	}
	for _, k := range []string{curStockLoc, curStockAfter} {
		if err := cursorSet(k, nil); err != nil {
			return observedN, err
		}
	}
	return observedN, nil
}

func submitObserved(recs []channel.StockObserved) error {
	for len(recs) > 0 {
		n := len(recs)
		if n > channel.MaxBatchRecords {
			n = channel.MaxBatchRecords
		}
		if _, err := submit(channel.KindStockObserved, recs[:n]); err != nil {
			return err
		}
		recs = recs[n:]
	}
	return nil
}

// observed builds a stock observation. Its identity is the variant at the
// location; its version is the level's updatedAt and quantity, so it changes
// exactly when the quantity does.
func observed(variant, location string, available int64, version string) channel.StockObserved {
	return channel.StockObserved{
		Identity: channel.Identity{ExternalID: variant + "@" + location, ExternalVersion: version + ":" + strconv.FormatInt(available, 10)},
		Variant:  variant, Location: location, Available: available,
	}
}
