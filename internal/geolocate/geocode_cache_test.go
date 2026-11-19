package geolocate

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

type fakeAddrStore struct {
	addrs map[string]string
	calls int
	puts  int
}

func (f *fakeAddrStore) GetAddr(_ context.Context, cell string, _ int) (string, bool, error) {
	a, ok := f.addrs[cell]
	return a, ok, nil
}
func (f *fakeAddrStore) PutAddr(_ context.Context, cell, a string) error {
	f.puts++
	f.addrs[cell] = a
	return nil
}
func (f *fakeAddrStore) CountCall(context.Context) (int, error) { f.calls++; return f.calls, nil }

// failingTransport fails the test if anything tries to reach Google.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("a request went to Google")
	return nil, errors.New("no network in tests")
}

func TestAddrCacheKeyGroupsJitter(t *testing.T) {
	a := addrCacheKey(37.70123, -121.45498)
	b := addrCacheKey(37.70141, -121.45531) // ~40 m away: the same cell
	if a != b {
		t.Fatalf("jitter of tens of metres must share a cell: %s vs %s", a, b)
	}
	if c := addrCacheKey(37.7061, -121.4549); c == a {
		t.Fatal("a point 700 m away must be another cell")
	}
}

func TestReverseUsesSharedStoreAndCap(t *testing.T) {
	g := NewGeocoder("k")
	g.client = &http.Client{Transport: failingTransport{t}}
	store := &fakeAddrStore{addrs: map[string]string{addrCacheKey(37.7012, -121.455): "3200 N Naglee Rd, Tracy, CA"}}
	g.SetStore(store, 2)

	// A cell another copy already paid for: no request, no call counted.
	addr, err := g.Reverse(context.Background(), 37.7014, -121.4553)
	if err != nil || addr != "3200 N Naglee Rd, Tracy, CA" {
		t.Fatalf("store hit: %q %v", addr, err)
	}
	if store.calls != 0 {
		t.Fatalf("a store hit must not count a paid call (%d)", store.calls)
	}

	// Over the daily cap: no address and no request.
	store.calls = 2
	addr, err = g.Reverse(context.Background(), 10.0, 10.0)
	if err != nil || addr != "" {
		t.Fatalf("over the cap: %q %v", addr, err)
	}
}
