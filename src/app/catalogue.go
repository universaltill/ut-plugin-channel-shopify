package app

import (
	"errors"
	"strings"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// Import cursor. The import reads products with variants and resumes from
// where it stopped; a finished import clears it, so the next one starts again
// from the top and (being deterministic) changes nothing. Stock is not part of
// it: the host refuses stock_observed in a pull, so the tick sweeps it
// (stock.go, ut-docs#6821).
const curProducts = "import.products"

// legacyImportCursors are the stock stage's cursors of 0.4.4 and earlier. An
// upgraded install may still hold them; the import clears them and never
// reads them.
var legacyImportCursors = []string{"import.stage", "import.stock.loc", "import.stock.after"}

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
	Description string      `json:"descriptionHtml"` // HTML: Product.description glues paragraphs ("ab"), live 2026-11-23
	Status      string      `json:"status"`
	UpdatedAt   string      `json:"updatedAt"`
	Variants    gqlVariants `json:"variants"`
	Featured    *struct {
		ID    string `json:"id"`
		Image *struct {
			URL string `json:"url"`
		} `json:"image"`
	} `json:"featuredMedia"`
}

type shopInfo struct {
	Currency      string `json:"currencyCode"`
	TaxesIncluded bool   `json:"taxesIncluded"`
}

type importStats struct{ imported, skipped int64 }

// importCatalogue pulls the shop's products page by page and submits them as
// listings. Records are deterministic (external_version is Shopify's
// updatedAt), so a second import is byte-identical and the host reports zero
// changes. It submits no stock: stock_observed is refused outside a run
// (ut-docs#6821). Nothing is ever written to Shopify.
func importCatalogue(channel.Event) (any, error) {
	if cursor(legacyImportCursors[0], cursorRe) != "" { // only an upgraded install has it
		for _, k := range legacyImportCursors {
			if err := cursorSet(k, nil); err != nil {
				return nil, err
			}
		}
	}
	var st importStats
	done, err := importProducts(&st)
	if err != nil {
		return nil, err
	}
	return map[string]any{"done": done, "imported": st.imported, "skipped": st.skipped}, nil
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
		Title:    clip(p.Title, 255), Description: clip(NormalizeText(p.Description), 5000), Status: listingStatus(p.Status),
	}
	// The first image only (ADR-0212): the cloud downloads and hashes it itself.
	if f := p.Featured; f != nil && f.Image != nil && okImageURL(f.Image.URL) && f.ID != "" {
		l.Images = []channel.Image{{ExternalID: f.ID, URL: f.Image.URL, Position: 1}}
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
			ExternalID: v.ID, SKU: clip(strings.TrimSpace(v.SKU), 100), PriceMinor: price, Currency: shop.Currency,
			TaxIncluded: shop.TaxesIncluded && v.Taxable, Tracked: v.InventoryItem.Tracked,
		}
		if v.CompareAtPrice != nil {
			if c, ok := parseMoney(*v.CompareAtPrice, shop.Currency); ok && c > 0 {
				out.CompareAtMinor = c
			}
		}
		if strings.TrimSpace(v.Barcode) != "" {
			out.Barcodes = []string{clip(strings.TrimSpace(v.Barcode), 64)}
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
