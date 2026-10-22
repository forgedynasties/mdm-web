package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Limits above which an app (other than the role's own) is flagged.
const (
	muMemWarnMB  = 300.0
	muCPUWarnPct = 20.0
)

// muSplit is a sort metric as a big number and its unit: 1.42 / GB.
func muSplit(sortBy string, v float64) (string, string) {
	switch sortBy {
	case "screen":
		h := v / 3600000
		if h >= 1 {
			return fmt.Sprintf("%.1f", h), "h"
		}
		return fmt.Sprintf("%.0f", v/60000), "min"
	case "net":
		switch {
		case v >= 1<<30:
			return fmt.Sprintf("%.2f", v/(1<<30)), "GB"
		case v >= 1<<20:
			return fmt.Sprintf("%.0f", v/(1<<20)), "MB"
		}
		return fmt.Sprintf("%.0f", v/1024), "KB"
	case "cpu":
		return fmt.Sprintf("%.1f", v), "%"
	case "battery":
		return fmt.Sprintf("%.0f", v), "mAh"
	}
	if v >= 1024 {
		return fmt.Sprintf("%.2f", v/1024), "GB"
	}
	return fmt.Sprintf("%.0f", v), "MB"
}

func muFmt(sortBy string, v float64) string {
	n, u := muSplit(sortBy, v)
	if u == "%" {
		return n + " %"
	}
	return n + " " + u
}

func muDeviceValue(sortBy string, u db.DeviceAppUsage) (float64, bool) {
	switch sortBy {
	case "screen":
		return float64(u.FgMs), true
	case "net":
		return float64(u.RxBytes + u.TxBytes), true
	case "battery":
		return u.PowerMAh, true
	case "cpu":
		if u.CPUAvg != nil {
			return *u.CPUAvg, true
		}
		return 0, false
	}
	if u.PssAvgMB != nil {
		return *u.PssAvgMB, true
	}
	return 0, false
}

// roleAppBase is the base package of the model's role app ("" when none).
func (h *Handler) roleAppBase(ctx context.Context, model string) string {
	role := ""
	if p, ok := product.Resolve(model); ok && p.Class != "" {
		role = p.Class
	}
	if role == "" {
		role, _ = h.db.ProductRole(ctx, model)
	}
	if role == "" {
		return ""
	}
	return h.db.RoleAppBase(ctx, role)
}

func isRoleApp(pkg, base string) bool {
	return base != "" && (pkg == base || strings.HasPrefix(pkg, base+"."))
}

// hostAppExits sums the role app's process exits in the last 24 h, by reason.
func hostAppExits(extra json.RawMessage) (int, map[string]int) {
	var m struct {
		HostApp struct {
			Variants []struct {
				Exits map[string]int `json:"exits_24h"`
			} `json:"variants"`
		} `json:"host_app"`
	}
	why := map[string]int{}
	if json.Unmarshal(extra, &m) != nil {
		return 0, why
	}
	n := 0
	for _, v := range m.HostApp.Variants {
		for k, c := range v.Exits {
			n += c
			why[k] += c
		}
	}
	return n, why
}

func exitReason(k string) string {
	switch strings.ToLower(k) {
	case "low_memory", "lmk":
		return " (low memory)"
	case "crash", "crash_native", "native_crash":
		return " (crashed)"
	case "anr":
		return " (not responding)"
	case "excessive_resources":
		return " (used too much)"
	case "init_failure":
		return " (failed to start)"
	case "":
		return ""
	}
	return " (" + strings.ReplaceAll(strings.ToLower(k), "_", " ") + ")"
}
