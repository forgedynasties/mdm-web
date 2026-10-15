package db

import "strings"

// systemActors are the names the server itself writes into attribution columns (who
// sent a command, who did an audited action) for work no person did. They never had an
// account, so they are not "former usernames" to merge, and they read as what they are.
var systemActors = map[string]string{
	"auto-update":   "Auto-update",
	"control probe": "Control probe",
	"api":           "Admin API",
	"api key":       "Admin API",
	"claude-code":   "Claude Code",
	"system":        "System",
	"compliance":    "Compliance auto-fix",
}

// SystemActorLabel is how a system actor reads ("Auto-update", "Stage server"), and
// false for a name that is a person's.
func SystemActorLabel(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if l, ok := systemActors[n]; ok {
		return l, true
	}
	// Another MDM server acting on a device it handed over (custody moves).
	if strings.HasPrefix(n, "peer:") {
		p := strings.TrimSpace(name[len("peer:"):])
		if p == "" {
			return "Peer server", true
		}
		return strings.ToUpper(p[:1]) + p[1:] + " server", true
	}
	return "", false
}
