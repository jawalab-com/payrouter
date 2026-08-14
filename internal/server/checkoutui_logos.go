package server

import (
	"embed"
	"encoding/base64"
	"html/template"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// checkoutLogoFiles holds the official, full-colour brand marks (banks,
// e-wallets, retail outlets, card schemes) shown on the hosted checkout page.
// They are the authentic marks — not monochrome icon-pack reductions — because
// a slightly-wrong logo reads as untrustworthy on a payment page. SVGs are
// inlined directly; the few brands with no vector source (Indomaret) are
// embedded as small palette-PNGs in a data URI. Sources and licensing are
// documented in NOTICE.md at the repository root.
//
//go:embed checkoutui/assets/logos
var checkoutLogoFiles embed.FS

// logoAliases maps the method keys the templates pass to {{logo ...}} onto the
// embedded file basenames.
var logoAliases = map[string]string{
	"bank_bca":          "bca",
	"bank_mandiri":      "mandiri",
	"bank_bni":          "bni",
	"bank_bri":          "bri",
	"bank_permata":      "permata",
	"id_qris":           "qris",
	"ewallet_gopay":     "gopay",
	"ewallet_shopeepay": "shopee",
	"ewallet_dana":      "dana",
	"ewallet_ovo":       "ovo",
	"id_retail":         "alfamart",
	"retail_outlet":     "alfamart",
	"credit_card":       "mastercard",
	"card":              "mastercard",
}

// embeddedLogos is the sanitized contents of every logo file, keyed by file
// basename, loaded once at startup.
var embeddedLogos = loadEmbeddedLogos()

func loadEmbeddedLogos() map[string]template.HTML {
	out := make(map[string]template.HTML)
	entries, err := checkoutLogoFiles.ReadDir("checkoutui/assets/logos")
	if err != nil {
		panic("checkout: embedded logos unreadable: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := checkoutLogoFiles.ReadFile("checkoutui/assets/logos/" + e.Name())
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".svg":
			out[name] = template.HTML(inlineSVG(string(b)))
		case ".png":
			out[name] = template.HTML(`<img src="data:image/png;base64,` +
				base64.StdEncoding.EncodeToString(b) + `" alt="` + name + `">`)
		}
	}
	return out
}

var (
	xmlPrologRe  = regexp.MustCompile(`(?s)\A\s*(?:<\?xml[^>]*\?>|<!DOCTYPE[^>]*>)\s*`)
	svgOpenTagRe = regexp.MustCompile(`<svg\b[^>]*>`)
	sizeAttrRe   = regexp.MustCompile(`\s(?:width|height)="[0-9.]*"`)
)

// inlineSVG readies a standalone SVG file for embedding mid-document: it drops
// the XML prolog/doctype, derives a viewBox from width/height when the file
// omits one (some sources do), and strips the intrinsic size so the page CSS —
// not the file — controls how large the mark renders.
func inlineSVG(s string) string {
	s = xmlPrologRe.ReplaceAllString(s, "")
	open := svgOpenTagRe.FindString(s)
	if open == "" {
		return s
	}
	tag := open
	if !strings.Contains(tag, "viewBox") {
		w, h := attrInt(tag, "width"), attrInt(tag, "height")
		if w > 0 && h > 0 {
			tag = strings.Replace(tag, "<svg",
				`<svg viewBox="0 0 `+strconv.Itoa(w)+` `+strconv.Itoa(h)+`"`, 1)
		}
	}
	tag = sizeAttrRe.ReplaceAllString(tag, "")
	return strings.Replace(s, open, tag, 1)
}

// attrInt pulls a numeric width/height attribute value out of a tag, tolerating
// decimals by truncating ("1000.0003" -> 1000).
func attrInt(tag, key string) int {
	m := regexp.MustCompile(`\b` + key + `="([0-9.]+)"`).FindStringSubmatch(tag)
	if len(m) != 2 {
		return 0
	}
	n, err := strconv.Atoi(strings.SplitN(m[1], ".", 2)[0])
	if err != nil {
		return 0
	}
	return n
}

// embeddedLogo resolves a template logo key to an official embedded brand
// mark. Labels can arrive as free text ("BCA Virtual Account"), so the brand is
// also tried as the first token. UI icons and anything unmapped fall through to
// the hand-drawn marks in LogoSVG.
func embeddedLogo(name string) (template.HTML, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	candidates := []string{key, logoAliases[key]}
	if fields := strings.Fields(key); len(fields) > 1 {
		candidates = append(candidates, fields[0], logoAliases[fields[0]])
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if h, ok := embeddedLogos[c]; ok {
			return h, true
		}
	}
	return "", false
}
