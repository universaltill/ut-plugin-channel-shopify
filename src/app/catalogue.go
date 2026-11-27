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
	UpdatedAt       string  `json:"updatedAt"`
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
	// Media is the variant's own media (ProductVariant.media, API 2026-10);
	// only the ids are read, they point into Product.media (ut-docs#7101).
	Media *struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	} `json:"media"`
}

// outVariant is channel.Variant plus the additive contract field
// variants[].external_version (sales-channel-v1 s7; the SDK type has none):
// the variant's own updatedAt, so the cloud can tell a variant-only edit.
// Absent or unreadable = omitted (older/unknown), never a refusal.
type outVariant struct {
	channel.Variant
	// VariantImageIndex is the 0-based index into the listing's images of the
	// variant's own first image (ut-docs#7101); nil = none.
	VariantImageIndex *int   `json:"variant_image_index,omitempty"`
	ExternalVersion   string `json:"external_version,omitempty"`
}

// outListing shadows Listing.Variants with outVariant (the outer field wins in JSON).
type outListing struct {
	channel.Listing
	Variants []outVariant `json:"variants,omitempty"`
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
	IsGiftCard  bool        `json:"isGiftCard"`
	// Media is Product.media in the merchant's order (ut-docs#7101).
	Media *struct {
		Nodes []gqlMedia `json:"nodes"`
	} `json:"media"`
	// Featured is the legacy primary image, used only when Media is absent.
	Featured *gqlMedia `json:"featuredMedia"`
}

// gqlMedia is a Media node; only a MediaImage has alt and image { url }.
type gqlMedia struct {
	ID    string `json:"id"`
	Alt   string `json:"alt"`
	Image *struct {
		URL string `json:"url"`
	} `json:"image"`
}

// maxListingImages is the gallery cap (ADR-0218): ten per owner.
const maxListingImages = 10

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
		listings := make([]outListing, 0, len(d.Products.Nodes))
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

