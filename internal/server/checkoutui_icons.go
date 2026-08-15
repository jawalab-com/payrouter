package server

import (
	"html/template"
	"strings"
)

// LogoSVG returns the embedded inline SVG string for payment brands,
// bank badges, merchant avatars, and Lucide vector UI icons.
// All SVGs are self-contained vector assets with zero external CDN dependencies.
func LogoSVG(name string) template.HTML {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.TrimSuffix(key, " virtual account")
	key = strings.TrimSuffix(key, " va")
	key = strings.TrimPrefix(key, "bank ")
	key = strings.TrimPrefix(key, "bank_")

	switch key {

	// --- Lucide Vector UI Icons (Stroke-width: 2, currentColor) ---
	case "clock", "timer":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>`)

	case "chevron-left", "chevron_left":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m15 18-6-6 6-6"/></svg>`)

	case "chevron-right", "chevron_right", "chevron":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m9 18 6-6-6-6"/></svg>`)

	case "chevron-down", "chevron_down":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m6 9 6 6 6-6"/></svg>`)

	case "chevrons-up-down", "chevrons_up_down":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m7 15 5 5 5-5"/><path d="m7 9 5-5 5 5"/></svg>`)

	case "x", "close":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>`)

	case "alert-circle", "alert_circle", "alert", "warning", "triangle-alert":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="8" y2="12"/><line x1="12" x2="12.01" y1="16" y2="16"/></svg>`)

	case "download":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" x2="12" y1="15" y2="3"/></svg>`)

	case "check-circle-2", "check_circle_2", "check-circle":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/></svg>`)

	case "info":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/></svg>`)

	case "copy":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>`)

	case "check":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><polyline points="20 6 9 17 4 12"/></svg>`)

	case "building-2", "building_2", "building":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="M6 22V4a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v18Z"/><path d="M6 12H4a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2h2"/><path d="M18 9h2a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2h-2"/><path d="M10 6h4"/><path d="M10 10h4"/><path d="M10 14h4"/><path d="M10 18h4"/></svg>`)

	case "search":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>`)

	case "credit-card", "credit_card", "card", "id_card":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><rect width="20" height="14" x="2" y="5" rx="2"/><line x1="2" x2="22" y1="10" y2="10"/></svg>`)

	case "shield-check", "shield_check", "shield", "security":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="m9 12 2 2 4-4"/></svg>`)

	case "lock":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>`)

	case "smartphone", "phone":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><rect width="14" height="20" x="5" y="2" rx="2" ry="2"/><path d="M12 18h.01"/></svg>`)

	case "wallet":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="M19 7V4a1 1 0 0 0-1-1H5a2 2 0 0 0 0 4h15a1 1 0 0 1 1 1v4h-3a2 2 0 0 0 0 4h3a1 1 0 0 0 1-1v-2a1 1 0 0 0-1-1"/><path d="M3 5v14a2 2 0 0 0 2 2h15a1 1 0 0 0 1-1v-4"/></svg>`)

	case "globe":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><circle cx="12" cy="12" r="10"/><path d="M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20"/><path d="M2 12h20"/></svg>`)

	case "merchant", "store":
		return template.HTML(`<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m2 7 4.41-4.41A2 2 0 0 1 7.83 2h8.34a2 2 0 0 1 1.42.59L22 7"/><path d="M4 12v8a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-8"/><path d="M15 22v-4a2 2 0 0 0-2-2h-2a2 2 0 0 0-2 2v4"/><path d="M2 7h20"/><path d="M22 7v3a2 2 0 0 1-2 2v0a2.7 2.7 0 0 1-1.59-.63.7.7 0 0 0-.82 0A2.7 2.7 0 0 1 16 12a2.7 2.7 0 0 1-1.59-.63.7.7 0 0 0-.82 0A2.7 2.7 0 0 1 12 12a2.7 2.7 0 0 1-1.59-.63.7.7 0 0 0-.82 0A2.7 2.7 0 0 1 8 12a2.7 2.7 0 0 1-1.59-.63.7.7 0 0 0-.82 0A2.7 2.7 0 0 1 4 12v0a2 2 0 0 1-2-2V7"/></svg>`)

	// --- GPN (Gerbang Pembayaran Nasional) Logo ---
	case "gpn":
		return template.HTML(`<svg viewBox="0 0 64 24" width="44" height="18" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="GPN">
			<path d="M12 3L2 12L12 21L22 12L12 3Z" fill="#ED1C24"/>
			<path d="M12 7L6 12L12 17L18 12L12 7Z" fill="#fff"/>
			<text x="26" y="16" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="11" fill="#003D79" letter-spacing="0.5">GPN</text>
		</svg>`)

	// --- QRIS Official Vector Mark ---
	case "qris", "id_qris":
		return template.HTML(`<svg viewBox="0 0 72 24" width="52" height="18" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="QRIS">
			<rect width="72" height="24" rx="4" fill="#EE3124"/>
			<g fill="#ffffff">
				<rect x="6" y="5" width="5" height="5" rx="0.5"/>
				<rect x="7" y="6" width="3" height="3" fill="#EE3124"/>
				<rect x="8" y="7" width="1" height="1" fill="#ffffff"/>
				
				<rect x="13" y="5" width="5" height="5" rx="0.5"/>
				<rect x="14" y="6" width="3" height="3" fill="#EE3124"/>
				<rect x="15" y="7" width="1" height="1" fill="#ffffff"/>

				<rect x="6" y="14" width="5" height="5" rx="0.5"/>
				<rect x="7" y="15" width="3" height="3" fill="#EE3124"/>
				<rect x="8" y="16" width="1" height="1" fill="#ffffff"/>
				
				<text x="22" y="17" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="12" fill="#ffffff" letter-spacing="1">QRIS</text>
			</g>
		</svg>`)

	// --- Card Networks ---
	case "visa":
		return template.HTML(`<svg viewBox="0 0 40 24" width="36" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="VISA">
			<text x="20" y="17" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#1A1F71" font-style="italic" letter-spacing="0.5">VISA</text>
		</svg>`)

	case "mastercard", "mc":
		return template.HTML(`<svg viewBox="0 0 40 24" width="36" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Mastercard">
			<circle cx="15" cy="12" r="8" fill="#EB001B"/>
			<circle cx="25" cy="12" r="8" fill="#F79E1B" opacity="0.88"/>
		</svg>`)

	case "jcb":
		return template.HTML(`<svg viewBox="0 0 40 24" width="36" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="JCB">
			<rect x="4" y="3" width="10" height="18" rx="2" fill="#0066B2"/>
			<rect x="15" y="3" width="10" height="18" rx="2" fill="#EE1C25"/>
			<rect x="26" y="3" width="10" height="18" rx="2" fill="#009944"/>
			<text x="9" y="16" font-family="sans-serif" font-weight="900" font-size="9" fill="#fff" text-anchor="middle">J</text>
			<text x="20" y="16" font-family="sans-serif" font-weight="900" font-size="9" fill="#fff" text-anchor="middle">C</text>
			<text x="31" y="16" font-family="sans-serif" font-weight="900" font-size="9" fill="#fff" text-anchor="middle">B</text>
		</svg>`)

	case "amex", "american_express":
		return template.HTML(`<svg viewBox="0 0 40 24" width="36" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="AMEX">
			<rect width="40" height="24" rx="2" fill="#006FCF"/>
			<text x="20" y="16" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="9" fill="#ffffff" letter-spacing="0.5">AMEX</text>
		</svg>`)

	// --- Authentic Indonesian Bank Vector Logos ---
	case "bca":
		return template.HTML(`<svg viewBox="0 0 50 24" width="38" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BCA">
			<path d="M9 4L4 12L9 20L14 12L9 4Z" fill="#00529C"/>
			<text x="20" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#00529C" letter-spacing="0.5">BCA</text>
		</svg>`)

	case "mandiri":
		return template.HTML(`<svg viewBox="0 0 65 24" width="46" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Mandiri">
			<text x="2" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="11" fill="#002D62">mandırı</text>
			<path d="M48 6C54 6 58 10 63 15C59 13 54 11 48 11C44 11 40 12 37 13C41 9 44 6 48 6Z" fill="#F5A800"/>
		</svg>`)

	case "bni":
		return template.HTML(`<svg viewBox="0 0 52 24" width="40" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BNI">
			<text x="2" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#005E6A" letter-spacing="0.5">BNI</text>
			<circle cx="43" cy="12" r="5" fill="#F15A24"/>
		</svg>`)

	case "bri":
		return template.HTML(`<svg viewBox="0 0 50 24" width="38" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BRI">
			<text x="2" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#00529C" letter-spacing="0.5">BRI</text>
			<rect x="36" y="5" width="10" height="14" rx="2" fill="#F37024"/>
		</svg>`)

	case "permata":
		return template.HTML(`<svg viewBox="0 0 70 24" width="48" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Permata">
			<polygon points="10,4 16,12 10,20 4,12" fill="#78BE20"/>
			<polygon points="10,4 16,12 10,12" fill="#00857C"/>
			<text x="22" y="16" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="800" font-size="10" fill="#00857C">Permata</text>
		</svg>`)

	case "cimb", "cimb_niaga":
		return template.HTML(`<svg viewBox="0 0 54 24" width="42" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="CIMB">
			<rect x="2" y="4" width="8" height="16" fill="#ED1C24"/>
			<text x="14" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="12" fill="#780116">CIMB</text>
		</svg>`)

	case "bsi":
		return template.HTML(`<svg viewBox="0 0 50 24" width="38" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BSI">
			<circle cx="8" cy="12" r="5" fill="#00A39D"/>
			<text x="16" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="12" fill="#00A39D">BSI</text>
		</svg>`)

	case "btn":
		return template.HTML(`<svg viewBox="0 0 50 24" width="38" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BTN">
			<text x="2" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#002D62">BTN</text>
			<rect x="36" y="6" width="8" height="12" fill="#ED1C24"/>
		</svg>`)

	case "danamon":
		return template.HTML(`<svg viewBox="0 0 65 24" width="46" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Danamon">
			<rect x="2" y="5" width="8" height="14" rx="1" fill="#FF7900"/>
			<text x="14" y="16" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="800" font-size="10" fill="#003A70">Danamon</text>
		</svg>`)

	case "maybank":
		return template.HTML(`<svg viewBox="0 0 65 24" width="46" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Maybank">
			<circle cx="8" cy="12" r="5" fill="#FFC800"/>
			<text x="16" y="16" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="10" fill="#000000">Maybank</text>
		</svg>`)

	case "ocbc":
		return template.HTML(`<svg viewBox="0 0 54 24" width="42" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="OCBC">
			<circle cx="8" cy="12" r="5" fill="#ED1C24"/>
			<text x="16" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="12" fill="#ED1C24">OCBC</text>
		</svg>`)

	case "mega":
		return template.HTML(`<svg viewBox="0 0 54 24" width="42" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="MEGA">
			<text x="4" y="17" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="12" fill="#F37024">MEGA</text>
		</svg>`)

	// --- E-Wallets Vector Marks ---
	case "gopay", "ewallet_gopay":
		return template.HTML(`<svg viewBox="0 0 56 24" width="44" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="GoPay">
			<circle cx="8" cy="12" r="5" fill="#00AED6"/>
			<circle cx="8" cy="12" r="2.5" fill="#fff"/>
			<text x="16" y="16" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="800" font-size="10" fill="#00AED6">gopay</text>
		</svg>`)

	case "ovo", "ewallet_ovo":
		return template.HTML(`<svg viewBox="0 0 46 24" width="38" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="OVO">
			<text x="23" y="17" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#4C2A86" letter-spacing="1">OVO</text>
		</svg>`)

	case "shopee", "shopeepay", "ewallet_shopeepay":
		return template.HTML(`<svg viewBox="0 0 70 24" width="50" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="ShopeePay">
			<path d="M8 6C6 6 4 8 4 10V18C4 19 5 20 6 20H14C15 20 16 19 16 18V10C16 8 14 6 12 6H8ZM9 4C9 3.5 9.5 3 10 3C10.5 3 11 3.5 11 4V6H9V4Z" fill="#EE4D2D"/>
			<text x="19" y="16" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="800" font-size="9" fill="#EE4D2D">ShopeePay</text>
		</svg>`)

	case "dana", "ewallet_dana":
		return template.HTML(`<svg viewBox="0 0 52 24" width="40" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="DANA">
			<text x="26" y="17" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="13" fill="#118EEA" letter-spacing="0.5">DANA</text>
		</svg>`)

	case "linkaja", "ewallet_linkaja":
		return template.HTML(`<svg viewBox="0 0 60 24" width="46" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="LinkAja">
			<rect x="2" y="5" width="14" height="14" rx="3" fill="#ED1C24"/>
			<text x="20" y="16" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="800" font-size="9" fill="#ED1C24">LinkAja!</text>
		</svg>`)

	// --- Retail Outlets ---
	case "alfamart", "id_retail", "retail_outlet":
		return template.HTML(`<svg viewBox="0 0 65 24" width="48" height="20" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Alfamart">
			<rect width="65" height="24" rx="3" fill="#ED1C24"/>
			<text x="32" y="16" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,sans-serif" font-weight="900" font-size="10" fill="#ffffff">Alfamart</text>
		</svg>`)

	case "indomaret":
		return template.HTML(`<img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==" alt="Indomaret">`)

	default:
		return template.HTML(`<span style="font-size:0.625rem;font-weight:800;color:var(--fg)">` + template.HTMLEscapeString(strings.ToUpper(name)) + `</span>`)
	}
}
