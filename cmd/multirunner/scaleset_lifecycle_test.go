package main

import (
	"context"
	"errors"
	"testing"

	"github.com/GerardSmit/multirunner/internal/config"
	"github.com/GerardSmit/multirunner/internal/metrics"
	"github.com/GerardSmit/multirunner/internal/pool"
	"github.com/GerardSmit/multirunner/internal/runner"
)

func TestDefaultScaleSetModeForwardsStructuredObserverAndRepository(t *testing.T) {
	observability := metrics.New()
	hooks := observability.Hooks()
	if hooks.Observer == nil {
		t.Fatal("default metrics hooks have no structured observer")
	}
	cfg := &config.Config{GitHub: config.GitHub{Scope: config.ScopeRepo, Owner: "o", Repo: "r"}}
	p := scaleSetPool{cfg: config.Pool{Name: "linux", Size: 2}, image: "runner"}
	options := scaleSetLaunchOptions(cfg, p, hooks)
	if options.Observer != hooks.Observer || options.Repository != "o/r" || options.MaxRunners != 2 {
		t.Fatalf("default launch options lost lifecycle integration: %+v", options)
	}
	for _, scope := range []config.Scope{config.ScopeOrg, config.ScopeEnterprise} {
		cfg.GitHub.Scope = scope
		if options := scaleSetLaunchOptions(cfg, p, hooks); options.Repository != "" {
			t.Fatalf("%s registration invented a repository: %s", scope, options.Repository)
		}
	}
}

func TestScaleSetLegacyHooksRemainPoolScoped(t *testing.T) {
	started, stopped := "", ""
	expected := errors.New("failed")
	options := scaleSetLaunchOptions(&config.Config{}, scaleSetPool{cfg: config.Pool{Name: "linux"}}, pool.Hooks{
		OnStart: func(pool string) { started = pool },
		OnStop: func(pool string, code int, err error) {
			stopped = pool
			if code != -1 || !errors.Is(err, expected) {
				t.Errorf("stop = %d, %v", code, err)
			}
		},
	})
	options.OnStart()
	options.OnStop(-1, expected)
	if options.Observer != nil || started != "linux" || stopped != "linux" {
		t.Fatal("legacy hooks lost compatibility")
	}
	observer := runner.LifecycleObserverFunc(func(context.Context, runner.LifecycleEvent) {})
	options = scaleSetLaunchOptions(&config.Config{}, scaleSetPool{}, pool.Hooks{Observer: observer})
	if options.Observer == nil {
		t.Fatal("observer-only hooks were dropped")
	}
}
