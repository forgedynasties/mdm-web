package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"mdm/internal/db"
	"mdm/internal/ws"
)

// TestTwoCopiesAgainstPostgres runs two hubs ("copies") on one database: a device whose
// socket is in A is seen, pushed to and heard from through B. Needs MDM_TEST_DATABASE_URL.
func TestTwoCopiesAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	hubA, hubB := ws.NewHub(), ws.NewHub()
	a := New(d, hubA, "copy-a-"+uuid.NewString(), "")
	b := New(d, hubB, "copy-b-"+uuid.NewString(), "")
	hubA.SetCluster(a)
	hubB.SetCluster(b)
	a.Start(ctx)
	b.Start(ctx)
	time.Sleep(300 * time.Millisecond) // both LISTENing

	// A device connects to A.
	dev := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := hubA.Upgrade(w, r, dev)
		if err != nil {
			return
		}
		go c.WritePump()
		c.ReadPump()
	}))
	defer srv.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	eventually := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 50; i++ {
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting: %s", what)
	}
	eventually("B sees the device as connected", func() bool { b.refresh(); return hubB.IsConnected(dev) })
	if _, ok := hubB.LocalIDs()[dev]; ok {
		t.Fatal("the device's socket is in A, not B")
	}

	read := func() string {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, m, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("device read: %v", err)
		}
		return string(m)
	}
	if !hubB.Push(dev, []byte(`{"type":"ping_request","nonce":"x"}`)) {
		t.Fatal("B could not push to A's device")
	}
	if got := read(); !strings.Contains(got, "ping_request") {
		t.Fatalf("device got %q", got)
	}
	big := `{"type":"config","pad":"` + strings.Repeat("x", 20000) + `"}`
	if !hubB.Push(dev, []byte(big)) {
		t.Fatal("B could not push a large frame")
	}
	if got := read(); len(got) != len(big) {
		t.Fatalf("large frame arrived with %d bytes, want %d", len(got), len(big))
	}

	// A's dashboard events reach B's subscribers.
	cmds := hubB.SubscribeCommandUpdates()
	cmd := uuid.New()
	hubA.PublishCommandUpdate(cmd)
	select {
	case ev := <-cmds:
		if ev.CommandID != cmd {
			t.Fatalf("B got command %s, want %s", ev.CommandID, cmd)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B never heard A's command update")
	}

	// A Lock button on B waits for the device's pong, which arrives at A.
	wait := hubB.RegisterPingWaiter("nonce-1")
	hubA.SignalPong("nonce-1")
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
		t.Fatal("B's waiter never got the pong A received")
	}

	// A session request for A's device that reaches B is served by A (remote, shell, logcat).
	served := func(name string, cl *Cluster) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cl.ProxyIfRemote(w, r, dev) {
				return
			}
			w.Write([]byte("served by " + name))
		}))
	}
	webA, webB := served("A", a), served("B", b)
	defer webA.Close()
	defer webB.Close()
	if err := d.ClusterHeartbeat(ctx, a.id, webA.URL); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(webB.URL + "/devices/x/logcat/stream")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 64)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if got := string(body[:n]); got != "served by A" {
		t.Fatalf("a request to B for A's device got %q, want it served by A", got)
	}

	// The device leaves: B stops seeing it once A's write lands.
	conn.Close()
	eventually("B sees the device gone", func() bool { b.refresh(); return !hubB.IsConnected(dev) })
	if hubB.Push(dev, []byte(`{}`)) {
		t.Error("a push to a device no copy holds must report false")
	}
}

// TestRemoteTokensAgainstPostgres: a token is good once, on any copy, until it expires.
func TestRemoteTokensAgainstPostgres(t *testing.T) {
	url := os.Getenv("MDM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MDM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.RunMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	dev := uuid.New()
	if err := d.PutRemoteToken(ctx, "tok-1", dev, time.Minute); err != nil {
		t.Fatal(err)
	}
	if id, ok, err := d.TakeRemoteToken(ctx, "tok-1"); err != nil || !ok || id != dev {
		t.Fatalf("first take: %s %v %v", id, ok, err)
	}
	if _, ok, _ := d.TakeRemoteToken(ctx, "tok-1"); ok {
		t.Fatal("a token must work once")
	}
	if err := d.PutRemoteToken(ctx, "tok-2", dev, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.TakeRemoteToken(ctx, "tok-2"); ok {
		t.Fatal("an expired token must not work")
	}
}
