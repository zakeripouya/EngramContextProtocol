package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

func TestConfigModifiers(t *testing.T) {
	c, err := store.NamedConfig("full+rrf+additive+slots3+boost10+about")
	if err != nil {
		t.Fatal(err)
	}
	if c.Fusion != "rrf" || !c.SpreadAdditive || c.PathSlots != 3 || c.SpreadBoost != 0.10 || !c.SpreadAbout || c.Name != "full+rrf+additive+slots3+boost10+about" {
		t.Fatalf("config %+v", c)
	}
	if c, _ := store.NamedConfig("no-spreading+rrf"); !c.NoSpreading || c.Fusion != "rrf" {
		t.Fatalf("modifiers apply to any base: %+v", c)
	}
	for _, bad := range []string{"full+fast", "full+slots99", "nope+rrf"} {
		if _, err := store.NamedConfig(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestPartsAndVariantsRun(t *testing.T) {
	samples, err := LoadLoCoMo(strings.NewReader(locomoSample), false)
	if err != nil {
		t.Fatal(err)
	}
	two := append(samples, samples[0])
	tune, _ := Part(two, "tune")
	report, _ := Part(two, "report")
	if len(tune) != 1 || len(report) != 1 {
		t.Fatalf("tune %d report %d", len(tune), len(report))
	}
	if _, err := Part(two, "train"); err == nil {
		t.Fatal("an unknown part should be refused")
	}
	rep, err := RunBench(context.Background(), "locomo", samples, BenchOptions{K: 3,
		Configs: []string{"full", "full+rrf", "full+additive", "full+slots1", "full+boost25", "full+additive@episodes", "full+boost25@episodes"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Systems) != 7 {
		t.Fatalf("%d systems", len(rep.Systems))
	}
	for _, s := range rep.Systems {
		if s.Questions != 3 {
			t.Errorf("%s answered %d questions", s.Name, s.Questions)
		}
	}
}
