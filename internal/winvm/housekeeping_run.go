package winvm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/GerardSmit/multirunner/internal/backgroundexec"
)

// GoldenRef ties a golden image to the bake options needed to rebuild it.
type GoldenRef struct {
	Bake BakeOptions
}

// ActionFuncs are the housekeeping side effects (injectable for testing).
type ActionFuncs struct {
	Rearm   func(ctx context.Context, ref GoldenRef, now time.Time) error
	Rebuild func(ctx context.Context, ref GoldenRef, now time.Time) error
}

// DefaultActions wires the real rearm/rebuild operations.
func DefaultActions() ActionFuncs {
	return ActionFuncs{Rearm: RearmGolden, Rebuild: RebuildGolden}
}

// Housekeeper periodically keeps golden images' eval licenses valid and tools fresh.
type Housekeeper struct {
	refs   []GoldenRef
	policy Policy
	every  time.Duration
	acts   ActionFuncs
	now    func() time.Time
	logger *slog.Logger
}

// NewHousekeeper builds a Housekeeper (every<=0 disables periodic checks).
func NewHousekeeper(refs []GoldenRef, policy Policy, every time.Duration, logger *slog.Logger) *Housekeeper {
	return &Housekeeper{
		refs: refs, policy: policy, every: every, acts: DefaultActions(),
		now: time.Now, logger: logger.With("component", "housekeeper"),
	}
}

// Run checks once immediately, then on each interval until ctx is cancelled.
func (h *Housekeeper) Run(ctx context.Context) {
	h.CheckOnce(ctx)
	if h.every <= 0 {
		return
	}
	t := time.NewTicker(h.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.CheckOnce(ctx)
		}
	}
}

// CheckOnce evaluates every golden once and applies the chosen action.
func (h *Housekeeper) CheckOnce(ctx context.Context) {
	for _, ref := range h.refs {
		meta, err := LoadMeta(ref.Bake.Golden)
		if err != nil {
			h.logger.Warn("load golden meta failed", "golden", ref.Bake.Golden, "err", err)
			continue
		}
		if meta.EvalDays == 0 {
			h.logger.Debug("golden not baked yet; skipping", "golden", ref.Bake.Golden)
			continue
		}
		switch h.policy.Decide(meta, ref.Bake.WorkflowsHash, h.now()) {
		case ActionNone:
			continue
		case ActionRearm:
			h.logger.Info("rearming golden eval license", "golden", ref.Bake.Golden, "days_left", meta.DaysLeft(h.now()))
			if err := h.acts.Rearm(ctx, ref, h.now()); err != nil {
				h.logger.Error("rearm failed", "golden", ref.Bake.Golden, "err", err)
			}
		case ActionRebuild:
			if ref.Bake.WindowsISO == "" {
				h.logger.Warn("golden needs rebuild but no bake ISO configured; run: multirunner bake", "golden", ref.Bake.Golden)
				continue
			}
			h.logger.Info("rebuilding golden", "golden", ref.Bake.Golden)
			if err := h.acts.Rebuild(ctx, ref, h.now()); err != nil {
				h.logger.Error("rebuild failed", "golden", ref.Bake.Golden, "err", err)
			}
		}
	}
}

// RearmGolden boots the golden image in place (writable, not an overlay) with a
// rearm marker ISO; the guest runs slmgr /rearm and powers off, resetting the
// eval clock. Must run while no jobs use the golden (drain first).
func RearmGolden(ctx context.Context, ref GoldenRef, now time.Time) error {
	o := ref.Bake
	o.defaults()
	iso := o.Golden + ".rearm.iso"
	if err := BuildISO(iso, "REARM", map[string]string{"rearm.txt": "1"}); err != nil {
		return err
	}
	defer os.Remove(iso)

	args := QEMUArgs(LaunchOpts{Overlay: o.Golden, JITISOPath: iso, MemMB: o.MemMB, CPUs: o.CPUs, Accel: o.Accel})
	cmd := backgroundexec.CommandContext(ctx, o.QEMUBin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rearm boot: %w", err)
	}

	meta, err := LoadMeta(o.Golden)
	if err != nil {
		return err
	}
	return SaveMeta(o.Golden, meta.ApplyRearm(now))
}

// RebuildGolden re-bakes the golden from scratch (resets eval + tool fingerprint).
func RebuildGolden(ctx context.Context, ref GoldenRef, now time.Time) error {
	return Bake(ctx, ref.Bake, now)
}
