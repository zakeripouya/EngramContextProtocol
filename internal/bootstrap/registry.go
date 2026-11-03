package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Pack registries (milestone M11). A registry is a static index.json plus pack
// files, served from a directory (local), a public URL (for example GitHub raw
// files or Pages), or a private URL that needs a bearer token read from an
// environment variable. Anyone can host one with a static file server.
//
//	{"name": "acme", "packs": [{"name": "developer", "version": "0.2.0", "kind": "template",
//	  "description": "...", "file": "packs/developer-0.2.0.json", "sha256": "...", "publisher": "1a2b..."}]}

// Registry is a configured pack source.
type Registry struct {
	Name     string `json:"name"`
	Location string `json:"location"`           // a directory, or an http(s) URL of the registry root
	TokenEnv string `json:"tokenEnv,omitempty"` // environment variable holding a bearer token (private registries)
}

// IndexEntry is one pack version in a registry.
type IndexEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`
	File        string `json:"file"`
	SHA256      string `json:"sha256"`
	Publisher   string `json:"publisher,omitempty"`
}

// Index is a registry's index.json.
type Index struct {
	Name  string       `json:"name"`
	Packs []IndexEntry `json:"packs"`
}

const (
	settingRegistries = "pack.registries"
	maxPackBytes      = 32 << 20
)

// Registries lists configured registries.
func Registries(st *store.Store) ([]Registry, error) {
	v, err := st.Setting(settingRegistries)
	if err != nil || v == "" {
		return []Registry{}, err
	}
	var rs []Registry
	return rs, json.Unmarshal([]byte(v), &rs)
}

// AddRegistry adds or replaces a registry.
func AddRegistry(st *store.Store, r Registry) error {
	if !packName.MatchString(r.Name) {
		return fmt.Errorf("registry names use lowercase letters, digits and dashes")
	}
	if strings.HasPrefix(r.Location, "http://") && !isLoopbackURL(r.Location) {
		return fmt.Errorf("use https for remote registries")
	}
	if !strings.HasPrefix(r.Location, "https://") && !strings.HasPrefix(r.Location, "http://") {
		abs, err := filepath.Abs(r.Location)
		if err != nil {
			return err
		}
		r.Location = abs
	}
	rs, err := Registries(st)
	if err != nil {
		return err
	}
	var out []Registry
	for _, x := range rs {
		if x.Name != r.Name {
			out = append(out, x)
		}
	}
	b, _ := json.Marshal(append(out, r))
	return st.SetSetting(settingRegistries, string(b))
}

// RemoveRegistry removes a registry by name.
func RemoveRegistry(st *store.Store, name string) error {
	rs, err := Registries(st)
	if err != nil {
		return err
	}
	out := []Registry{}
	found := false
	for _, x := range rs {
		if x.Name == name {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return fmt.Errorf("no registry %q", name)
	}
	b, _ := json.Marshal(out)
	return st.SetSetting(settingRegistries, string(b))
}

func isLoopbackURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func (r Registry) remote() bool {
	return strings.HasPrefix(r.Location, "https://") || strings.HasPrefix(r.Location, "http://")
}

// read fetches a file relative to the registry root.
func (r Registry) read(ctx context.Context, rel string) ([]byte, error) {
	if strings.Contains(rel, "..") {
		return nil, fmt.Errorf("invalid path %q in registry %s", rel, r.Name)
	}
	if !r.remote() {
		f, err := os.Open(filepath.Join(r.Location, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, maxPackBytes+1))
	}
	u := strings.TrimSuffix(r.Location, "/") + "/" + path.Clean(rel)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if r.TokenEnv != "" {
		tok := os.Getenv(r.TokenEnv)
		if tok == "" {
			return nil, fmt.Errorf("registry %s needs a token in $%s", r.Name, r.TokenEnv)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry %s: %s returned %d", r.Name, rel, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxPackBytes+1))
	if len(b) > maxPackBytes {
		return nil, fmt.Errorf("registry %s: %s is larger than %d MB", r.Name, rel, maxPackBytes>>20)
	}
	return b, err
}

// FetchIndex reads a registry's index.
func FetchIndex(ctx context.Context, r Registry) (Index, error) {
	b, err := r.read(ctx, "index.json")
	if err != nil {
		return Index{}, err
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return Index{}, fmt.Errorf("registry %s: invalid index.json: %w", r.Name, err)
	}
	return idx, nil
}

// FetchPack downloads a pack and checks it against the index's checksum.
func FetchPack(ctx context.Context, r Registry, e IndexEntry) (Pack, error) {
	b, err := r.read(ctx, e.File)
	if err != nil {
		return Pack{}, err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != strings.ToLower(e.SHA256) {
		return Pack{}, fmt.Errorf("registry %s: %s does not match its checksum", r.Name, e.File)
	}
	return parsePack(b)
}

func parsePack(b []byte) (Pack, error) {
	var p Pack
	if err := json.Unmarshal(b, &p); err != nil {
		return Pack{}, fmt.Errorf("invalid pack: %w", err)
	}
	if !packName.MatchString(p.Name) {
		return Pack{}, fmt.Errorf("invalid pack name %q", p.Name)
	}
	return p, nil
}

// CompareVersions compares dotted numeric versions (1.10.0 > 1.9.2); a
// non-numeric part compares as text.
func CompareVersions(a, b string) int {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		switch {
		case ex == nil && ey == nil && nx != ny:
			if nx < ny {
				return -1
			}
			return 1
		case (ex != nil || ey != nil) && x != y:
			return strings.Compare(x, y)
		}
	}
	return 0
}

// Found is a pack found in a registry.
type Found struct {
	Registry Registry
	Entry    IndexEntry
}

// Search lists packs in every registry whose name or description contains term
// ("" lists all), newest version of each name first per registry.
func Search(ctx context.Context, st *store.Store, term string) ([]Found, []error) {
	rs, err := Registries(st)
	if err != nil {
		return nil, []error{err}
	}
	var out []Found
	var errs []error
	term = strings.ToLower(term)
	for _, r := range rs {
		idx, err := FetchIndex(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range idx.Packs {
			if term == "" || strings.Contains(strings.ToLower(e.Name+" "+e.Description), term) {
				out = append(out, Found{r, e})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Entry.Name != out[j].Entry.Name {
			return out[i].Entry.Name < out[j].Entry.Name
		}
		return CompareVersions(out[i].Entry.Version, out[j].Entry.Version) > 0
	})
	return out, errs
}

// Resolve finds a pack by name, and optionally version ("" means newest), across
// registries in the order they were added.
func Resolve(ctx context.Context, st *store.Store, name, version string) (Found, error) {
	found, errs := Search(ctx, st, "")
	var best *Found
	for i := range found {
		f := found[i]
		if f.Entry.Name != name || (version != "" && f.Entry.Version != version) {
			continue
		}
		if best == nil || CompareVersions(f.Entry.Version, best.Entry.Version) > 0 {
			best = &f
		}
	}
	if best == nil {
		msg := fmt.Sprintf("no pack %q", name)
		if version != "" {
			msg += " version " + version
		}
		msg += " in any registry"
		if len(errs) > 0 {
			msg += fmt.Sprintf(" (%d registr(ies) unreachable: %v)", len(errs), errs[0])
		}
		return Found{}, fmt.Errorf("%s", msg)
	}
	return *best, nil
}

// Publish adds a signed pack file to a local registry directory (create the
// directory to start a registry) and updates its index.json. Serve the
// directory statically, or commit it to a repository, to share it.
func Publish(packFile, registryDir, registryName string) (IndexEntry, error) {
	b, err := os.ReadFile(packFile)
	if err != nil {
		return IndexEntry{}, err
	}
	p, err := parsePack(b)
	if err != nil {
		return IndexEntry{}, err
	}
	if p.Signature == nil {
		return IndexEntry{}, fmt.Errorf("sign the pack before publishing: engram pack sign %s", packFile)
	}
	if p.Version == "" {
		return IndexEntry{}, fmt.Errorf("the pack needs a version")
	}
	rel := "packs/" + p.Name + "-" + p.Version + ".json"
	if err := os.MkdirAll(filepath.Join(registryDir, "packs"), 0o755); err != nil {
		return IndexEntry{}, err
	}
	if err := os.WriteFile(filepath.Join(registryDir, filepath.FromSlash(rel)), b, 0o644); err != nil {
		return IndexEntry{}, err
	}
	idx := Index{Name: registryName}
	if raw, err := os.ReadFile(filepath.Join(registryDir, "index.json")); err == nil {
		if err := json.Unmarshal(raw, &idx); err != nil {
			return IndexEntry{}, fmt.Errorf("existing index.json is invalid: %w", err)
		}
	}
	if idx.Name == "" {
		idx.Name = registryName
	}
	sum := sha256.Sum256(b)
	e := IndexEntry{Name: p.Name, Version: p.Version, Kind: p.Kind, Description: p.Description, File: rel,
		SHA256: hex.EncodeToString(sum[:]), Publisher: p.Signature.KeyID}
	var entries []IndexEntry
	for _, x := range idx.Packs {
		if !(x.Name == e.Name && x.Version == e.Version) {
			entries = append(entries, x)
		}
	}
	idx.Packs = append(entries, e)
	sort.Slice(idx.Packs, func(i, j int) bool {
		if idx.Packs[i].Name != idx.Packs[j].Name {
			return idx.Packs[i].Name < idx.Packs[j].Name
		}
		return CompareVersions(idx.Packs[i].Version, idx.Packs[j].Version) < 0
	})
	out, _ := json.MarshalIndent(idx, "", "  ")
	return e, os.WriteFile(filepath.Join(registryDir, "index.json"), append(out, '\n'), 0o644)
}

// ExportOptions control an instance-pack export.
type ExportOptions struct {
	Name, Version, Description string
	MinStrength                float64 // skip memories weaker than this
}

// exportKinds are what an instance pack may contain. Episodes (raw events that may
// quote conversations), decisions and suggestions stay private.
var exportKinds = map[string]bool{"concept": true, "fact": true, "procedure": true}

// Export turns a namespace's learned subgraph into an instance pack: its current
// facts, concepts and procedures and the pathways between them at their learned
// weights. Episodes, decisions and suggestions are never exported, and content
// that looks like a secret is dropped.
func Export(st *store.Store, namespace string, o ExportOptions) (Pack, error) {
	if !packName.MatchString(o.Name) {
		return Pack{}, fmt.Errorf("pack names use lowercase letters, digits and dashes")
	}
	if o.Version == "" {
		o.Version = "0.1.0"
	}
	mems, err := st.CurrentMemories(namespace)
	if err != nil {
		return Pack{}, err
	}
	p := Pack{Name: o.Name, Version: o.Version, Kind: "instance", Description: o.Description,
		Memories: []PackMemory{}, Edges: []PackEdge{}}
	if p.Description == "" {
		p.Description = "Exported from " + namespace
	}
	keys := map[string]string{}
	for _, m := range mems {
		if !exportKinds[m.Kind] || m.Strength < o.MinStrength || store.FindSensitive(m.Content) != "" {
			continue
		}
		key := fmt.Sprintf("m%03d", len(p.Memories)+1)
		keys[m.ID] = key
		var tags []string
		for _, t := range m.Tags {
			if t != "pack" && t != o.Name {
				tags = append(tags, t)
			}
		}
		p.Memories = append(p.Memories, PackMemory{Key: key, Kind: m.Kind, Content: m.Content, Tags: tags})
	}
	if len(p.Memories) == 0 {
		return p, fmt.Errorf("nothing to export from %s: only current facts, concepts and procedures are exported", namespace)
	}
	edges, err := st.EdgesIn(namespace, "coactive", "similar", "about", "derived")
	if err != nil {
		return p, err
	}
	for _, e := range edges {
		a, okA := keys[e.From]
		b, okB := keys[e.To]
		if okA && okB && e.Weight > 0.05 {
			p.Edges = append(p.Edges, PackEdge{From: a, To: b, Kind: e.Kind, Weight: round2(e.Weight)})
		}
	}
	return p, nil
}

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }
