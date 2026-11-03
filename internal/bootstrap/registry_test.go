package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func samplePack(version string, memories ...PackMemory) Pack {
	return Pack{Name: "ops", Version: version, Description: "ops knowledge", Memories: memories,
		Edges: []PackEdge{{From: "deploy", To: "rollback", Kind: "coactive", Weight: 0.9}}}
}

func TestSignAndVerify(t *testing.T) {
	st := newStore(t)
	dir := t.TempDir()
	pub, err := GenerateKey(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateKey(filepath.Join(dir, "k")); err == nil {
		t.Fatal("must not overwrite an existing key")
	}
	key, err := LoadKey(filepath.Join(dir, "k"))
	if err != nil {
		t.Fatal(err)
	}
	p := samplePack("1.0.0", PackMemory{Key: "deploy", Kind: "fact", Content: "Deploys are blue-green"},
		PackMemory{Key: "rollback", Kind: "procedure", Content: "Rollback flips the load balancer back"})
	if _, err := Verify(st, p); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("want unsigned, got %v", err)
	}
	signed, err := Sign(p, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(st, signed); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("an untrusted key must not verify, got %v", err)
	}
	if _, err := Trust(st, base64.StdEncoding.EncodeToString(pub), "ops-team"); err != nil {
		t.Fatal(err)
	}
	if id, err := Verify(st, signed); err != nil || id != KeyID(pub) {
		t.Fatalf("a trusted signature must verify: %q %v", id, err)
	}
	tampered := signed
	tampered.Memories = append([]PackMemory{}, signed.Memories...)
	tampered.Memories[0].Content = "Deploys go straight to production"
	if _, err := Verify(st, tampered); !errors.Is(err, ErrBadSig) {
		t.Fatalf("a changed pack must fail verification, got %v", err)
	}
	if n, _ := Untrust(st, "ops-team"); n != 1 {
		t.Fatal("untrust by name")
	}
}

func TestUpdateKeepsLearnedWeights(t *testing.T) {
	st := newStore(t)
	v1 := samplePack("1.0.0",
		PackMemory{Key: "deploy", Kind: "fact", Content: "Deploys are blue-green"},
		PackMemory{Key: "rollback", Kind: "procedure", Content: "Rollback flips the load balancer back"},
		PackMemory{Key: "freeze", Kind: "fact", Content: "No deploys on Fridays"})
	res, err := Install(st, v1, InstallOptions{Source: "test", Verified: true})
	if err != nil || res.Memories != 3 || res.Edges != 1 {
		t.Fatalf("install: %+v %v", res, err)
	}
	keys, _ := st.SystemKeys("pack:ops:")
	deploy, rollback, freeze := keys["pack:ops:deploy"], keys["pack:ops:rollback"], keys["pack:ops:freeze"]
	if w := edgeWeight(t, st, deploy, rollback); w != 0.4 {
		t.Fatalf("pack priors are capped at 0.4, got %v", w)
	}
	// Use teaches the pathway something.
	if err := st.AddEdge(deploy, rollback, "coactive", 0.85); err != nil {
		t.Fatal(err)
	}
	v2 := samplePack("1.1.0",
		PackMemory{Key: "deploy", Kind: "fact", Content: "Deploys are blue-green with a canary step"},
		PackMemory{Key: "rollback", Kind: "procedure", Content: "Rollback flips the load balancer back"})
	res, err = Install(st, v2, InstallOptions{Source: "test", Verified: true})
	if err != nil || res.Updated != 1 || res.Archived != 1 || res.Memories != 0 {
		t.Fatalf("update: %+v %v", res, err)
	}
	if w := edgeWeight(t, st, deploy, rollback); w != 0.85 {
		t.Fatalf("an update must keep the learned weight, got %v", w)
	}
	cur, _ := st.CurrentMemories("pack/ops")
	ids := map[string]string{}
	for _, m := range cur {
		ids[m.ID] = m.Content
	}
	if ids[deploy] != "Deploys are blue-green with a canary step" || ids[freeze] != "" {
		t.Fatalf("changed memory updated in place, dropped one archived: %+v", ids)
	}
	info, ok, _ := st.InstalledPack("ops")
	if !ok || info.Version != "1.1.0" {
		t.Fatalf("installed version: %+v", info)
	}
	stats, err := st.PackStatsFor("ops")
	if err != nil || stats.Strengthened != 1 || stats.Edges != 1 {
		t.Fatalf("reweighting stats: %+v %v", stats, err)
	}
	// Removing then reinstalling works (stale import keys are cleaned up).
	if _, err := st.RemoveNamespace("pack/ops"); err != nil {
		t.Fatal(err)
	}
	if err := st.ForgetPack("ops"); err != nil {
		t.Fatal(err)
	}
	res, err = Install(st, v2, InstallOptions{Source: "test"})
	if err != nil || res.Memories != 2 {
		t.Fatalf("reinstall after remove: %+v %v", res, err)
	}
}

