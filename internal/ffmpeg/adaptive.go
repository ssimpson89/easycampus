package ffmpeg

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PresetTier resolves the next-lower / next-higher QualityPreset for a
// given preset ID. The parallel broadcast engineer's tier matrix will
// provide a concrete implementation that knows the consumer/prosumer/
// broadcast/cinema ordering. easycampus-go ships a no-op
// implementation (StaticTier) used until the table lands.
//
// Methods:
//   - Lower(currentID) returns the next preset down, or (zero, false) if
//     already at the floor.
//   - Higher(currentID, targetID) returns the next preset up, capped at
//     the user's target (we never exceed what they chose).
type PresetTier interface {
	Lower(currentID string) (QualityPreset, bool)
	Higher(currentID, targetID string) (QualityPreset, bool)
}

// StaticTier is a PresetTier that knows about exactly the preset it was
// constructed with — there's nothing to step down to. Used as a safe
// default until the broadcast engineer's tier table is wired in.
type StaticTier struct{}

func (StaticTier) Lower(string) (QualityPreset, bool)          { return nil, false }
func (StaticTier) Higher(string, string) (QualityPreset, bool) { return nil, false }

// AdaptiveConfig controls automatic quality fallback. Intentionally
// conservative — must sustain bad metrics across the full window before
// triggering, and waits longer for recovery than for degradation.
type AdaptiveConfig struct {
	Enabled           bool
	DropRateThreshold float64       // 0.03 = 3% of frames dropped
	DropWindow        time.Duration // must sustain for this long
	SpeedThreshold    float64       // 0.92 = sustained below 92% real-time = network congestion
	StartupGrace      time.Duration
	DowngradeCooldown time.Duration
	StableForRecovery time.Duration
	MaxDowngrades     int
	RestartRateWindow time.Duration
	RestartRateMax    int
}

// DefaultAdaptiveConfig returns conservative defaults that match industry
// practice (similar to OBS dynamic bitrate but slower to react).
func DefaultAdaptiveConfig() AdaptiveConfig {
	return AdaptiveConfig{
		Enabled:           true,
		DropRateThreshold: 0.03,
		DropWindow:        60 * time.Second,
		SpeedThreshold:    0.92,
		StartupGrace:      60 * time.Second,
		DowngradeCooldown: 5 * time.Minute,
		StableForRecovery: 10 * time.Minute,
		MaxDowngrades:     3,
		RestartRateWindow: 5 * time.Minute,
		RestartRateMax:    3,
	}
}

