package dashboard

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

func TestOpsHomeRenders(t *testing.T) {
	now := time.Now()
	venue := uuid.New()
	rows := []placeRow{
		{Device: db.Device{ID: uuid.New(), SerialNumber: "DK19256F40259", AgentKind: "dpc", EnrolledAt: now}, By: "shahrukh.bashir", Online: true,
			Choices: []string{"kds", "pos"}},
		{Device: db.Device{ID: uuid.New(), SerialNumber: "DP02256HJ0202", AgentKind: "dpc", DeviceClass: "pos", EnrolledAt: now}, Choices: []string{"kds", "pos"}},
		{Device: db.Device{ID: uuid.New(), SerialNumber: "AT070AA2600027", AgentKind: "firmware", DeviceClass: "t7", EnrolledAt: now}},
	}
	venues := []db.Restaurant{{ID: venue, Name: "Ah Huevo - Pleasanton", DeviceCount: 4}}
	svc := []svcRow{{Device: db.Device{ID: uuid.New(), SerialNumber: "AK270AABU00180", RestaurantID: &venue, RestaurantName: "Ah Huevo"}}}
	tmpl := hubTmpl(t, "ops_home.html", "my_visits.html", "my_enroll.html", "enrollment.html", "restaurants.html", "layout.html", "_settings_cards.html")
	for _, view := range []string{"place", "service"} {
		data := map[string]any{"View": view, "Rows": rows, "SvcRows": svc, "ServiceN": 1, "LabN": 2,
			"Venues": venues, "RailVenues": venues, "Enrollments": []db.Enrollment{}, "Role": "fleet_manager"}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "ops_home.html", data); err != nil {
			t.Fatalf("ops_home.html %s: %v", view, err)
		}
		out := buf.String()
		if view == "place" {
			for _, want := range []string{`data-serial="DK19256F40259"`, `+ New venue…`, `Send to Lab`, `<option value="pos" selected>`, `Pick a role…`, `href="/devices?restaurant=`} {
				if !strings.Contains(out, want) {
					t.Errorf("place view lacks %q", want)
				}
			}
		}
		for _, gone := range []string{"Setting up", "Hand over", "setup-again", "/visits"} {
			if strings.Contains(out, gone) {
				t.Errorf("%s view still mentions %q", view, gone)
			}
		}
	}
	// Field's Setup with their own queue: no venue creation, no Lab.
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "my_visits.html", map[string]any{"Place": rows, "Venues": venues, "Others": 2}); err != nil {
		t.Fatalf("my_visits.html: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "Your devices to place") || strings.Contains(out, "+ New venue") || strings.Contains(out, "Send to Lab") {
		t.Errorf("field setup queue: wrong controls")
	}
	// Nothing to place: the empty state points at Enroll.
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "my_visits.html", map[string]any{"Place": []placeRow{}, "Venues": venues}); err != nil {
		t.Fatalf("my_visits.html empty: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "Nothing to place") || !strings.Contains(out, `href="/setup/mine/enroll"`) {
		t.Errorf("field setup empty state lacks the Enroll pointer")
	}
	// Field's Enroll: the three steps, the guide popup and what is waiting.
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "my_enroll.html", map[string]any{"Waiting": rows[:1], "WaitingN": 1}); err != nil {
		t.Fatalf("my_enroll.html: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "Place what you enrolled") || !strings.Contains(out, `id="se-guide-dlg"`) || !strings.Contains(out, `href="/setup/mine"`) {
		t.Errorf("field enroll page: missing a card")
	}
}

func TestToPlaceState(t *testing.T) {
	v := uuid.New()
	cases := []struct {
		rid       *uuid.UUID
		placement string
		want      string
	}{
		{nil, "", db.StateLab},
		{nil, db.PlacementLab, db.StateLab},
		{nil, db.PlacementQueue, db.StateToPlace},
		{&v, db.PlacementQueue, db.StateInService}, // placed = in service at once
		{&v, "", db.StateInService},
	}
	for _, c := range cases {
		if got := db.DeviceState(c.rid, c.placement); got != c.want {
			t.Errorf("DeviceState(%v,%q) = %s, want %s", c.rid != nil, c.placement, got, c.want)
		}
	}
}

// Field's "own" grant covers the devices they enrolled until they are placed; the Ops lead's
// to_place grant covers the queue and not the Lab.
func TestOwnAndToPlaceGrants(t *testing.T) {
	v := uuid.New()
	mine, theirs, handed, lab, queue := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	scopes := map[uuid.UUID]db.DeviceScope{
		mine:   {EnrolledBy: "kevin", Placement: db.PlacementQueue},
		theirs: {EnrolledBy: "someone", Placement: db.PlacementQueue},
		handed: {EnrolledBy: "kevin", RestaurantID: &v},
		lab:    {},
		queue:  {Placement: db.PlacementQueue},
	}
	field := &access{ctx: context.Background(), role: "field", username: "kevin", scopes: scopes}
	field.once.Do(func() {})
	own := db.AccessLevel{ScopeType: "own", Level: "manage"}
	for id, want := range map[uuid.UUID]bool{mine: true, theirs: false, handed: false, lab: false} {
		if got := field.grantCoversDevice(own, scopes[id]); got != want {
			t.Errorf("own grant on %v = %v, want %v", scopes[id], got, want)
		}
	}
	ops := &access{ctx: context.Background(), role: "fleet_manager", username: "ops", scopes: scopes}
	ops.once.Do(func() {})
	tp := db.AccessLevel{ScopeType: "state", State: db.StateToPlace, Level: "manage"}
	if !ops.grantCoversDevice(tp, scopes[queue]) || ops.grantCoversDevice(tp, scopes[lab]) {
		t.Errorf("to_place grant should cover the queue and not the Lab")
	}
}