func edgeWeight(t *testing.T, st *store.Store, a, b string) float64 {
	t.Helper()
	edges, err := st.EdgesIn("pack/ops", "coactive")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range edges {
		if e.From == a && e.To == b {
			return e.Weight
		}
	}
	t.Fatalf("no edge %s→%s", a, b)
	return 0
}

func TestRegistryResolveAndPublish(t *testing.T) {
	st := newStore(t)
	dir := t.TempDir()
	if _, err := GenerateKey(filepath.Join(dir, "k")); err != nil {
		t.Fatal(err)
	}
	key, _ := LoadKey(filepath.Join(dir, "k"))
	reg := filepath.Join(dir, "registry")
	for _, v := range []string{"1.2.0", "1.10.0", "1.9.3"} {
		p, _ := Sign(samplePack(v, PackMemory{Key: "deploy", Kind: "fact", Content: "Deploys " + v},
			PackMemory{Key: "rollback", Kind: "procedure", Content: "Rollback"}), key)
		f := filepath.Join(dir, "ops-"+v+".json")
		if err := writeJSON(f, p); err != nil {
			t.Fatal(err)
		}
		if _, err := Publish(f, reg, "test"); err != nil {
			t.Fatal(err)
		}
	}
	unsigned := filepath.Join(dir, "unsigned.json")
	_ = writeJSON(unsigned, samplePack("2.0.0"))
	if _, err := Publish(unsigned, reg, "test"); err == nil {
		t.Fatal("unsigned packs must not be published")
	}
	if err := AddRegistry(st, Registry{Name: "test", Location: reg}); err != nil {
		t.Fatal(err)
	}
	if err := AddRegistry(st, Registry{Name: "plain", Location: "http://example.com/packs"}); err == nil {
		t.Fatal("remote registries must use https")
	}
	f, err := Resolve(context.Background(), st, "ops", "")
	if err != nil || f.Entry.Version != "1.10.0" {
		t.Fatalf("newest version by number, not text: %+v %v", f.Entry, err)
	}
	f, err = Resolve(context.Background(), st, "ops", "1.2.0")
	if err != nil || f.Entry.Version != "1.2.0" {
		t.Fatalf("a pinned version: %+v %v", f.Entry, err)
	}
	p, err := FetchPack(context.Background(), f.Registry, f.Entry)
	if err != nil || p.Version != "1.2.0" {
		t.Fatalf("fetch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(reg, filepath.FromSlash(f.Entry.File)), []byte(`{"name":"ops","version":"1.2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchPack(context.Background(), f.Registry, f.Entry); err == nil {
		t.Fatal("a file that doesn't match the index checksum must be refused")
	}
}

func TestExportKeepsPrivateKindsOut(t *testing.T) {
	st := newStore(t)
	if err := st.Pair("cli", []string{"team/x"}, []string{"team/x"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, in := range []store.ObserveInput{
		{Kind: "fact", Namespace: "team/x", Content: "Invoices are sent on the first business day"},
		{Kind: "episode", Namespace: "team/x", Content: "Talked with the accountant about invoices"},
		{Kind: "decision", Namespace: "team/x", Author: "user", Content: "Use net-30 terms",
			Decision: &store.Decision{Situation: "terms", Choice: "net-30"}},
	} {
		if _, err := st.Observe("cli", in); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Export(st, "team/x", ExportOptions{Name: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Memories) != 1 || p.Memories[0].Kind != "fact" || p.Kind != "instance" {
		t.Fatalf("only facts, concepts and procedures are exported: %+v", p.Memories)
	}
	if _, err := Export(st, "team/empty", ExportOptions{Name: "nothing"}); err == nil {
		t.Fatal("an empty export is an error")
	}
}

func writeJSON(path string, p Pack) error {
	b, err := jsonMarshalIndent(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func jsonMarshalIndent(p Pack) ([]byte, error) { return json.MarshalIndent(p, "", "  ") }