// AdaptiveState is the controller's externally visible status.
type AdaptiveState struct {
	Enabled        bool      `json:"enabled"`
	OriginalPreset string    `json:"originalPreset,omitempty"`
	ActivePreset   string    `json:"activePreset,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	DowngradeCount int       `json:"downgradeCount"`
	LastTransition time.Time `json:"lastTransition,omitempty"`
	IsFallback     bool      `json:"isFallback"`
}

// AdaptiveController watches a Supervisor's progress and adjusts quality
// by restarting FFmpeg at a different preset tier.
//
// TODO(broadcast-engineer): once the consumer/prosumer/broadcast/cinema
// QualityPreset implementations land, replace the StaticTier passed into
// NewAdaptiveController with the real tier-walking implementation. No
// other wiring changes should be needed — the controller already calls
// PresetTier.Lower/Higher and restarts the supervisor at the new preset.
type AdaptiveController struct {
	cfg    AdaptiveConfig
	logger *log.Logger
	sup    *Supervisor
	tier   PresetTier

	mu sync.Mutex

	targetPreset     string
	currentPreset    string
	streamStartedAt  time.Time
	dropSamples      []dropSample
	lastTransitionAt time.Time
	restartTimes     []time.Time
	downgradeCount   int
	reason           string

	cancel chan struct{}
	done   chan struct{}
}

type dropSample struct {
	at      time.Time
	frame   int
	dropped int
	speed   float64
}

// NewAdaptiveController creates a controller for the given supervisor.
// Pass StaticTier{} for `tier` until the broadcast engineer's tier matrix
// is available.
func NewAdaptiveController(sup *Supervisor, tier PresetTier, cfg AdaptiveConfig, logger *log.Logger) *AdaptiveController {
	if logger == nil {
		logger = log.Default()
	}
	if tier == nil {
		tier = StaticTier{}
	}
	if cfg.DropWindow <= 0 {
		cfg = DefaultAdaptiveConfig()
	}
	return &AdaptiveController{cfg: cfg, sup: sup, tier: tier, logger: logger}
}

func (a *AdaptiveController) SetEnabled(enabled bool) {
	a.mu.Lock()
	a.cfg.Enabled = enabled
	a.mu.Unlock()
}

// OnStreamStart records the user-selected target and resets state.
func (a *AdaptiveController) OnStreamStart(presetID string) {
	a.mu.Lock()
	a.targetPreset = presetID
	a.currentPreset = presetID
	a.streamStartedAt = time.Now()
	a.dropSamples = a.dropSamples[:0]
	a.lastTransitionAt = time.Time{}
	a.restartTimes = a.restartTimes[:0]
	a.downgradeCount = 0
	a.reason = ""
	a.mu.Unlock()
}

func (a *AdaptiveController) OnStreamStop() {
	a.mu.Lock()
	a.targetPreset = ""
	a.currentPreset = ""
	a.mu.Unlock()
}

// OnRestart records that FFmpeg restarted (used to detect restart storms).
func (a *AdaptiveController) OnRestart() {
	a.mu.Lock()
	now := time.Now()
	a.restartTimes = append(a.restartTimes, now)
	cutoff := now.Add(-a.cfg.RestartRateWindow)
	idx := 0
	for i, t := range a.restartTimes {
		if t.After(cutoff) {
			idx = i
			break
		}
	}
	a.restartTimes = a.restartTimes[idx:]
	a.mu.Unlock()
}

func (a *AdaptiveController) Start() {
	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		return
	}
	a.cancel = make(chan struct{})
	a.done = make(chan struct{})
	a.mu.Unlock()
	go a.run()
}

func (a *AdaptiveController) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	done := a.done
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		close(cancel)
	}
	if done != nil {
		<-done
	}
}

func (a *AdaptiveController) State() AdaptiveState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AdaptiveState{
		Enabled:        a.cfg.Enabled,
		OriginalPreset: a.targetPreset,
		ActivePreset:   a.currentPreset,
		Reason:         a.reason,
		DowngradeCount: a.downgradeCount,
		LastTransition: a.lastTransitionAt,
		IsFallback:     a.currentPreset != "" && a.currentPreset != a.targetPreset,
	}
}

func (a *AdaptiveController) run() {
	defer func() {
		a.mu.Lock()
		done := a.done
		a.done = nil
		a.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		a.mu.Lock()
		cancel := a.cancel
		a.mu.Unlock()
		if cancel == nil {
			return
		}
		select {
		case <-cancel:
			return
		case <-ticker.C:
			a.tick()
		}
	}
}

func (a *AdaptiveController) tick() {
	a.mu.Lock()
	if !a.cfg.Enabled || a.targetPreset == "" || a.currentPreset == "" {
		a.mu.Unlock()
		return
	}
	if time.Since(a.streamStartedAt) < a.cfg.StartupGrace {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	// Capture the stream generation before deciding, so a transition
	// computed against this stream can't land on a later one.
	gen := a.sup.Generation()
	status := a.sup.Status()
	if status.State != StateRunning && status.State != StateDegraded && status.State != StateRestarting {
		return
	}
	a.sampleProgress(status.LastProgress)

	dropping := a.isPersistentDrop()
	slow := a.isPersistentlySlow()
	stalling := status.State == StateDegraded
	restartStorm := a.hasRestartStorm()

	if dropping || slow || stalling || restartStorm {
		reason := ""
		switch {
		case stalling:
			reason = "FFmpeg progress stalled (network or encoder issue)"
		case restartStorm:
			reason = "Stream restarted repeatedly — network unstable"
		case dropping:
			reason = "Sustained dropped frames detected"
		case slow:
			reason = "Encoder falling behind real-time (upload bandwidth limited)"
		}
		a.handleDegradation(reason, gen)
		return
	}
	a.maybeRecover(gen)
}

func (a *AdaptiveController) sampleProgress(p Progress) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.UpdatedAt.IsZero() {
		return
	}
	if n := len(a.dropSamples); n > 0 && a.dropSamples[n-1].at.Equal(p.UpdatedAt) {
		return
	}
	a.dropSamples = append(a.dropSamples, dropSample{
		at:      p.UpdatedAt,
		frame:   p.Frame,
		dropped: p.Dropped,
		speed:   parseSpeed(p.Speed),
	})
	cutoff := time.Now().Add(-a.cfg.DropWindow)
	idx := 0
	for i, s := range a.dropSamples {
		if s.at.After(cutoff) {
			idx = i
			break
		}
	}
	a.dropSamples = a.dropSamples[idx:]
}

func (a *AdaptiveController) isPersistentlySlow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.dropSamples) < 4 {
		return false
	}
	first := a.dropSamples[0]
	last := a.dropSamples[len(a.dropSamples)-1]
	if last.at.Sub(first.at) < a.cfg.DropWindow-(5*time.Second) {
		return false
	}
	for _, s := range a.dropSamples {
		if s.speed <= 0 || s.speed >= a.cfg.SpeedThreshold {
			return false
		}
	}
	return true
}

func parseSpeed(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(s, "x"))
	if s == "" || s == "N/A" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func (a *AdaptiveController) isPersistentDrop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.dropSamples) < 2 {
		return false
	}
	first := a.dropSamples[0]
	last := a.dropSamples[len(a.dropSamples)-1]
	if last.at.Sub(first.at) < a.cfg.DropWindow-(5*time.Second) {
		return false
	}
	frames := last.frame - first.frame
	dropped := last.dropped - first.dropped
	if frames <= 0 {
		return false
	}
	rate := float64(dropped) / float64(frames+dropped)
	return rate >= a.cfg.DropRateThreshold
}

func (a *AdaptiveController) hasRestartStorm() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-a.cfg.RestartRateWindow)
	count := 0
	for _, t := range a.restartTimes {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= a.cfg.RestartRateMax
}

func (a *AdaptiveController) handleDegradation(reason string, gen uint64) {
	a.mu.Lock()
	if !a.lastTransitionAt.IsZero() && time.Since(a.lastTransitionAt) < a.cfg.DowngradeCooldown {
		a.mu.Unlock()
		return
	}
	if a.downgradeCount >= a.cfg.MaxDowngrades {
		a.mu.Unlock()
		return
	}
	current := a.currentPreset
	tier := a.tier
	a.mu.Unlock()

	next, ok := tier.Lower(current)
	if !ok {
		return
	}
	a.logger.Printf("adaptive: degradation detected (%s). Stepping down %s -> %s", reason, current, next.ID())
	if err := a.transitionTo(next, reason, true, gen); err != nil {
		a.logger.Printf("adaptive: failed to step down: %v", err)
	}
}

func (a *AdaptiveController) maybeRecover(gen uint64) {
	a.mu.Lock()
	if a.currentPreset == a.targetPreset {
		a.mu.Unlock()
		return
	}
	if a.lastTransitionAt.IsZero() || time.Since(a.lastTransitionAt) < a.cfg.StableForRecovery {
		a.mu.Unlock()
		return
	}
	current := a.currentPreset
	target := a.targetPreset
	tier := a.tier
	a.mu.Unlock()

	next, ok := tier.Higher(current, target)
	if !ok {
		return
	}
	a.logger.Printf("adaptive: stable for %s. Stepping back up %s -> %s", a.cfg.StableForRecovery, current, next.ID())
	if err := a.transitionTo(next, "stable conditions — restoring quality", false, gen); err != nil {
		a.logger.Printf("adaptive: failed to step up: %v", err)
	}
}

// transitionTo swaps the running stream onto `preset`. It goes through
// Supervisor.SwapPresetIfGeneration (config update + in-place restart
// under the supervisor lock) rather than Stop+Start, so it can't
// resurrect a stream the operator just stopped, keeps the real event ID,
// and the restart appends to the live playlist instead of wiping it.
func (a *AdaptiveController) transitionTo(preset QualityPreset, reason string, isDowngrade bool, gen uint64) error {
	if err := a.sup.SwapPresetIfGeneration(preset, "adaptive: "+reason, gen); err != nil {
		return err
	}
	a.mu.Lock()
	a.currentPreset = preset.ID()
	a.lastTransitionAt = time.Now()
	a.streamStartedAt = time.Now()
	a.dropSamples = a.dropSamples[:0]
	a.reason = reason
	if isDowngrade {
		a.downgradeCount++
	}
	a.mu.Unlock()
	return nil
}
