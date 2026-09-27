package scaleset

import (
	"context"
	"log/slog"
	"time"
)

const defaultReconcileInterval = time.Minute

func (l *Launcher) reconcilePeriodically(ctx context.Context) {
	interval := l.opts.reconcileInterval
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconciled, err := l.Reconcile(ctx)
			if err != nil {
				if ctx.Err() == nil {
					l.opts.Logger.Error("periodic runner reconciliation failed", slog.Any("error", err))
				}
				continue
			}
			if reconciled > 0 {
				l.opts.Logger.Info("periodically reconciled runners", slog.Int("count", reconciled))
			}
		}
	}
}
