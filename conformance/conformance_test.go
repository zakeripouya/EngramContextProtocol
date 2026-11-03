package conformance

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/ecpserver"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
	"github.com/zakeripouya/EngramContextProtocol/sdk/go/ecpclient"
)

func engram(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Pair("conformance", []string{"conformance/test"}, []string{"conformance/test"}, ""); err != nil {
		t.Fatal(err)
	}
	return st
}

func check(t *testing.T, rep Report) {
	t.Helper()
	var buf bytes.Buffer
	rep.Print(&buf)
	t.Log("\n" + buf.String())
	for _, r := range rep.Results {
		if r.Status == Fail {
			t.Errorf("%s %s %s: %s", r.Level, r.ID, r.Title, r.Detail)
		}
	}
	if len(rep.Results) < 20 {
		t.Fatalf("only %d checks ran", len(rep.Results))
	}
}

// Engram passes its own conformance suite over HTTP.
func TestEngramOverHTTP(t *testing.T) {
	st := engram(t)
	tok, _ := st.CreateToken("conformance", false)
	srv := ecpserver.New(st, ecpserver.Options{Version: "test"})
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()
	rep, err := Run(context.Background(), Target{URL: ts.URL + "/ecp", Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	check(t, rep)
}

// ... and over stdio.
func TestEngramOverStdio(t *testing.T) {
	st := engram(t)
	srv := ecpserver.New(st, ecpserver.Options{Version: "test"})
	connect := func(ctx context.Context) (*ecpclient.Client, error) {
		toServer, fromClient := io.Pipe()
		toClient, fromServer := io.Pipe()
		go func() {
			_ = srv.ServeStdio(context.Background(), nil, toServer, fromServer, "")
			fromServer.Close()
		}()
		return ecpclient.NewStdioRaw(toClient, fromClient, fromClient.Close), nil
	}
	rep, err := Run(context.Background(), Target{Connect: connect})
	if err != nil {
		t.Fatal(err)
	}
	check(t, rep)
}

// A broken server fails: one that skips the provenance rule.
func TestDetectsViolations(t *testing.T) {
	st := engram(t)
	tok, _ := st.CreateToken("someone-else", false) // writes attributed to the wrong identity
	if err := st.Pair("someone-else", []string{"conformance/test"}, []string{"conformance/test"}, ""); err != nil {
		t.Fatal(err)
	}
	srv := ecpserver.New(st, ecpserver.Options{})
	ts := httptest.NewServer(srv.HTTPHandler())
	defer ts.Close()
	rep, _ := Run(context.Background(), Target{URL: ts.URL + "/ecp", Token: tok})
	failed := false
	for _, r := range rep.Results {
		if r.ID == "M2" && r.Status == Fail {
			failed = true
		}
	}
	if !failed {
		t.Fatal("the suite should notice writes attributed to a different client")
	}
}
