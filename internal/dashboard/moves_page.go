package dashboard

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"mdm/internal/db"
)

// The Moves page (/moves, MV1 "Fleet's twin", picked 19 Nov 2026): read-only progress of the
// move of devices from this server to prod. It reads what GET /api/v1/admin/move/status does
// (db.ListDeviceMoves) and exists only where moves do (movesOn), for the super admin. The
// page draws itself from one JSON document (movesView), which /moves/data serves again for
// the 30 s refresh.

// movesOn reports whether this server moves devices: the same rule as the api package's
// deviceMoveTarget (mdm.dev always; elsewhere ALLOW_DEVICE_MOVE=1 with MOVE_TARGET_URL).
func movesOn() bool {
	first := strings.TrimSpace(strings.Split(os.Getenv("PUBLIC_ORIGIN"), ",")[0])
	if u, err := url.Parse(first); err == nil && strings.EqualFold(u.Hostname(), "mdm.dev.aioapp.com") {
		return true
	}
	return os.Getenv("ALLOW_DEVICE_MOVE") == "1" && strings.TrimSpace(os.Getenv("MOVE_TARGET_URL")) != ""
}

// moveOnlineWithin is how recently a device must have been heard to count as online (the
// move job's own rule).
const moveOnlineWithin = 10 * time.Minute

// moveDarkAfter is how long a pending device may stay unheard before the page calls it dark.
const moveDarkAfter = 7 * 24 * time.Hour

type moveStep struct {
	Title string `json:"title"`
	Sub   string `json:"sub,omitempty"`
	State string `json:"state"` // done | now | bad | todo
}

