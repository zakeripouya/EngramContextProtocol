package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zakeripouya/EngramContextProtocol/internal/bootstrap"
	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

const packUsage = `usage:
  engram pack list                              installed and built-in packs, with how use has reweighted them
  engram pack search [TERM]                     packs in your registries
  engram pack install NAME[@VERSION] | FILE     install a built-in, registry or local pack
        [--from REGISTRY] [--allow-unsigned] [--allow-untrusted]
  engram pack update [NAME]                     install newer versions from registries (keeps learned weights)
  engram pack remove NAME
  engram pack export NAMESPACE --name NAME [--version V] [--out FILE] [--sign] [--min-strength 0.2]
  engram pack keygen | sign FILE | trust PUBLIC_KEY --name NAME | untrust ID | keys
  engram pack registry add NAME DIR|URL [--token-env VAR] | list | remove NAME
  engram pack publish FILE --to DIR [--name REGISTRY]`

func keyPath() string { return filepath.Join(filepath.Dir(dbPath()), "pack-signing.key") }

func runPack(st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New(packUsage)
	}
	ctx := context.Background()
	switch args[0] {
	case "list":
		installed, err := st.Packs()
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, p := range installed {
			have[p.Name] = true
			s, _ := st.PackStatsFor(p.Name)
			trust := "unverified"
			if p.Verified {
				trust = "verified"
			}
			fmt.Printf("%-16s %-8s %-9s %-20s %-10s %d memories (%d recalled), pathways %d: %d strengthened, %d weakened\n",
				p.Name, p.Version, p.Kind, p.Source, trust, s.Memories, s.Recalled, s.Edges, s.Strengthened, s.Weakened)
		}
		for _, name := range bootstrap.BuiltinPacks() {
			if have[name] {
				continue
			}
			p, err := bootstrap.LoadPack(name)
			if err != nil {
				return err
			}
			fmt.Printf("%-16s %-8s %-9s %-20s not installed  %s\n", p.Name, p.Version, "template", "builtin", p.Description)
		}
		return nil

	case "search":
		term := ""
		if len(args) > 1 {
			term = args[1]
		}
		found, errs := bootstrap.Search(ctx, st, term)
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "engram:", e)
		}
		rs, _ := bootstrap.Registries(st)
		if len(rs) == 0 {
			fmt.Println("No registries. Add one with: engram pack registry add NAME DIR|URL")
		}
		for _, f := range found {
			fmt.Printf("%-16s %-8s %-9s %-12s %s\n", f.Entry.Name, f.Entry.Version, f.Entry.Kind, f.Registry.Name, f.Entry.Description)
		}
		return nil

	case "install":
		fs := flag.NewFlagSet("pack install", flag.ContinueOnError)
		from := fs.String("from", "", "install from this registry")
		allowUnsigned := fs.Bool("allow-unsigned", false, "install a local file that is not signed (for packs you wrote)")
		allowUntrusted := fs.Bool("allow-untrusted", false, "install a local file signed by a key you have not trusted")
		if len(args) < 2 {
			return errors.New(packUsage)
		}
		target := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		return installPack(ctx, st, target, *from, *allowUnsigned, *allowUntrusted)

	case "update":
		installed, err := st.Packs()
		if err != nil {
			return err
		}
		updated := 0
		for _, p := range installed {
			if len(args) > 1 && p.Name != args[1] {
				continue
			}
			switch {
			case p.Source == "builtin":
				b, err := bootstrap.LoadPack(p.Name)
				if err != nil || bootstrap.CompareVersions(b.Version, p.Version) <= 0 {
					continue
				}
				if err := installPack(ctx, st, p.Name, "", false, false); err != nil {
					return err
				}
				updated++
			case strings.HasPrefix(p.Source, "registry:"):
				f, err := bootstrap.Resolve(ctx, st, p.Name, "")
				if err != nil {
					fmt.Fprintln(os.Stderr, "engram:", err)
					continue
				}
				if bootstrap.CompareVersions(f.Entry.Version, p.Version) <= 0 {
					continue
				}
				if err := installPack(ctx, st, p.Name+"@"+f.Entry.Version, f.Registry.Name, false, false); err != nil {
					return err
				}
				updated++
			}
		}
		if updated == 0 {
			fmt.Println("Every pack is up to date.")
		}
		return nil

	case "remove":
		if len(args) != 2 {
			return errors.New(packUsage)
		}
		n, err := st.RemoveNamespace("pack/" + args[1])
		if err != nil {
			return err
		}
		if err := st.ForgetPack(args[1]); err != nil {
			return err
		}
		fmt.Printf("Removed %d memories from pack/%s\n", n, args[1])
		return nil

	case "export":
		if len(args) < 2 {
			return errors.New(packUsage)
		}
		ns := args[1]
		fs := flag.NewFlagSet("pack export", flag.ContinueOnError)
		name := fs.String("name", "", "pack name (lowercase, digits, dashes)")
		version := fs.String("version", "0.1.0", "pack version")
		desc := fs.String("description", "", "one line about the pack")
		out := fs.String("out", "", "file to write; default NAME-VERSION.json")
		sign := fs.Bool("sign", false, "sign with your key (engram pack keygen)")
		minStrength := fs.Float64("min-strength", 0, "skip memories weaker than this")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		p, err := bootstrap.Export(st, ns, bootstrap.ExportOptions{Name: *name, Version: *version, Description: *desc, MinStrength: *minStrength})
		if err != nil {
			return err
		}
		if *sign {
			key, err := bootstrap.LoadKey(keyPath())
			if err != nil {
				return fmt.Errorf("no signing key; create one with engram pack keygen (%v)", err)
			}
			if p, err = bootstrap.Sign(p, key); err != nil {
				return err
			}
		}
		if *out == "" {
			*out = p.Name + "-" + p.Version + ".json"
		}
		if err := writePackFile(*out, p); err != nil {
			return err
		}
		fmt.Printf("Exported %d memories and %d pathways from %s to %s\n", len(p.Memories), len(p.Edges), ns, *out)
		fmt.Println("Only current facts, concepts and procedures are exported; episodes, decisions and suggestions stay private. Review the file before sharing it.")
		return nil

	case "keygen":
		pub, err := bootstrap.GenerateKey(keyPath())
		if err != nil {
			return err
		}
		k, err := bootstrap.Trust(st, base64.StdEncoding.EncodeToString(pub), "me")
		if err != nil {
			return err
		}
		fmt.Printf("Signing key written to %s (keep it private).\nPublic key (share it so others can trust your packs):\n  %s\nKey id: %s (trusted on this machine as \"me\")\n", keyPath(), k.PublicKey, k.KeyID)
		return nil

	case "sign":
		if len(args) != 2 {
			return errors.New(packUsage)
		}
		p, err := bootstrap.LoadPack(args[1])
		if err != nil {
			return err
		}
		key, err := bootstrap.LoadKey(keyPath())
		if err != nil {
			return fmt.Errorf("no signing key; create one with engram pack keygen (%v)", err)
		}
		if p, err = bootstrap.Sign(p, key); err != nil {
			return err
		}
		if err := writePackFile(args[1], p); err != nil {
			return err
		}
		fmt.Printf("Signed %s %s with key %s\n", p.Name, p.Version, p.Signature.KeyID)
		return nil

	case "trust":
		fs := flag.NewFlagSet("pack trust", flag.ContinueOnError)
		name := fs.String("name", "", "who this key belongs to")
		if len(args) < 2 {
			return errors.New(packUsage)
		}
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *name == "" {
			return errors.New("give the key's owner: engram pack trust KEY --name NAME")
		}
		k, err := bootstrap.Trust(st, args[1], *name)
		if err != nil {
			return err
		}
		fmt.Printf("Trusted key %s (%s). Only trust keys you got from their owner through a channel you trust.\n", k.KeyID, k.Name)
		return nil

	case "untrust":
		if len(args) != 2 {
			return errors.New(packUsage)
		}
		n, err := bootstrap.Untrust(st, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Removed %d key(s)\n", n)
		return nil

	case "keys":
		keys, err := bootstrap.TrustedKeys(st)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Println("No trusted keys. Trust a publisher with: engram pack trust PUBLIC_KEY --name NAME")
		}
		for _, k := range keys {
			fmt.Printf("%s  %-16s %s\n", k.KeyID, k.Name, k.PublicKey)
		}
		return nil

	case "registry":
		if len(args) < 2 {
			return errors.New(packUsage)
		}
		switch args[1] {
		case "add":
			if len(args) < 4 {
				return errors.New(packUsage)
			}
			fs := flag.NewFlagSet("pack registry add", flag.ContinueOnError)
			tokenEnv := fs.String("token-env", "", "environment variable with a bearer token, for private registries")
			if err := fs.Parse(args[4:]); err != nil {
				return err
			}
			r := bootstrap.Registry{Name: args[2], Location: args[3], TokenEnv: *tokenEnv}
			if err := bootstrap.AddRegistry(st, r); err != nil {
				return err
			}
			idx, err := bootstrap.FetchIndex(ctx, r)
			if err != nil {
				fmt.Printf("Added registry %s, but it could not be read yet: %v\n", r.Name, err)
				return nil
			}
			fmt.Printf("Added registry %s with %d pack version(s)\n", r.Name, len(idx.Packs))
			return nil
		case "list":
			rs, err := bootstrap.Registries(st)
			if err != nil {
				return err
			}
			for _, r := range rs {
				auth := "public"
				if r.TokenEnv != "" {
					auth = "private ($" + r.TokenEnv + ")"
				}
				fmt.Printf("%-16s %-50s %s\n", r.Name, r.Location, auth)
			}
			return nil
		case "remove":
			if len(args) != 3 {
				return errors.New(packUsage)
			}
			return bootstrap.RemoveRegistry(st, args[2])
		}
		return errors.New(packUsage)

	case "publish":
		if len(args) < 2 {
			return errors.New(packUsage)
		}
		fs := flag.NewFlagSet("pack publish", flag.ContinueOnError)
		to := fs.String("to", "", "the registry directory")
		name := fs.String("name", "packs", "the registry's name in its index")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *to == "" {
			return errors.New("give the registry directory with --to DIR")
		}
		e, err := bootstrap.Publish(args[1], *to, *name)
		if err != nil {
			return err
		}
		fmt.Printf("Published %s %s to %s (%s). Serve the directory or commit it to share it.\n", e.Name, e.Version, *to, e.File)
		return nil
	}
	return errors.New(packUsage)
}

