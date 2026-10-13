package playback

import (
	"errors"
	"strings"
	"testing"
)

// containsArg reports whether args contains s as a complete element.
// Used for "does this argv include --foo=bar" assertions where order
// doesn't matter.
func containsArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// containsFlagPrefix reports whether args contains any element that
// starts with prefix (e.g. "--video-sync="). Used to assert that the
// auto profile emits NO --video-sync flag at all.
func containsFlagPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// TestRegistryListsAvailableProfiles pins the profiles All() offers.
// decklink-broadcast is deliberately absent: upstream mpv has no
// decklink vo / --decklink-format, so it is marked Unavailable.
func TestRegistryListsAvailableProfiles(t *testing.T) {
	want := []string{
		"auto",
		"projector-24",
		"projector-30",
		"projector-60",
		"tv-60",
		"tv-50-pal",
	}
	got := All()
	if len(got) != len(want) {
		t.Fatalf("profile count: got %d, want %d (registry: %+v)", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("profile[%d]: got id=%q, want %q", i, got[i].ID, id)
		}
		if err := got[i].CheckAvailable(); err != nil {
			t.Errorf("All() returned unavailable profile %q: %v", id, err)
		}
	}
}

// TestDecklinkProfileUnavailable: still resolvable (legacy requests and
// stored settings get a clear error), but flagged unavailable.
func TestDecklinkProfileUnavailable(t *testing.T) {
	p, ok := ByID("decklink-broadcast")
	if !ok {
		t.Fatal("decklink-broadcast should stay resolvable via ByID")
	}
	err := p.CheckAvailable()
	if !errors.Is(err, ErrProfileUnavailable) || !strings.Contains(err.Error(), "decklink") {
		t.Fatalf("CheckAvailable() = %v, want ErrProfileUnavailable mentioning decklink", err)
	}
	if _, err := ByIDAvailable("decklink-broadcast"); !errors.Is(err, ErrProfileUnavailable) {
		t.Errorf("ByIDAvailable(decklink-broadcast) err=%v", err)
	}
	if _, err := ByIDAvailable("projector-30"); err != nil {
		t.Errorf("ByIDAvailable(projector-30) err=%v", err)
	}
	if _, err := ByIDAvailable("nope"); err == nil {
		t.Error("ByIDAvailable(unknown) should error")
	}
}

// TestDescriptionsCarryCorrectCadenceArithmetic guards against the old
// wrong figures ("1-2-1-2-1-3", "clean 2.5x" 24→60) creeping back in.
func TestDescriptionsCarryCorrectCadenceArithmetic(t *testing.T) {
	for _, p := range All() {
		for _, bad := range []string{"1-2-1-2-1-3", "2.5×", "without judder"} {
			if strings.Contains(p.Description, bad) {
				t.Errorf("profile %q description contains stale cadence claim %q", p.ID, bad)
			}
		}
	}
	p30, _ := ByID("projector-30")
	if !strings.Contains(p30.Description, "2-1-1-1") {
		t.Errorf("projector-30 should describe the 2-1-1-1 pattern: %q", p30.Description)
	}
	p60, _ := ByID("projector-60")
	if !strings.Contains(p60.Description, "3:2 pulldown") {
		t.Errorf("projector-60 should describe 3:2 pulldown: %q", p60.Description)
	}
	pal, _ := ByID("tv-50-pal")
	if !strings.Contains(pal.Description, "2-2-1") {
		t.Errorf("tv-50-pal should describe the 30→50 2-2-1 pattern: %q", pal.Description)
	}
}

// TestProfileMPVArgs is the load-bearing table-driven test: every
// profile's MPVArgs() must match the documented intent. Adding a
// profile is the only edit needed — append a row here and the test
// guards the new behavior.
func TestProfileMPVArgs(t *testing.T) {
	tests := []struct {
		name           string
		id             string
		wantArgs       []string // exact flags that must be present
		wantNoPrefixes []string // flag prefixes that must NOT appear
	}{
		{
			name: "auto emits no video-sync, no interpolation, no fps override",
			id:   "auto",
			wantArgs: []string{
				"--vo=gpu",
				"--hwdec=auto",
			},
			wantNoPrefixes: []string{
				"--video-sync=",
				"--interpolation=",
				"--display-fps-override=",
				"--decklink-format=",
			},
		},
		{
			name: "projector-24 resamples to 24, no interpolation",
			id:   "projector-24",
			wantArgs: []string{
				"--vo=gpu",
				"--hwdec=auto",
				"--video-sync=display-resample",
				"--display-fps-override=24",
			},
			wantNoPrefixes: []string{
				"--interpolation=",
			},
		},
		{
			name: "projector-30 interpolates 24->30 cadence",
			id:   "projector-30",
			wantArgs: []string{
				"--vo=gpu",
				"--hwdec=auto",
				"--video-sync=display-resample",
				"--interpolation=yes",
				"--display-fps-override=30",
			},
		},
		{
			name: "projector-60 resamples to 60 without interpolation",
			id:   "projector-60",
			wantArgs: []string{
				"--vo=gpu",
				"--hwdec=auto",
				"--video-sync=display-resample",
				"--display-fps-override=60",
			},
			wantNoPrefixes: []string{
				"--interpolation=",
			},
		},
		{
			name: "tv-60 resamples to 60, no interpolation",
			id:   "tv-60",
			wantArgs: []string{
				"--vo=gpu",
				"--hwdec=auto",
				"--video-sync=display-resample",
				"--display-fps-override=60",
			},
			wantNoPrefixes: []string{
				"--interpolation=",
			},
		},
		{
			name: "tv-50-pal interpolates for 24->50 cadence",
			id:   "tv-50-pal",
			wantArgs: []string{
				"--vo=gpu",
				"--hwdec=auto",
				"--video-sync=display-resample",
				"--interpolation=yes",
				"--display-fps-override=50",
			},
		},
		{
			name: "decklink-broadcast uses vo=decklink, hwdec=no, vdrop",
			id:   "decklink-broadcast",
			wantArgs: []string{
				"--vo=decklink",
				"--hwdec=no",
				"--video-sync=display-vdrop",
			},
			wantNoPrefixes: []string{
				"--interpolation=",
				"--display-fps-override=",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := ByID(tc.id)
			if !ok {
				t.Fatalf("ByID(%q) returned false; profile not registered", tc.id)
			}
			got := p.MPVArgs()
			for _, want := range tc.wantArgs {
				if !containsArg(got, want) {
					t.Errorf("MPVArgs() missing %q in %v", want, got)
				}
			}
			for _, prefix := range tc.wantNoPrefixes {
				if containsFlagPrefix(got, prefix) {
					t.Errorf("MPVArgs() unexpectedly contains a %s* flag: %v", prefix, got)
				}
			}
		})
	}
}

