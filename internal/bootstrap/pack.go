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
	Kind        string       `json:"kind,omitempty"` // template (default), source or instance
	Description string       `json:"description"`
	Memories    []PackMemory `json:"memories"`
	Edges       []PackEdge   `json:"edges"`
	Signature   *Signature   `json:"signature,omitempty"`
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
	Memories  int    `json:"memories"` // added
	Updated   int    `json:"updated"`  // changed in place by an update
	Archived  int    `json:"archived"` // dropped by an update
	Edges     int    `json:"edges"`    // pathways added
}

var packKinds = map[string]bool{"concept": true, "fact": true, "procedure": true}

// InstallOptions say where a pack came from.
type InstallOptions struct {
	Source    string // builtin, a file path, or registry:<name>
	Publisher string // signing key id
	Verified  bool   // signed by a trusted key (or built in)
}

// InstallPack installs a built-in or already-verified pack.
func InstallPack(st *store.Store, p Pack) (PackResult, error) {
	return Install(st, p, InstallOptions{Source: "builtin", Verified: true})
}

// Install writes a pack into pack/<name>, or updates an installed one: new
// memories are added, changed ones updated in place (keeping their ids and
// learned pathways), and ones the new version dropped are archived. Pathways are
// added at their prior weight (capped at 0.4) only when absent, so updating a
// pack never resets what use has taught.
func Install(st *store.Store, p Pack, o InstallOptions) (PackResult, error) {
	ns := "pack/" + p.Name
	res := PackResult{Namespace: ns}
	if p.Kind == "" {
		p.Kind = "template"
	}
	if p.Kind != "template" && p.Kind != "source" && p.Kind != "instance" {
		return res, fmt.Errorf("pack kind must be template, source or instance")
	}
	prefix := "pack:" + p.Name + ":"
	before, err := st.SystemKeys(prefix)
	if err != nil {
		return res, err
	}
	ids := map[string]string{}
	for _, m := range p.Memories {
		if !packKinds[m.Kind] {
			return res, fmt.Errorf("pack memory %q: kind must be concept, fact or procedure", m.Key)
		}
		tags := append([]string{"pack", p.Name}, m.Tags...)
		if id, ok := before[prefix+m.Key]; ok {
			changed, err := st.UpdateSystemMemory(id, m.Content, tags)
			if err != nil {
				return res, fmt.Errorf("pack memory %q: %w", m.Key, err)
			}
			if changed {
				res.Updated++
			}
			ids[m.Key] = id
			continue
		}
		id, created, err := st.InsertSystem(store.SystemMemory{
			Key: prefix + m.Key, Kind: m.Kind, Namespace: ns, Service: "pack",
			Content: m.Content, Tags: tags, Strength: ImportStrength,
		})
		if err != nil {
			return res, fmt.Errorf("pack memory %q: %w", m.Key, err)
		}
		ids[m.Key] = id
		if created {
			res.Memories++
		}
	}
	for key, id := range before {
		if _, kept := ids[strings.TrimPrefix(key, prefix)]; !kept {
			if err := st.Archive(id, ""); err == nil {
				res.Archived++
			}
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
		added, err := st.AddPackEdge(p.Name, a, b, kind, min(e.Weight, 0.4))
		if err != nil {
			return res, err
		}
		if added {
			res.Edges++
		}
	}
	if o.Source == "" {
		o.Source = "file"
	}
	return res, st.RecordPack(store.PackInfo{Name: p.Name, Version: p.Version, Kind: p.Kind, Source: o.Source,
		Publisher: o.Publisher, Verified: o.Verified, Description: p.Description})
}
