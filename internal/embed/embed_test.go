package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHashingSimilarTextsAreCloser(t *testing.T) {
	h := NewHashing(DefaultHashDims)
	v, err := h.Embed(context.Background(), []string{
		"Branches start with the Jira ticket number",
		"Name each branch after its Jira ticket",
		"The payout job runs every night",
	}, Document)
	if err != nil {
		t.Fatal(err)
	}
	related, unrelated := Cosine(v[0], v[1]), Cosine(v[0], v[2])
	if related <= unrelated || related < 0.3 {
		t.Fatalf("related %.2f should clearly beat unrelated %.2f", related, unrelated)
	}
	if n := Cosine(v[0], v[0]); math.Abs(n-1) > 1e-5 {
		t.Fatalf("vectors must be unit length, got self-similarity %.4f", n)
	}
}

func TestOpenAIClientRequestAndParsing(t *testing.T) {
	var got embedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,2]},{"index":0,"embedding":[3,4]}]}`))
	}))
	defer srv.Close()

	o := NewOpenAI(Config{URL: srv.URL + "/", Model: "nomic-embed-text", APIKey: "k"})
	vecs, err := o.Embed(context.Background(), []string{"a", "b"}, Query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "nomic-embed-text" || !strings.HasPrefix(got.Input[0], "search_query: ") {
		t.Fatalf("unexpected request %+v", got)
	}
	// Results are reordered by index and normalized.
	if math.Abs(float64(vecs[0][0])-0.6) > 1e-6 || math.Abs(float64(vecs[1][1])-1) > 1e-6 {
		t.Fatalf("unexpected vectors %v", vecs)
	}
}

func TestOpenAIClientReportsServerErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found, try pulling it first"}}`))
	}))
	defer srv.Close()
	_, err := NewOpenAI(Config{URL: srv.URL, Model: "missing"}).Embed(context.Background(), []string{"a"}, Document)
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("want the server's message, got %v", err)
	}
}

func TestAutoFallsBackToHashing(t *testing.T) {
	e, note := New(context.Background(), Config{Provider: "auto", URL: "http://127.0.0.1:1", Model: "x"})
	if _, ok := e.(*Hashing); !ok || !strings.Contains(note, "offline") {
		t.Fatalf("want the hashing fallback, got %T (%s)", e, note)
	}
	if e, _ := New(context.Background(), Config{Provider: "off"}); e != nil {
		t.Fatal("off should return no embedder")
	}
}
