package sleep

import (
	"fmt"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// Sleep hours (docs/design.md, "Sleep hours"): the cycle runs only inside the
// user's inactive window, only when agents have been idle, and at most once a night.
// A scheduler (cron, launchd) calls `engram sleep run --if-due` every 15 minutes.

// Hours is a daily window in a time zone, such as 01:00-07:00 America/New_York.
// The window may cross midnight (23:00-06:00).
type Hours struct {
	Start, End time.Duration // since local midnight
	Zone       *time.Location
}

const settingHours = "sleep.hours"

// ParseHours parses "HH:MM-HH:MM" and an IANA zone name.
func ParseHours(window, zone string) (Hours, error) {
	parts := strings.Split(window, "-")
	if len(parts) != 2 {
		return Hours{}, fmt.Errorf("sleep hours look like 01:00-07:00")
	}
	parse := func(s string) (time.Duration, error) {
		t, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("%q is not a time like 01:00", s)
		}
		return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
	}
	start, err := parse(parts[0])
	if err != nil {
		return Hours{}, err
	}
	end, err := parse(parts[1])
	if err != nil {
		return Hours{}, err
	}
	if start == end {
		return Hours{}, fmt.Errorf("the window is empty")
	}
	if zone == "" {
		zone = "Local"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return Hours{}, fmt.Errorf("unknown time zone %q", zone)
	}
	return Hours{Start: start, End: end, Zone: loc}, nil
}

// String renders the window as stored.
func (h Hours) String() string {
	f := func(d time.Duration) string { return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60) }
	return f(h.Start) + "-" + f(h.End) + " " + h.Zone.String()
}

// Contains reports whether t falls inside the window.
func (h Hours) Contains(t time.Time) bool {
	local := t.In(h.Zone)
	since := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute
	if h.Start < h.End {
		return since >= h.Start && since < h.End
	}
	return since >= h.Start || since < h.End // crosses midnight
}

// SaveHours stores the user's sleep hours.
func SaveHours(st *store.Store, h Hours) error { return st.SetSetting(settingHours, h.String()) }

// LoadHours returns the stored sleep hours, or ok=false if setup has not set them.
func LoadHours(st *store.Store) (Hours, bool, error) {
	v, err := st.Setting(settingHours)
	if err != nil || v == "" {
		return Hours{}, false, err
	}
	sp := strings.SplitN(v, " ", 2)
	zone := ""
	if len(sp) == 2 {
		zone = sp[1]
	}
	h, err := ParseHours(sp[0], zone)
	return h, err == nil, err
}

// IdleFor is how long agents must have been quiet before a cycle starts.
const IdleFor = 20 * time.Minute

// Due explains whether a full cycle should run now, and why not if it should not.
func Due(st *store.Store, now time.Time) (bool, string, error) {
	h, ok, err := LoadHours(st)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "sleep hours are not set; run: engram sleep hours 01:00-07:00 --tz America/New_York", nil
	}
	if !h.Contains(now) {
		return false, "outside sleep hours (" + h.String() + ")", nil
	}
	last, err := st.LastAgentActivity()
	if err != nil {
		return false, "", err
	}
	if !last.IsZero() && now.Sub(last) < IdleFor {
		return false, fmt.Sprintf("agents were active %s ago; waiting for %s of quiet", now.Sub(last).Round(time.Minute), IdleFor), nil
	}
	runs, err := st.Runs(5)
	if err != nil {
		return false, "", err
	}
	for _, r := range runs {
		if r.Light || r.FinishedAt == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, r.FinishedAt); err == nil && now.Sub(t) < 16*time.Hour {
			return false, "already ran tonight", nil
		}
	}
	return true, "", nil
}