type moveRow struct {
	Serial       string     `json:"serial"`
	Kind         string     `json:"kind"` // std | fw
	Venue        string     `json:"venue"`
	VenueKey     string     `json:"venue_key"` // venue id, or "toplace"
	VenueHidden  bool       `json:"venue_hidden,omitempty"`
	DeviceHidden bool       `json:"device_hidden,omitempty"`
	State        string     `json:"state"`  // the move's state
	Bucket       string     `json:"bucket"` // pending | waiting_client | sent | arrived | came_back | failed | canceled
	Note         string     `json:"note"`
	Next         string     `json:"next"`
	Attempts     int        `json:"attempts"`
	CreatedBy    string     `json:"created_by"`
	Have         string     `json:"have"`
	Want         string     `json:"want"`
	Standing     string     `json:"standing"` // current | behind | unknown | tooold
	LastSeen     *time.Time `json:"last_seen,omitempty"`
	Online       bool       `json:"online"`
	DarkDays     int        `json:"dark_days,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Steps        []moveStep `json:"steps"`
}

type movePlace struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Total   int    `json:"total"`
	Arrived int    `json:"arrived"`
	Hidden  bool   `json:"hidden,omitempty"`
	ToPlace bool   `json:"to_place,omitempty"`
}

type moveAttention struct {
	Serial string `json:"serial"`
	Kind   string `json:"kind"` // came_back | failed | tooold | dark
	Title  string `json:"title"`
	Text   string `json:"text"`
}

type movesView struct {
	Target     string          `json:"target"`
	Now        time.Time       `json:"now"`
	Total      int             `json:"total"`
	Arrived    int             `json:"arrived"`
	OnlinePend int             `json:"online_pending"`
	Counts     map[string]int  `json:"counts"`
	Places     []movePlace     `json:"places"`
	Attention  []moveAttention `json:"attention"`
	Moves      []moveRow       `json:"moves"`
}

func fmtMoveTime(t time.Time) string { return t.UTC().Format("2 Jan 15:04") + " UTC" }

// moveStanding words a Standard or firmware client against the hosted build.
func moveStanding(st db.ClientStanding, note string) string {
	switch {
	case strings.Contains(note, "too old"):
		return "tooold"
	case st.State == "current":
		return "current"
	case st.State == "behind":
		return "behind"
	}
	return "unknown"
}

// buildMovesView groups the moves for the page: bucket and next step per row, the timeline,
// the rail's state and place counts, and what needs a person. standing names a move's client
// against the hosted build (nil: unknown). Pure, so it is tested without a database.
func buildMovesView(moves []db.DeviceMove, standing func(db.DeviceMove) db.ClientStanding, now time.Time) movesView {
	v := movesView{Now: now, Counts: map[string]int{}, Places: []movePlace{}, Attention: []moveAttention{}, Moves: []moveRow{}}
	places := map[string]*movePlace{}
	for _, m := range moves {
		r := moveRow{Serial: m.Serial, Kind: "std", Venue: m.Venue, VenueKey: "toplace", VenueHidden: m.VenueHidden,
			DeviceHidden: m.DeviceHidden, State: m.State, Note: m.Note, Attempts: m.Attempts, CreatedBy: m.CreatedBy,
			Have: m.ClientVersion, LastSeen: m.LastSeenAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
		if m.Kind == "firmware" {
			r.Kind = "fw"
		}
		if m.VenueID != nil {
			r.VenueKey = m.VenueID.String()
		} else {
			r.Venue = ""
		}
		r.Online = m.LastSeenAt != nil && now.Sub(*m.LastSeenAt) <= moveOnlineWithin
		var st db.ClientStanding
		if standing != nil {
			st = standing(m)
		}
		if st.Have != "" {
			r.Have = st.Have
		}
		r.Want = st.Want
		r.Standing = moveStanding(st, m.Note)
		tooOld := strings.Contains(m.Note, "too old")
		waitClient := m.State == db.MovePending && strings.Contains(m.Note, "waiting for MDM client")

		r.Bucket = m.State
		if waitClient {
			r.Bucket = "waiting_client"
		}
		if m.State == db.MovePending && m.LastSeenAt != nil && now.Sub(*m.LastSeenAt) > moveDarkAfter {
			r.DarkDays = int(now.Sub(*m.LastSeenAt) / (24 * time.Hour))
		} else if m.State == db.MovePending && m.LastSeenAt == nil {
			r.DarkDays = 9999
		}

		// What happens next.
		switch m.State {
		case db.MovePending:
			switch {
			case m.Note != "":
				r.Next = m.Note
			case !r.Online:
				r.Next = "waiting for it to come online"
			default:
				r.Next = "ready: goes out on the next minute"
			}
		case db.MoveSent:
			r.Next = "sent, waiting for it to check in on prod"
		case db.MoveArrived:
			r.Next = "on prod"
			if m.ArrivedAt != nil {
				r.Next = "on prod since " + fmtMoveTime(*m.ArrivedAt)
			}
		case db.MoveCameBack, db.MoveFailed, db.MoveCanceled:
			r.Next = m.Note
			if r.Next == "" {
				r.Next = strings.ReplaceAll(m.State, "_", " ")
			}
		}
		r.Steps = moveSteps(m, r, tooOld, waitClient)

		// Counts and places.
		v.Total++
		v.Counts[r.Bucket]++
		if m.State == db.MoveArrived {
			v.Arrived++
		}
		if m.State == db.MovePending && r.Online {
			v.OnlinePend++
		}
		p := places[r.VenueKey]
		if p == nil {
			name := m.Venue
			if r.VenueKey == "toplace" {
				name = "To place"
			}
			p = &movePlace{Key: r.VenueKey, Name: name, Hidden: m.VenueHidden, ToPlace: r.VenueKey == "toplace"}
			places[r.VenueKey] = p
		}
		p.Total++
		if m.State == db.MoveArrived {
			p.Arrived++
		}

		// Needs attention.
		switch {
		case m.State == db.MoveCameBack:
			v.Attention = append(v.Attention, moveAttention{m.Serial, "came_back", "Came back", m.Note})
		case m.State == db.MoveFailed:
			v.Attention = append(v.Attention, moveAttention{m.Serial, "failed", "Failed", m.Note})
		case m.State == db.MovePending && tooOld:
			v.Attention = append(v.Attention, moveAttention{m.Serial, "tooold", "On a client too old to update", m.Note})
		case r.DarkDays > 0:
			t := "Dark for " + moveItoa(r.DarkDays) + " days"
			if r.DarkDays == 9999 {
				t = "Never seen"
			}
			v.Attention = append(v.Attention, moveAttention{m.Serial, "dark", t, "pending, not heard from"})
		}
		v.Moves = append(v.Moves, r)
	}
	v.Counts["attention"] = len(v.Attention)
	for _, p := range places {
		v.Places = append(v.Places, *p)
	}
	// To place first, then the venues with the most left to move.
	sort.Slice(v.Places, func(i, j int) bool {
		a, b := v.Places[i], v.Places[j]
		if a.ToPlace != b.ToPlace {
			return a.ToPlace
		}
		if a.Total-a.Arrived != b.Total-b.Arrived {
			return a.Total-a.Arrived > b.Total-b.Arrived
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	// Attention order: came back / failed first, then too old, then dark.
	rank := map[string]int{"came_back": 0, "failed": 1, "tooold": 2, "dark": 3}
	sort.SliceStable(v.Attention, func(i, j int) bool { return rank[v.Attention[i].Kind] < rank[v.Attention[j].Kind] })
	return v
}

func moveItoa(n int) string { return strconv.Itoa(n) }

// moveSteps is the timeline the detail pane draws, from what the move row has.
func moveSteps(m db.DeviceMove, r moveRow, tooOld, waitClient bool) []moveStep {
	steps := []moveStep{{Title: "Filed", Sub: fmtMoveTime(m.CreatedAt) + " · by " + moveDash(m.CreatedBy), State: "done"}}
	if m.ImportedAt != nil {
		steps = append(steps, moveStep{Title: "Imported on prod", Sub: fmtMoveTime(*m.ImportedAt) + " · row, config and key copied", State: "done"})
	} else {
		steps = append(steps, moveStep{Title: "Imported on prod", Sub: "copied just before it is sent", State: "todo"})
	}
	switch {
	case tooOld:
		steps = append(steps, moveStep{Title: "Client update", Sub: m.Note, State: "bad"})
	case waitClient:
		steps = append(steps, moveStep{Title: "Client update", Sub: m.Note, State: "now"})
	case strings.Contains(m.Note, "to close"):
		steps = append(steps, moveStep{Title: "Venue closed", Sub: m.Note, State: "now"})
	}
	switch {
	case m.SentAt != nil:
		steps = append(steps, moveStep{Title: "Sent", Sub: fmtMoveTime(*m.SentAt) + " · move command delivered", State: "done"})
	case m.State == db.MovePending && !tooOld && !waitClient && !r.Online:
		steps = append(steps, moveStep{Title: "Sent", Sub: "waiting for it to come online", State: "now"})
	default:
		steps = append(steps, moveStep{Title: "Sent", Sub: "move command delivered", State: "todo"})
	}
	switch m.State {
	case db.MoveArrived:
		sub := "checked in on prod"
		if m.ArrivedAt != nil {
			sub = fmtMoveTime(*m.ArrivedAt) + " · checked in on prod"
		}
		steps = append(steps, moveStep{Title: "Arrived on prod", Sub: sub, State: "done"})
	case db.MoveCameBack:
		steps = append(steps, moveStep{Title: "Came back", Sub: m.Note, State: "bad"})
	case db.MoveFailed:
		steps = append(steps, moveStep{Title: "Failed", Sub: m.Note, State: "bad"})
	case db.MoveCanceled:
		steps = append(steps, moveStep{Title: "Canceled", Sub: m.Note, State: "bad"})
	default:
		steps = append(steps, moveStep{Title: "Arrived on prod", Sub: "first check-in there", State: "todo"})
	}
	return steps
}

func moveDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// movesData loads the moves and builds the view.
func (h *Handler) movesData(r *http.Request) (movesView, error) {
	ctx := r.Context()
	moves, err := h.db.ListDeviceMoves(ctx, nil, nil)
	if err != nil {
		return movesView{}, err
	}
	serials := make([]string, 0, len(moves))
	for _, m := range moves {
		serials = append(serials, m.Serial)
	}
	devs, err := h.db.MoveClientDevices(ctx, serials)
	if err != nil {
		return movesView{}, err
	}
	standing := func(m db.DeviceMove) db.ClientStanding {
		d, ok := devs[m.Serial]
		if !ok {
			return db.ClientStanding{State: "unknown"}
		}
		return h.ClientStandingFor(ctx, &d)
	}
	v := buildMovesView(moves, standing, time.Now())
	v.Target = strings.TrimRight(os.Getenv("MOVE_TARGET_URL"), "/")
	if v.Target == "" {
		v.Target = "https://mdm.aioapp.com"
	}
	return v, nil
}

// MovesPage renders /moves.
func (h *Handler) MovesPage(w http.ResponseWriter, r *http.Request) {
	if !movesOn() {
		http.NotFound(w, r)
		return
	}
	v, err := h.movesData(r)
	if err != nil {
		http.Error(w, "could not read the moves", http.StatusInternalServerError)
		return
	}
	raw, _ := json.Marshal(v)
	// "</" inside a script block would end it early (a note is free text).
	safe := strings.ReplaceAll(string(raw), "</", `<\/`)
	h.render(w, r, "moves.html", map[string]any{
		"Title": "Moves",
		"V":     v,
		"JSON":  template.JS(safe),
	})
}

// MovesData is GET /moves/data: the same document, for the page's refresh.
func (h *Handler) MovesData(w http.ResponseWriter, r *http.Request) {
	if !movesOn() {
		http.NotFound(w, r)
		return
	}
	v, err := h.movesData(r)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not read the moves")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