// TestProfileMPVArgsDecklinkFormat verifies the decklink-broadcast
// profile correctly threads through a DecklinkFormat the operator picks
// in the existing UI. The format is NOT baked into the profile — the
// handler clones the profile and sets DecklinkFormat from the request
// before calling MPVService.Start.
func TestProfileMPVArgsDecklinkFormat(t *testing.T) {
	p, ok := ByID("decklink-broadcast")
	if !ok {
		t.Fatalf("ByID(decklink-broadcast) returned false")
	}
	// Without DecklinkFormat: no --decklink-format flag.
	if containsFlagPrefix(p.MPVArgs(), "--decklink-format=") {
		t.Errorf("decklink-broadcast with empty DecklinkFormat should not emit --decklink-format")
	}
	// With DecklinkFormat: flag appears.
	p.DecklinkFormat = "1080p59.94"
	if !containsArg(p.MPVArgs(), "--decklink-format=1080p59.94") {
		t.Errorf("MPVArgs() should include --decklink-format=1080p59.94; got %v", p.MPVArgs())
	}
}

// TestProfileMPVArgsZeroValueProfile confirms MPVArgs() on a zero Profile
// produces sane defaults rather than panicking or emitting --vo=
// (empty). The zero value is what callers see when they forget to
// resolve, so the fallback matters.
func TestProfileMPVArgsZeroValueProfile(t *testing.T) {
	var p Profile
	got := p.MPVArgs()
	if !containsArg(got, "--vo=gpu") {
		t.Errorf("zero Profile MPVArgs() must default --vo=gpu; got %v", got)
	}
	if !containsArg(got, "--hwdec=auto") {
		t.Errorf("zero Profile MPVArgs() must default --hwdec=auto; got %v", got)
	}
}

// TestByIDUnknownReturnsFalse — the contract requires callers to check
// the bool; a missing profile must not silently return Default().
func TestByIDUnknownReturnsFalse(t *testing.T) {
	if p, ok := ByID("doesntexist"); ok {
		t.Errorf("ByID(doesntexist) returned ok=true (profile=%+v)", p)
	}
	if p, ok := ByID(""); ok {
		t.Errorf("ByID(\"\") returned ok=true (profile=%+v)", p)
	}
}

// TestDefaultIsAuto verifies the default profile is "auto" — the
// safety fallback that leaves mpv to its prior defaults.
func TestDefaultIsAuto(t *testing.T) {
	d := Default()
	if d.ID != "auto" {
		t.Errorf("Default().ID = %q, want %q", d.ID, "auto")
	}
	if d.ID != DefaultID {
		t.Errorf("Default().ID = %q != DefaultID = %q", d.ID, DefaultID)
	}
}

// TestAllReturnsCopy confirms All() doesn't expose the underlying
// registry — mutations to the returned slice must not leak.
func TestAllReturnsCopy(t *testing.T) {
	a := All()
	if len(a) == 0 {
		t.Fatal("All() returned empty")
	}
	originalID := a[0].ID
	a[0].ID = "MUTATED"
	b := All()
	if b[0].ID != originalID {
		t.Errorf("All() leaked package state: second call returned %q after mutation", b[0].ID)
	}
}

// TestValidatePassesOnShippedRegistry — the catalog the binary ships
// must validate at boot. If a new profile is added with a typo, this
// fails before the binary ever starts answering requests.
func TestValidatePassesOnShippedRegistry(t *testing.T) {
	if err := Validate(); err != nil {
		t.Errorf("Validate() on shipped registry: %v", err)
	}
}

// TestFormatFPSIntegerAndFractional pins the override formatter — mpv
// accepts both forms, but logs are easier to read when integer rates
// render as "30" not "30.000". A fractional rate (e.g. 23.976, were we
// to add a 23.976 override later) renders to 3 decimals.
func TestFormatFPSIntegerAndFractional(t *testing.T) {
	if got := formatFPS(30); got != "30" {
		t.Errorf("formatFPS(30) = %q, want %q", got, "30")
	}
	if got := formatFPS(60); got != "60" {
		t.Errorf("formatFPS(60) = %q, want %q", got, "60")
	}
	if got := formatFPS(23.976); got != "23.976" {
		t.Errorf("formatFPS(23.976) = %q, want %q", got, "23.976")
	}
}