func writePackFile(path string, p bootstrap.Pack) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func installPack(ctx context.Context, st *store.Store, target, from string, allowUnsigned, allowUntrusted bool) error {
	name, version, _ := strings.Cut(target, "@")
	isFile := strings.ContainsAny(target, `/\`) || strings.HasSuffix(target, ".json")
	var p bootstrap.Pack
	var opts bootstrap.InstallOptions
	switch {
	case isFile:
		var err error
		if p, err = bootstrap.LoadPack(target); err != nil {
			return err
		}
		opts.Source = target
		keyID, verr := bootstrap.Verify(st, p)
		opts.Publisher = keyID
		switch {
		case verr == nil:
			opts.Verified = true
		case errors.Is(verr, bootstrap.ErrBadSig):
			return verr
		case errors.Is(verr, bootstrap.ErrUnsigned) && !allowUnsigned:
			return fmt.Errorf("%s is not signed; install it anyway with --allow-unsigned if you wrote it, or ask its author for a signed copy", target)
		case errors.Is(verr, bootstrap.ErrUntrusted) && !allowUntrusted:
			return fmt.Errorf("%s is signed by key %s, which you have not trusted. If you know its owner: engram pack trust %s --name OWNER",
				target, keyID, p.Signature.PublicKey)
		}
	case from == "" && version == "" && slicesContains(bootstrap.BuiltinPacks(), name):
		var err error
		if p, err = bootstrap.LoadPack(name); err != nil {
			return err
		}
		opts = bootstrap.InstallOptions{Source: "builtin", Verified: true}
	default:
		var f bootstrap.Found
		var err error
		if from != "" {
			rs, rerr := bootstrap.Registries(st)
			if rerr != nil {
				return rerr
			}
			found := false
			for _, r := range rs {
				if r.Name == from {
					found = true
					idx, ierr := bootstrap.FetchIndex(ctx, r)
					if ierr != nil {
						return ierr
					}
					for _, e := range idx.Packs {
						if e.Name == name && (version == "" || e.Version == version) &&
							(f.Entry.Version == "" || bootstrap.CompareVersions(e.Version, f.Entry.Version) > 0) {
							f = bootstrap.Found{Registry: r, Entry: e}
						}
					}
				}
			}
			if !found {
				return fmt.Errorf("no registry %q", from)
			}
			if f.Entry.Name == "" {
				return fmt.Errorf("registry %s has no pack %q", from, target)
			}
		} else if f, err = bootstrap.Resolve(ctx, st, name, version); err != nil {
			return err
		}
		if p, err = bootstrap.FetchPack(ctx, f.Registry, f.Entry); err != nil {
			return err
		}
		keyID, verr := bootstrap.Verify(st, p)
		if verr != nil {
			if errors.Is(verr, bootstrap.ErrUntrusted) {
				return fmt.Errorf("%s %s from %s is signed by key %s, which you have not trusted. If you know its publisher: engram pack trust %s --name PUBLISHER",
					p.Name, p.Version, f.Registry.Name, keyID, p.Signature.PublicKey)
			}
			return fmt.Errorf("%s %s from %s: %w (registry packs must be signed by a trusted key)", p.Name, p.Version, f.Registry.Name, verr)
		}
		opts = bootstrap.InstallOptions{Source: "registry:" + f.Registry.Name, Publisher: keyID, Verified: true}
	}
	prev, had, _ := st.InstalledPack(p.Name)
	res, err := bootstrap.Install(st, p, opts)
	if err != nil {
		return err
	}
	if had && prev.Version != p.Version {
		fmt.Printf("Updated %s %s → %s: %d added, %d changed, %d archived, %d new pathways (learned weights kept)\n",
			p.Name, prev.Version, p.Version, res.Memories, res.Updated, res.Archived, res.Edges)
	} else {
		fmt.Printf("Installed %s %s into %s: %d memories, %d pathways (%s)\n", p.Name, p.Version, res.Namespace, res.Memories, res.Edges, opts.Source)
	}
	if !opts.Verified {
		fmt.Println("Warning: this pack is not verified.")
	}
	fmt.Printf("Let an agent read it with: engram pair <client> --read %s\n", res.Namespace)
	return nil
}
