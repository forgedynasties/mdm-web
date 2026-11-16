package dashboard

import (
	"net/url"
	"strings"
)

// The header's server label (E1, picked 16 Nov 2026; user: "mdm dev should have dev label in
// the aio mdm header"): a coral pill after the wordmark and a prefix on the tab title, so
// mdm.dev and mdm.aioapp.com, which otherwise look the same, can't be mistaken for each
// other while devices move between them. Decided by the server's own address (the first
// PUBLIC_ORIGIN): prod and anything unknown show none.
var envLabels = map[string]string{
	"mdm.dev.aioapp.com":       "DEV",
	"mdm-stage.dev.aioapp.com": "STAGE",
}

func envLabelFor(publicOrigin string) string {
	first := strings.TrimSpace(strings.Split(publicOrigin, ",")[0])
	u, err := url.Parse(first)
	if err != nil {
		return ""
	}
	return envLabels[strings.ToLower(u.Hostname())]
}