// fetchProducts re-reads the named products and submits them as listings (the
// product doorbell, ut-docs#7084). ids are well-formed product gids. Products
// are read productsPerRead to a request through ONE nodes() query, so N ids
// cost N/productsPerRead requests; a node that is null is gone or unreadable
// and is skipped (nothing is deleted; the next full re-read marks the link).
// It stops between requests when the ask's budget is used and returns the ids
// it did not read. The rows are exactly an import's rows (mapProduct), and no
// stock is submitted (stock observed outside a run is refused).
func fetchProducts(ids []string) (read int, deferred []string, err error) {
	seen := map[string]bool{}
	var todo []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			todo = append(todo, id)
		}
	}
	for len(todo) > 0 {
		if spent.cost+qProductsByID.Cost > productFetchBudget {
			deferred = todo
			hostLog(channel.LevelWarn, "fetch.deferred_products", map[string]any{"read": read, "deferred": len(todo)})
			break
		}
		n := min(productsPerRead, len(todo))
		batch := todo[:n]
		todo = todo[n:]
		var d struct {
			Shop  shopInfo      `json:"shop"`
			Nodes []*gqlProduct `json:"nodes"`
		}
		if err := run(qProductsByID, map[string]any{"ids": batch, "vfirst": variantPage}, "", &d); err != nil {
			return read, nil, err
		}
		if !curRe.MatchString(d.Shop.Currency) {
			return read, nil, errors.New("the shop reported no usable currency")
		}
		asked := map[string]bool{}
		for _, id := range batch {
			asked[id] = true
		}
		listings := make([]outListing, 0, len(d.Nodes))
		for _, p := range d.Nodes {
			if p == nil || p.ID == "" {
				hostLog(channel.LevelInfo, "fetch.skip_id", map[string]any{"reason": "gone"})
				continue
			}
			if !asked[p.ID] { // an answer about a product that was not asked for is never used
				hostLog(channel.LevelWarn, "fetch.skip_id", map[string]any{"reason": "not_asked"})
				continue
			}
			l, reason := mapProduct(p, d.Shop)
			if reason != "" {
				hostLog(channel.LevelWarn, "fetch.refused."+reason, map[string]any{})
				continue
			}
			listings = append(listings, l)
		}
		if len(listings) > 0 {
			if _, err := submit(channel.KindListing, listings); err != nil {
				return read, nil, err
			}
			read += len(listings)
		}
	}
	if len(deferred) > maxFetchIDs {
		deferred = deferred[:maxFetchIDs]
	}
	return read, deferred, nil
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
func mapProduct(p *gqlProduct, shop shopInfo) (outListing, string) {
	if !okGID(tProduct, p.ID) || !timeRe.MatchString(p.UpdatedAt) {
		return outListing{}, "bad_product"
	}
	if err := completeVariants(p); err != nil {
		logErr("import.variants_failed", err)
		return outListing{}, "variants_unavailable"
	}
	if len(p.Variants.Nodes) > maxVariants || p.Variants.PageInfo.HasNextPage {
		return outListing{}, "too_many_variants"
	}
	var l outListing
	l.Listing = channel.Listing{
		Identity: channel.Identity{ExternalID: p.ID, ExternalVersion: p.UpdatedAt},
		Title:    clip(p.Title, 255), Description: clip(NormalizeText(p.Description), 5000), Status: listingStatus(p.Status),
	}
	if p.IsGiftCard {
		l.Kind = "gift_card" // ut-docs#7099, ADR-0220: the cloud decides what to do with it
	}
	// Every product image in order with its alt text (ut-docs#7101, ADR-0218); the
	// cloud downloads and hashes them itself, behind its own flags. Video/3D/external
	// media and non-https urls are skipped. Product.media is in the merchant's order.
	imgIdx := map[string]int{} // media id -> index into l.Images
	add := func(m gqlMedia) {
		if len(l.Images) >= maxListingImages || m.ID == "" || m.Image == nil || !okImageURL(m.Image.URL) {
			return
		}
		imgIdx[m.ID] = len(l.Images)
		l.Images = append(l.Images, channel.Image{ExternalID: m.ID, URL: m.Image.URL, Position: len(l.Images) + 1, Alt: clip(strings.TrimSpace(m.Alt), 512)})
	}
	if p.Media != nil {
		for _, m := range p.Media.Nodes {
			add(m)
		}
	} else if p.Featured != nil {
		add(*p.Featured)
	}
	if c := p.single(); c != "" {
		l.CategoryPath = []string{c}
	}
	for _, v := range p.Variants.Nodes {
		price, ok := parseMoney(v.Price, shop.Currency)
		if !okGID(tVariant, v.ID) || !ok {
			return outListing{}, "bad_variant"
		}
		out := channel.Variant{
			ExternalID: v.ID, SKU: clip(strings.TrimSpace(v.SKU), 100), PriceMinor: price, Currency: shop.Currency,
			// TaxIncluded is the shop's price basis, whatever the variant's tax
			// state (ut-docs#7099): folding "not taxable" into it made the cloud
			// gross a final price up by the default rate.
			TaxIncluded: shop.TaxesIncluded, Tracked: v.InventoryItem.Tracked,
		}
		tx := v.Taxable
		out.Taxable = &tx
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
			return outListing{}, "too_many_options"
		}
		if len(opts) > 0 {
			out.Options = opts
		}
		ov := outVariant{Variant: out}
		if v.Media != nil {
			for _, m := range v.Media.Nodes {
				if i, ok := imgIdx[m.ID]; ok {
					i := i
					ov.VariantImageIndex = &i
					break
				}
			}
		}
		if timeRe.MatchString(v.UpdatedAt) {
			ov.ExternalVersion = v.UpdatedAt
		}
		l.Variants = append(l.Variants, ov)
	}
	if len(l.Variants) == 1 && l.Variants[0].VariantImageIndex != nil { // contract: listing-level field
		i := *l.Variants[0].VariantImageIndex
		l.VariantImageIndex = &i
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
