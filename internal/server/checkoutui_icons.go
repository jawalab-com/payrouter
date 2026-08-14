package server

import (
	"html/template"
	"strings"
)

// LogoSVG returns the authentic embedded inline SVG string for payment brands,
// merchant brand badges, and Lucide UI icons.
// Official brand marks come from the embedded files in checkoutui/assets/logos;
// the hand-drawn marks below remain as the fallback layer for anything not
// covered by a file (UI icons, the merchant badge, Indomaret, ...).
func LogoSVG(name string) template.HTML {
	if h, ok := embeddedLogo(name); ok {
		return h
	}
	switch strings.ToLower(strings.TrimSpace(name)) {

	// --- Merchant Store / PayRouter Brand Header ---
	case "store", "merchant", "brand_avatar":
		return template.HTML(`<svg viewBox="0 0 32 32" width="28" height="28" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Merchant">
			<rect width="32" height="32" rx="8" fill="#2563EB"/>
			<path d="M7 11L9 6H23L25 11" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
			<path d="M7 11C7 12.6569 8.34315 14 10 14C11.6569 14 13 12.6569 13 11C13 12.6569 14.3431 14 16 14C17.6569 14 19 12.6569 19 11C19 12.6569 20.3431 14 22 14C23.6569 14 25 12.6569 25 11" stroke="#fff" stroke-width="2" stroke-linecap="round"/>
			<path d="M8 14V24C8 25.1046 8.89543 26 10 26H22C23.1046 26 24 25.1046 24 24V14" stroke="#fff" stroke-width="2" stroke-linecap="round"/>
			<path d="M13 26V19H19V26" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
		</svg>`)

	case "payrouter":
		return template.HTML(`<svg viewBox="0 0 120 32" width="100" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="PayRouter">
			<rect width="32" height="32" rx="8" fill="#2563EB"/>
			<path d="M10 16L16 10L22 16L16 22Z" fill="#fff"/>
			<circle cx="16" cy="16" r="3" fill="#2563EB"/>
			<text x="38" y="22" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="800" font-size="16" fill="currentColor" letter-spacing="-0.5">PayRouter</text>
		</svg>`)

	// --- Indonesian Payment Channels (Official Brand Marks) ---

	case "qris", "id_qris":
		return template.HTML(`<svg viewBox="0 0 100 36" width="80" height="28" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="QRIS">
			<rect width="100" height="36" rx="6" fill="#EE3124"/>
			<g fill="#ffffff">
				<!-- QR Matrix Marks -->
				<rect x="8" y="8" width="9" height="9" rx="1"/>
				<rect x="10" y="10" width="5" height="5" fill="#EE3124"/>
				<rect x="11.5" y="11.5" width="2" height="2" fill="#ffffff"/>
				
				<rect x="20" y="8" width="9" height="9" rx="1"/>
				<rect x="22" y="10" width="5" height="5" fill="#EE3124"/>
				<rect x="23.5" y="11.5" width="2" height="2" fill="#ffffff"/>

				<rect x="8" y="19" width="9" height="9" rx="1"/>
				<rect x="10" y="21" width="5" height="5" fill="#EE3124"/>
				<rect x="11.5" y="22.5" width="2" height="2" fill="#ffffff"/>
				
				<rect x="20" y="19" width="4" height="4"/>
				<rect x="25" y="24" width="4" height="4"/>
				
				<!-- QRIS Typography -->
				<path d="M36 18c0-4.5 3-7.5 7.5-7.5s7.5 3 7.5 7.5c0 3.2-1.5 5.5-4 6.7l4 5.3h-4.2l-3.3-4.5h-2.5V28H36V18zm7.5-4c-2.3 0-3.8 1.6-3.8 4s1.5 4 3.8 4 3.8-1.6 3.8-4-1.5-4-3.8-4z"/>
				<path d="M54 10.5h7.2c3.5 0 5.8 1.8 5.8 4.7 0 2-1.2 3.5-3 4.2l3.8 8.6h-4.2l-3.2-7.5h-2.7V28H54V10.5zm7 7c1.4 0 2.4-.8 2.4-2s-1-2-2.4-2H58v4h3z"/>
				<path d="M70 10.5h3.8V28H70V10.5z"/>
				<path d="M78 24.5l2.4-2.8c1.5 1.5 3.3 2.5 5.3 2.5 1.7 0 2.7-.8 2.7-1.8 0-2.8-8.5-1.5-8.5-7.7 0-2.8 2.3-4.5 5.8-4.5 2.5 0 4.7.9 6.5 2.5l-2.2 3c-1.4-1.2-2.8-1.8-4.3-1.8-1.5 0-2.3.7-2.3 1.5 0 2.7 8.5 1.4 8.5 7.7 0 3-2.4 4.7-6.2 4.7-2.8 0-5.5-1.2-7.7-3.3z"/>
			</g>
		</svg>`)

	case "bca", "bank_bca":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BCA">
			<rect width="100" height="36" rx="6" fill="#0060AF"/>
			<g fill="#ffffff">
				<!-- BCA Diamond Emblem -->
				<path d="M12 18L18 10L24 18L18 26L12 18Z" fill="#fff" opacity="0.9"/>
				<path d="M15 18L18 14L21 18L18 22L15 18Z" fill="#0060AF"/>
				<!-- BCA Typography -->
				<path d="M34 11h9c3.3 0 5.5 1.6 5.5 4.3 0 1.6-.9 2.9-2.3 3.5 1.9.6 3.1 2.1 3.1 4 0 3-2.4 4.7-6 4.7H34V11zm8.2 6.5c1.4 0 2.3-.7 2.3-1.8 0-1.2-.9-1.8-2.3-1.8H37.8v3.6h4.4zm.6 7.2c1.6 0 2.6-.8 2.6-2 0-1.3-1-2.1-2.6-2.1H37.8v4.1h5z"/>
				<path d="M64 12.8c-1.5-1.2-3.4-1.8-5.5-1.8-5 0-8.5 3.5-8.5 8.5s3.5 8.5 8.5 8.5c2.2 0 4.2-.7 5.7-2l-2-2.8c-1.1.9-2.3 1.4-3.7 1.4-3 0-4.9-2-4.9-5.1s1.9-5.1 4.9-5.1c1.3 0 2.5.5 3.5 1.3l2-2.9z"/>
				<path d="M72 27.5h-4l6.8-16.5h4.8l6.8 16.5h-4.2l-1.5-4h-7.2l-1.4 4zm6.8-12.8l-2.4 6h4.8l-2.4-6z"/>
			</g>
		</svg>`)

	case "mandiri", "bank_mandiri":
		return template.HTML(`<svg viewBox="0 0 110 36" width="80" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Mandiri">
			<rect width="110" height="36" rx="6" fill="#003D79"/>
			<!-- Mandiri Golden Ribbon -->
			<path d="M10 20C15 13 22 10 28 10C32 10 35 12 37 15C33 16 30 18 27 21C22 25 15 27 10 20Z" fill="#F5A800"/>
			<!-- Mandiri Clean Text -->
			<text x="68" y="24" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="16" fill="#ffffff" letter-spacing="-0.5">mandiri</text>
		</svg>`)

	case "bni", "bank_bni":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BNI">
			<rect width="100" height="36" rx="6" fill="#005E6A"/>
			<!-- BNI Orange Sun Ring -->
			<circle cx="20" cy="18" r="8" fill="#F15A24"/>
			<circle cx="20" cy="18" r="4.5" fill="#005E6A"/>
			<!-- BNI Bold Text -->
			<text x="58" y="25" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="20" fill="#ffffff" letter-spacing="1">BNI</text>
		</svg>`)

	case "bri", "bank_bri":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="BRI">
			<rect width="100" height="36" rx="6" fill="#00529C"/>
			<!-- BRI Dual Color Mark -->
			<path d="M12 11h8c3.5 0 5.5 1.5 5.5 4 0 1.6-1 2.8-2.5 3.3 2 .5 3.2 1.8 3.2 3.8 0 2.8-2.2 4.4-5.8 4.4H12V11z" fill="#F37024"/>
			<text x="60" y="25" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="20" fill="#ffffff" letter-spacing="1">BRI</text>
		</svg>`)

	case "permata", "bank_permata":
		return template.HTML(`<svg viewBox="0 0 110 36" width="80" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Permata">
			<rect width="110" height="36" rx="6" fill="#00828A"/>
			<!-- Permata Green Diamond Jewel -->
			<polygon points="18,9 26,18 18,27 10,18" fill="#78BE20"/>
			<polygon points="18,13 22,18 18,23 14,18" fill="#ffffff" opacity="0.4"/>
			<!-- Permata Text -->
			<text x="65" y="24" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="800" font-size="15" fill="#ffffff">Permata</text>
		</svg>`)

	case "gopay", "ewallet_gopay":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="GoPay">
			<rect width="100" height="36" rx="6" fill="#00AED6"/>
			<circle cx="20" cy="18" r="6.5" fill="#ffffff"/>
			<circle cx="20" cy="18" r="3.2" fill="#00AED6"/>
			<text x="56" y="24" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="16" fill="#ffffff">gopay</text>
		</svg>`)

	case "shopeepay", "ewallet_shopeepay":
		return template.HTML(`<svg viewBox="0 0 115 36" width="85" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="ShopeePay">
			<rect width="115" height="36" rx="6" fill="#EE4D2D"/>
			<!-- Shopee Bag -->
			<path d="M16 13c0-2.2 1.8-4 4-4s4 1.8 4 4v1h-8v-1z" stroke="#fff" stroke-width="1.8"/>
			<rect x="13" y="14" width="14" height="13" rx="2" fill="#fff"/>
			<path d="M20 17c-1.5 0-2.5.7-2.5 1.8 0 2 3.5 1.5 3.5 3.2 0 .8-.8 1.4-1.8 1.4-1.2 0-2-.5-2.5-1.2" stroke="#EE4D2D" stroke-width="1.6" stroke-linecap="round"/>
			<text x="68" y="23" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="800" font-size="14" fill="#ffffff">ShopeePay</text>
		</svg>`)

	case "dana", "ewallet_dana":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="DANA">
			<rect width="100" height="36" rx="6" fill="#118EEA"/>
			<text x="50" y="25" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="20" fill="#ffffff" letter-spacing="1">DANA</text>
		</svg>`)

	case "ovo", "ewallet_ovo":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="OVO">
			<rect width="100" height="36" rx="6" fill="#4C3494"/>
			<text x="50" y="25" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="20" fill="#ffffff" letter-spacing="2">OVO</text>
		</svg>`)

	case "alfamart", "id_retail", "retail_outlet":
		return template.HTML(`<svg viewBox="0 0 110 36" width="80" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Alfamart">
			<rect width="110" height="36" rx="6" fill="#ED1C24"/>
			<text x="55" y="24" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="16" fill="#ffffff">Alfamart</text>
		</svg>`)

	case "indomaret":
		return template.HTML(`<svg viewBox="0 0 110 36" width="80" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Indomaret">
			<rect width="110" height="36" rx="6" fill="#005BAC"/>
			<text x="55" y="24" text-anchor="middle" font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,sans-serif" font-weight="900" font-size="16" fill="#ffffff">Indomaret</text>
		</svg>`)

	case "card", "credit_card", "id_card":
		return template.HTML(`<svg viewBox="0 0 100 36" width="75" height="26" fill="none" xmlns="http://www.w3.org/2000/svg" aria-label="Card">
			<rect width="100" height="36" rx="6" fill="#1A1F36"/>
			<circle cx="42" cy="18" r="9" fill="#EB001B" opacity="0.9"/>
			<circle cx="58" cy="18" r="9" fill="#F79E1B" opacity="0.9"/>
		</svg>`)

	// --- Lucide Vector UI Icons (Stroke-width: 2, currentColor) ---
	case "timer":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><line x1="10" x2="14" y1="2" y2="2"/><line x1="12" x2="15" y1="14" y2="11"/><circle cx="12" cy="14" r="8"/></svg>`)

	case "alert", "warning", "triangle-alert":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/></svg>`)

	case "lock", "shield", "security":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><rect width="18" height="11" x="3" y="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>`)

	case "copy":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>`)

	case "check":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><polyline points="20 6 9 17 4 12"/></svg>`)

	case "arrow-left":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m12 19-7-7 7-7"/><path d="M19 12H5"/></svg>`)

	case "chevron", "chevron-right":
		return template.HTML(`<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="lucide-icon"><path d="m9 18 6-6-6-6"/></svg>`)

	default:
		return template.HTML(`<svg viewBox="0 0 36 36" width="26" height="26" fill="none" xmlns="http://www.w3.org/2000/svg">
			<rect width="36" height="36" rx="6" fill="#E3E6EA"/>
			<path d="M10 18h16M18 10v16" stroke="#666E7A" stroke-width="2.5" stroke-linecap="round"/>
		</svg>`)
	}
}
