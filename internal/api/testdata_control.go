package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// Device control for the testing team (QA key, 30 Oct 2026; user: "add one control, so that
// they can trigger turning on a specific app and then they can trigger closing it and
// turning on off screen"; "need to allow role app only for the app launching"). Each route sends one fixed shell command built here, never a
// caller's text, to a Lab firmware device (qaFirmwareDevice), and the caller polls
// GET /api/v1/testdata/commands/{id} — or passes ?wait=true to get the result in the reply.
//
// Open and close take only the device's role app (Lineup › role › App) or one of its
// channels — aio.app.nugget, aio.app.nugget.uatv2, … — never any other package.
//
//	open   monkey -p <pkg> -c android.intent.category.LAUNCHER 1   (the app's launcher activity)
//	close  am force-stop <pkg>
//	screen input keyevent KEYCODE_WAKEUP | KEYCODE_SLEEP

// qaCommandBy marks the commands these routes create, so the status route answers for
// those and no other command.
const qaCommandBy = "QA API"

var qaPackageRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// qaControlWait is how long ?wait=true holds the request for the device's answer.
const qaControlWait = 20 * time.Second

func (h *Handler) TestDataAppOpen(w http.ResponseWriter, r *http.Request) {
	pkg, ok := qaPackageBody(w, r)
	if !ok {
		return
	}
	// pm path first: monkey on a missing package says "No activities found" with exit 0
	// on some builds, so the command would read completed for an app that never opened.
	h.qaControl(w, r, "app_open", pkg, map[string]string{"package": pkg},
		"pm path "+pkg+" >/dev/null 2>&1 || { echo 'not installed: "+pkg+"'; exit 1; }; "+
			"monkey -p "+pkg+" -c android.intent.category.LAUNCHER 1 2>&1 | grep -q 'Events injected: 1' "+
			"&& echo 'opened "+pkg+"' || { echo 'could not open "+pkg+"'; exit 1; }")
}

func (h *Handler) TestDataAppClose(w http.ResponseWriter, r *http.Request) {
	pkg, ok := qaPackageBody(w, r)
	if !ok {
		return
	}
	h.qaControl(w, r, "app_close", pkg, map[string]string{"package": pkg},
		"pm path "+pkg+" >/dev/null 2>&1 || { echo 'not installed: "+pkg+"'; exit 1; }; "+
			"am force-stop "+pkg+" && echo 'closed "+pkg+"'")
}

func (h *Handler) TestDataScreen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	key := map[string]string{"on": "KEYCODE_WAKEUP", "off": "KEYCODE_SLEEP"}[strings.ToLower(body.State)]
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `state must be "on" or "off"`})
		return
	}
	state := strings.ToLower(body.State)
	h.qaControl(w, r, "screen_"+state, "", map[string]string{"state": state},
		"input keyevent "+key+" && echo 'screen "+state+"'")
}

// qaRoleAppMatch: pkg is the role app's base package or one of its channels (base.*).
func qaRoleAppMatch(base, pkg string) bool {
	return pkg == base || strings.HasPrefix(pkg, base+".")
}

func qaPackageBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Package string `json:"package"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	pkg := strings.TrimSpace(body.Package)
	if !qaPackageRe.MatchString(pkg) || len(pkg) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `package must be an Android package name, e.g. "aio.app.nugget"`})
		return "", false
	}
	return pkg, true
}

// qaControl sends cmd as a shell command to the {serial} device and answers with the
// command's id (202), or its result with ?wait=true. pkg, when set, must be the device's
// role app or one of its channels. An offline device is refused (409) rather than
// queued: a screen-off that lands hours later would spoil someone's test.
func (h *Handler) qaControl(w http.ResponseWriter, r *http.Request, action, pkg string, args map[string]string, cmd string) {
	dev, ok := h.qaFirmwareDevice(w, r)
	if !ok {
		return
	}
	if pkg != "" {
		base := h.db.RoleAppBase(r.Context(), dev.Class())
		if base == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this device's role has no app on Lineup"})
			return
		}
		if !qaRoleAppMatch(base, pkg) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the device's role app can be opened or closed", "role_app": base})
			return
		}
	}
	if !dev.Supports("shell") {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this device's MDM client can't run device controls"})
		return
	}
	recent := time.Since(dev.LastSeenAt) < 3*time.Duration(h.cfg.CheckinInterval())*time.Second
	if !h.hub.IsConnected(dev.ID) && !recent {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "device is offline", "last_seen_at": dev.LastSeenAt.UTC()})
		return
	}
	payload, _ := json.Marshal(map[string]string{"cmd": cmd})
	ids := []uuid.UUID{dev.ID}
	c, err := h.db.CreateCommandBy(r.Context(), "shell", "", payload, "devices", ids, qaCommandBy)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.pushCommand(r.Context(), c, "devices", ids)
	detail := action
	for k, v := range args {
		detail += " " + k + "=" + v
	}
	_ = h.db.InsertAuditDevice(r.Context(), "qa-api", "testdata.control", dev.SerialNumber, detail, dev.ID)

	if r.URL.Query().Get("wait") == "true" {
		deadline := time.Now().Add(qaControlWait)
		for time.Now().Before(deadline) {
			res, err := h.qaCommandResult(r, c)
			if err == nil && res["done"] == true {
				writeJSON(w, http.StatusOK, res)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	res, err := h.qaCommandResult(r, c)
	if err != nil {
		res = map[string]any{"command_id": c.ID, "status": "queued", "done": false}
	}
	res["action"] = action
	for k, v := range args {
		res[k] = v
	}
	writeJSON(w, http.StatusAccepted, res)
}

// TestDataCommand is the status of a command a control route created:
// GET /api/v1/testdata/commands/{id}.
func (h *Handler) TestDataCommand(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "command not found"})
		return
	}
	c, err := h.db.GetCommand(r.Context(), id)
	if err != nil || c.CreatedBy != qaCommandBy {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "command not found"})
		return
	}
	res, err := h.qaCommandResult(r, c)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// qaCommandResult is one control command as the testing team sees it: status is
// queued → sent → running → completed | failed | expired; done once it can't change.
func (h *Handler) qaCommandResult(r *http.Request, c *db.Command) (map[string]any, error) {
	ds, err := h.db.GetCommandDeliveries(r.Context(), c.ID, h.cfg.CommandExpiry())
	if err != nil {
		return nil, err
	}
	res := map[string]any{"command_id": c.ID, "created_at": c.CreatedAt.UTC(), "status": "queued", "done": false, "output": ""}
	if len(ds) > 0 {
		d := ds[0]
		status := d.Status
		switch status {
		case "pending", "":
			status = "queued"
		case "delivered":
			status = "sent"
		}
		res["serial"] = d.SerialNumber
		res["status"] = status
		res["output"] = strings.TrimSpace(d.Output)
		res["updated_at"] = d.UpdatedAt.UTC()
		res["done"] = status == "completed" || status == "failed" || status == "expired"
	}
	return res, nil
}
