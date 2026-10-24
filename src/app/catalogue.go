package app

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Import cursors. The import is two stages: products with variants, then the
// stock level of every tracked variant at every location. Each stage resumes
// from where it stopped; a finished import clears them all, so the next one
// starts again from the top and (being deterministic) changes nothing.
const (
	curStage    = "import.stage"
	curProducts = "import.products"
	curLoc      = "import.stock.loc"
	curLocAfter = "import.stock.after"
)

var stageRe = regexp.MustCompile(`^stock$`)

type pageInfoT struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type gqlVariant struct {
	ID              string  `json:"id"`
	SKU             string  `json:"sku"`
	Barcode         string  `json:"barcode"`
	Price           string  `json:"price"`
	CompareAtPrice  *string `json:"compareAtPrice"`
	Taxable         bool    `json:"taxable"`
	SelectedOptions []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"selectedOptions"`
	InventoryItem struct {
		Tracked bool `json:"tracked"`
	} `json:"inventoryItem"`
}

type gqlVariants struct {
	PageInfo pageInfoT    `json:"pageInfo"`
	Nodes    []gqlVariant `json:"nodes"`
}

// gqlCategorised is what a product says about its category (ut-docs#4944).
type gqlCategorised struct {
	ProductType string `json:"productType"`
	Category    *struct {
		Name string `json:"name"`
	} `json:"category"`
	Collections struct {
		Nodes []struct {
			Title string `json:"title"`
		} `json:"nodes"`
	} `json:"collections"`
}

// single is the ONE category a till item has (ut-docs#4944): the merchant's
// product type, else the standard category's name, else the first collection,
// else "". The other collections stay in Shopify. Push and import share this,
// so what import would read is never written back.
func (c gqlCategorised) single() string {
	if s := categoryName(c.ProductType); s != "" {
		return s
	}
	if c.Category != nil {
		if s := categoryName(c.Category.Name); s != "" {
			return s
		}
	}
	if len(c.Collections.Nodes) > 0 {
		return categoryName(c.Collections.Nodes[0].Title)
	}
	return ""
}

type gqlProduct struct {
	gqlCategorised
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Status      string      `json:"status"`
	UpdatedAt   string      `json:"updatedAt"`
	Variants    gqlVariants `json:"variants"`
}

type shopInfo struct {
	Currency      string `json:"currencyCode"`
	TaxesIncluded bool   `json:"taxesIncluded"`
}

type importStats struct{ imported, observed, skipped int64 }

// importCatalogue pulls the shop's products and stock page by page and
// submits them as listings and stock observations. Records are deterministic
// (external_version is Shopify's updatedAt; a stock observation's is the
// level's updatedAt and quantity), so a second import is byte-identical and
// the host reports zero changes. Nothing is ever written to Shopify.
func importCatalogue(channel.Event) (any, error) {
	var st importStats
	done := false
	if cursor(curStage, stageRe) == "" {
		finished, err := importProducts(&st)
		if err != nil {
			return nil, err
		}
		if finished {
			if err := cursorSet(curStage, []byte("stock")); err != nil {
				return nil, err
			}
		}
	}
	if cursor(curStage, stageRe) == "stock" {
		finished, err := importStock(&st)
		if err != nil {
			return nil, err
		}
		done = finished
	}
	return map[string]any{"done": done, "imported": st.imported, "observed": st.observed, "skipped": st.skipped}, nil
}

func importProducts(st *importStats) (finished bool, err error) {
	next := cursor(curProducts, cursorRe)
	for pages := 0; pages < maxProductPages && spent.cost < importCostBudget; pages++ {
		vars := map[string]any{"first": productPage, "vfirst": variantPage}
		if next != "" {
			vars["after"] = next
		}
		var d struct {
			Shop     shopInfo `json:"shop"`
			Products struct {
				PageInfo pageInfoT    `json:"pageInfo"`
				Nodes    []gqlProduct `json:"nodes"`
			} `json:"products"`
		}
		if err := run(qProducts, vars, "", &d); err != nil {
			return false, err
		}
		if !curRe.MatchString(d.Shop.Currency) {
			return false, errors.New("the shop reported no usable currency")
		}
		listings := make([]channel.Listing, 0, len(d.Products.Nodes))
		for _, p := range d.Products.Nodes {
			l, reason := mapProduct(&p, d.Shop)
			if reason != "" {
				st.skipped++
				hostLog(channel.LevelWarn, "import.refused."+reason, map[string]any{})
				continue
			}
			listings = append(listings, l)
		}
		if len(listings) > 0 {
			if _, err := submit(channel.KindListing, listings); err != nil {
				return false, err
			}
			st.imported += int64(len(listings))
		}
		pi := d.Products.PageInfo
		next = ""
		if pi.HasNextPage {
			if !cursorRe.MatchString(pi.EndCursor) {
				return false, errors.New("channel sent a page cursor that is not a cursor")
			}
			next = pi.EndCursor
		}
		if err := cursorSet(curProducts, []byte(next)); err != nil {
			return false, err
		}
		hostLog(channel.LevelInfo, "import.page", map[string]any{"records": len(listings), "skipped": st.skipped, "cost_left": int64(lastCost.Available)})
		if next == "" {
			return true, nil
		}
	}
	return false, nil
}

// completeVariants follows a product's variant pages. A product past the
// manifest's variant limit stops early; the caller refuses it.
func completeVariants(p *gqlProduct) error {
	for extra := 0; p.Variants.PageInfo.HasNextPage && len(p.Variants.Nodes) <= maxVariants && extra < 3; extra++ {
		if !okGID(tProduct, p.ID) || !cursorRe.MatchString(p.Variants.PageInfo.EndCursor) {
			return nil
		}
		var d struct {
			Node *struct {
				Variants gqlVariants `json:"variants"`
			} `json:"node"`
		}
		vars := map[string]any{"id": p.ID, "first": variantPage, "after": p.Variants.PageInfo.EndCursor}
		if err := run(qMoreVariants, vars, "", &d); err != nil {
			return err
		}
		if d.Node == nil {
			return nil
		}
		p.Variants.Nodes = append(p.Variants.Nodes, d.Node.Variants.Nodes...)
		p.Variants.PageInfo = d.Node.Variants.PageInfo
	}
	return nil
}

// mapProduct turns Shopify's product into the contract's listing. A product
// that cannot be mapped is refused with a reason code (logged), never
// truncated or repaired: a shop over the option or variant limit gets a
// refusal, not a truncation (ADR-0169 section 7).
func mapProduct(p *gqlProduct, shop shopInfo) (channel.Listing, string) {
	if !okGID(tProduct, p.ID) || !timeRe.MatchString(p.UpdatedAt) {
		return channel.Listing{}, "bad_product"
	}
	if err := completeVariants(p); err != nil {
		logErr("import.variants_failed", err)
		return channel.Listing{}, "variants_unavailable"
	}
	if len(p.Variants.Nodes) > maxVariants || p.Variants.PageInfo.HasNextPage {
		return channel.Listing{}, "too_many_variants"
	}
	l := channel.Listing{
		Identity: channel.Identity{ExternalID: p.ID, ExternalVersion: p.UpdatedAt},
		Title:    clip(p.Title, 255), Description: clip(p.Description, 5000), Status: listingStatus(p.Status),
	}
	if c := p.single(); c != "" {
		l.CategoryPath = []string{c}
	}
	for _, v := range p.Variants.Nodes {
		price, ok := parseMoney(v.Price, shop.Currency)
		if !okGID(tVariant, v.ID) || !ok {
			return channel.Listing{}, "bad_variant"
		}
		out := channel.Variant{
			ExternalID: v.ID, SKU: clip(v.SKU, 100), PriceMinor: price, Currency: shop.Currency,
			TaxIncluded: shop.TaxesIncluded && v.Taxable, Tracked: v.InventoryItem.Tracked,
		}
		if v.CompareAtPrice != nil {
			if c, ok := parseMoney(*v.CompareAtPrice, shop.Currency); ok && c > 0 {
				out.CompareAtMinor = c
			}
		}
		if v.Barcode != "" {
			out.Barcodes = []string{clip(v.Barcode, 64)}
		}
		opts := map[string]string{}
		for _, o := range v.SelectedOptions {
			if o.Name == "Title" && o.Value == "Default Title" { // Shopify's placeholder for "no options"
				continue
			}
			opts[clip(o.Name, 100)] = clip(o.Value, 100)
		}
		if len(opts) > maxOptions {
			return channel.Listing{}, "too_many_options"
		}
		if len(opts) > 0 {
			out.Options = opts
		}
		l.Variants = append(l.Variants, out)
	}
	return l, ""
}

func listingStatus(s string) string {
	switch s {
	case "ACTIVE":
		return "active"
	case "ARCHIVED":
		return "archived"
	}
	return "draft"
}

// importStock reads every location's inventory levels and submits the
// available quantity of each tracked variant as an observation. The channel's
// number is an observation; Universal Till decides (contract section 2).
func importStock(st *importStats) (finished bool, err error) {
	var ld struct {
		Locations struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"locations"`
	}
	if err := run(qLocations, map[string]any{"first": maxLocations + 1}, "", &ld); err != nil {
		return false, err
	}
	if len(ld.Locations.Nodes) > maxLocations {
		// Never drop stock silently: the shop has more locations than one
		// import reads. The first maxLocations are imported.
		hostLog(channel.LevelWarn, "import.locations_truncated", map[string]any{"read": maxLocations})
		ld.Locations.Nodes = ld.Locations.Nodes[:maxLocations]
	}
	var locs []string
	for _, n := range ld.Locations.Nodes {
		if okGID(tLocation, n.ID) {
			locs = append(locs, n.ID)
		}
	}
	idx, after := 0, ""
	if cur := cursor(curLoc, gidRes[tLocation]); cur != "" {
		for i, l := range locs {
			if l == cur {
				idx, after = i, cursor(curLocAfter, cursorRe)
			}
		}
	}
	for pages := 0; idx < len(locs); {
		if pages >= maxStockPages || spent.cost >= importCostBudget {
			if err := cursorSet(curLoc, []byte(locs[idx])); err != nil {
				return false, err
			}
			return false, cursorSet(curLocAfter, []byte(after))
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
			return false, err
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
					return false, err
				}
				st.observed += int64(len(obs))
			}
			if pi := d.Location.Levels.PageInfo; pi.HasNextPage {
				if !cursorRe.MatchString(pi.EndCursor) {
					return false, errors.New("channel sent a page cursor that is not a cursor")
				}
				after = pi.EndCursor
			}
		}
		if after == "" {
			idx++
		}
	}
	for _, k := range []string{curStage, curLoc, curLocAfter} {
		if err := cursorSet(k, nil); err != nil {
			return false, err
		}
	}
	return true, nil
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
