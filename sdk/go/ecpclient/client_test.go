package ecpclient_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecpserver"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
	"github.com/zakeripouya/EngramContextProtocol/sdk/go/ecpclient"
)

func TestHTTPClientWithNotifications(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Pair("go-sdk", []string{"me"}, []string{"me"}, ""); err != nil {
		t.Fatal(err)
	}
	tok, _ := st.CreateToken("go-sdk", false)
	srv := ecpserver.New(st, ecpserver.Options{PollEvery: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()

	c, err := ecpclient.DialHTTP(ctx, ts.URL+"/ecp", tok, ecpclient.Info{Name: "go-sdk", Conflict: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Content: "Standups are at 9:30"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Observe(ctx, ecpclient.ObserveParams{Kind: "fact", Content: "Standups are at 10:00", Contradicts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-c.Notifications():
		if n.Method != "notifications/engram/conflict" || !strings.Contains(string(n.Params), "9:30") {
			t.Fatalf("unexpected notification %s %s", n.Method, n.Params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no conflict notification")
	}
	hits, err := c.Recall(ctx, ecpclient.RecallParams{Query: "when is standup"})
	if err != nil || len(hits) == 0 {
		t.Fatalf("recall: %v %v", hits, err)
	}
	if _, err := c.Get(ctx, "mem_missing"); !ecpclient.IsCode(err, ecpclient.CodeMemoryNotFound) {
		t.Fatalf("want -32005, got %v", err)
	}
}
