package aiobackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNilClient(t *testing.T) {
	if c := New("", "", nil); c != nil {
		t.Fatal("no key should give a nil client")
	}
	var c *Client
	if _, err := c.Restaurants(context.Background(), Prod); err != ErrNotConfigured {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesSecrets(t *testing.T) {
	c := New("http://127.0.0.1:1", "k", map[Env]int{Prod: 3})
	for _, q := range []string{`SELECT token FROM restaurant_devices`, `SELECT "privateKey" FROM x`, `SELECT * FROM restaurant`, `SELECT "endpointArn" FROM y`} {
		if _, err := c.Query(context.Background(), Prod, q); err == nil || !strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: err = %v", q, err)
		}
	}
}

func TestDevicesPagesAndParses(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dataset" || r.Header.Get("x-api-key") != "k" {
			http.Error(w, "bad", 400)
			return
		}
		var body struct {
			Database int `json:"database"`
			Native   struct {
				Query string `json:"query"`
			} `json:"native"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Database != 3 {
			http.Error(w, "db", 400)
			return
		}
		queries = append(queries, body.Native.Query)
		var rows [][]any
		start := 1
		if strings.Contains(body.Native.Query, "id > 0") {
			start = 1
		} else {
			start = pageSize + 1
		}
		n := pageSize
		if start > 1 {
			n = 2
		}
		for i := 0; i < n; i++ {
			rows = append(rows, []any{start + i, 2007, "tv", fmt.Sprintf(" S%d ", start+i), "10.0.0.1", true, false, "2026-10-01"})
		}
		cols := []map[string]string{{"name": "id"}, {"name": "restaurantId"}, {"name": "type"}, {"name": "deviceSerialNumber"}, {"name": "ipAddress"}, {"name": "onboarded"}, {"name": "revoked"}, {"name": "updated"}}
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "data": map[string]any{"rows": rows, "cols": cols}})
	}))
	defer srv.Close()
	c := New(srv.URL, "k", map[Env]int{Prod: 3, Stage: 4})
	ds, err := c.Devices(context.Background(), Prod)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != pageSize+2 || len(queries) != 2 {
		t.Fatalf("got %d devices in %d queries", len(ds), len(queries))
	}
	if !strings.Contains(queries[1], fmt.Sprintf("id > %d", pageSize)) {
		t.Fatalf("second page: %s", queries[1])
	}
	if d := ds[0]; d.Serial != "S1" || d.RestaurantID != 2007 || !d.Onboarded || d.Type != "tv" {
		t.Fatalf("parsed %+v", d)
	}
}

func TestFullAddress(t *testing.T) {
	r := Restaurant{Address: "609 1st St", City: "Brentwood", State: "CA", Zip: "94513"}
	if got := r.FullAddress(); got != "609 1st St, Brentwood, CA 94513" {
		t.Fatal(got)
	}
}
