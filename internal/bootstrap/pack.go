package bootstrap

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Pathway packs (docs/design.md, "Pathway packs"): versioned bundles of pre-wired
// memory installed into their own namespace, pack/<name>, as low-weight priors.
// Packs are data, never instructions.

//go:embed packs/*.json
var builtinPacks embed.FS

// Pack is the on-disk pack format.
type Pack struct {
	Name        string       `json:"name"`
	Version     string       `json:"version"`
	Description string       `json:"description"`
	Memories    []PackMemory `json:"memories"`
	Edges       []PackEdge   `json:"edges"`
}

// PackMemory is one memory in a pack, referenced by its key within the pack.
type PackMemory struct {
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

// PackEdge links two pack memories by key.
type PackEdge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Kind   string  `json:"kind"`
	Weight float64 `json:"weight"`
}

var packName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// BuiltinPacks lists the packs shipped inside the binary.
func BuiltinPacks() []string {
	entries, _ := builtinPacks.ReadDir("packs")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names
}

// LoadPack reads a built-in pack by name, or a pack file by path.
func LoadPack(nameOrPath string) (Pack, error) {
	var r io.Reader
	if packName.MatchString(nameOrPath) {
		f, err := builtinPacks.Open("packs/" + nameOrPath + ".json")
		if err != nil {
			return Pack{}, fmt.Errorf("no built-in pack %q (available: %s)", nameOrPath, strings.Join(BuiltinPacks(), ", "))
		}
		defer f.Close()
		r = f
	} else {
		f, err := os.Open(nameOrPath)
		if err != nil {
			return Pack{}, err
		}
		defer f.Close()
		r = f
	}
	var p Pack
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return Pack{}, fmt.Errorf("invalid pack: %w", err)
	}
	if !packName.MatchString(p.Name) {
		return Pack{}, fmt.Errorf("invalid pack name %q", p.Name)
	}
	return p, nil
}

// PackResult reports a pack installation.
type PackResult struct {
	Namespace string `json:"namespace"`
	Memories  int    `json:"memories"`
	Edges     int    `json:"edges"`
}

var packKinds = map[string]bool{"concept": true, "fact": true, "procedure": true}

// InstallPack writes a pack into pack/<name>. Reinstalling the same pack is a no-op
// for memories it already holds.
func InstallPack(st *store.Store, p Pack) (PackResult, error) {
	ns := "pack/" + p.Name
	res := PackResult{Namespace: ns}
	ids := map[string]string{}
	for _, m := range p.Memories {
		if !packKinds[m.Kind] {
			return res, fmt.Errorf("pack memory %q: kind must be concept, fact or procedure", m.Key)
		}
		id, created, err := st.InsertSystem(store.SystemMemory{
			Key: "pack:" + p.Name + ":" + m.Key, Kind: m.Kind, Namespace: ns, Service: "pack",
			Content: m.Content, Tags: append([]string{"pack", p.Name}, m.Tags...), Strength: ImportStrength,
		})
		if err != nil {
			return res, fmt.Errorf("pack memory %q: %w", m.Key, err)
		}
		ids[m.Key] = id
		if created {
			res.Memories++
		}
	}
	for _, e := range p.Edges {
		a, okA := ids[e.From]
		b, okB := ids[e.To]
		if !okA || !okB {
			return res, fmt.Errorf("pack edge %s→%s refers to an unknown key", e.From, e.To)
		}
		kind := e.Kind
		if kind == "" {
			kind = "coactive"
		}
		// Pack edges are priors: capped low so real use can outweigh them.
		if err := st.AddEdge(a, b, kind, min(e.Weight, 0.4)); err != nil {
			return res, err
		}
		res.Edges++
	}
	return res, nil
}
