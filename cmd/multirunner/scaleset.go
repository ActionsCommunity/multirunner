package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GerardSmit/multirunner/internal/backend"
	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/metrics"
	"github.com/GerardSmit/multirunner/internal/pool"
	scalesetmode "github.com/GerardSmit/multirunner/internal/scaleset"
)

// scaleSetPool is everything the scale-set mode needs to launch runners for one
// pool. It deliberately skips pool.Launcher, because in this mode the JIT
// config comes from the scale-set session rather than generate-jitconfig.
type scaleSetPool struct {
	cfg    config.Pool
	be     backend.Backend
	image  string
	env    map[string]string
	mounts []backend.Mount
}

type scaleSetStateReporter func(string, scalesetmode.SessionAvailability)

func newScaleSetHealthReporter(m *metrics.Metrics, pools []config.Pool) scaleSetStateReporter {
	for _, p := range pools {
		// Config validation makes pool names stable and unique within this
		// orchestrator, including when several sessions run concurrently.
		m.SetRequiredSessionAvailable(p.Name, false)
	}
	return func(sessionKey string, state scalesetmode.SessionAvailability) {
		m.SetRequiredSessionAvailable(sessionKey, state == scalesetmode.SessionAvailable)
	}
}

// runScaleset holds one long-poll session per pool and provisions runners as
// GitHub reports demand. Each pool has its own scale set, because a scale set
// carries one label set and therefore one runner OS.
func runScaleset(
	ctx context.Context,
	cfg *config.Config,
	pools []scaleSetPool,
	hooks pool.Hooks,
	reportState scaleSetStateReporter,
	logger *slog.Logger,
) error {
	target, err := scalesetmode.TargetURL(cfg.GitHub.URL, string(cfg.GitHub.Scope), cfg.GitHub.Owner, cfg.GitHub.Repo)
	if err != nil {
		return err
	}

	clientOpts := scalesetmode.ClientOptions{
		TargetURL:      target,
		PAT:            cfg.Auth.PAT,
		AppID:          cfg.Auth.AppID,
		InstallationID: cfg.Auth.InstallationID,
		PrivateKeyPath: cfg.Auth.PrivateKeyPath,
	}

	sessions := make([]scalesetmode.SupervisedSession, 0, len(pools))
	for _, p := range pools {
		p := p
		sessionKey := p.cfg.Name
		client, err := scalesetmode.NewClient(clientOpts)
		if err != nil {
			return fmt.Errorf("pool %s: %w", p.cfg.Name, err)
		}
		sessionOptions := scalesetmode.SessionOptions{
			Name:        p.cfg.ScaleSet,
			RunnerGroup: p.cfg.RunnerGroup,
			Labels:      p.cfg.Labels,
			Target:      target,
			Pool:        p.cfg.Name,
			Launch: scalesetmode.Options{
				Image:      p.image,
				WorkFolder: p.cfg.WorkFolder,
				Labels:     p.cfg.Labels,
				Env:        p.env,
				Mounts:     p.mounts,
				MaxRunners: p.cfg.Size,
				OnStart: func() {
					if hooks.OnStart != nil {
						hooks.OnStart(p.cfg.Name)
					}
				},
				OnStop: func(code int, err error) {
					if hooks.OnStop != nil {
						hooks.OnStop(p.cfg.Name, code, err)
					}
				},
			},
		}
		sessions = append(sessions, scalesetmode.SupervisedSession{
			Name: p.cfg.ScaleSet,
			Run: func(runCtx context.Context, available func()) error {
				runOptions := sessionOptions
				runOptions.OnStateChange = func(state scalesetmode.SessionAvailability) {
					reportState(sessionKey, state)
					if state == scalesetmode.SessionAvailable {
						available()
					}
				}
				return scalesetmode.Run(runCtx, client, p.be, runOptions, logger.With("pool", p.cfg.Name))
			},
			OnStateChange: func(state scalesetmode.SessionAvailability) {
				reportState(sessionKey, state)
			},
		})
	}
	return scalesetmode.RunSupervised(ctx, sessions, logger)
}
