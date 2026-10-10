package pool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLauncherPauseResumeAndDrain(t *testing.T) {
	launcher := controlTestLauncher()
	launcher.Pause()
	if !launcher.Paused() {
		t.Fatal("launcher is not paused")
	}
	waited := make(chan error, 1)
	go func() {
		waited <- launcher.waitUntilProvisioningAllowed(t.Context())
	}()
	select {
	case err := <-waited:
		t.Fatalf("paused provisioning returned early: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	launcher.Resume()
	if err := <-waited; err != nil {
		t.Fatal(err)
	}

	active := &activeRunner{cancel: func() {}, done: make(chan struct{})}
	launcher.controlMu.Lock()
	launcher.active["session"] = active
	launcher.controlMu.Unlock()
	drained := make(chan error, 1)
	go func() {
		drained <- launcher.Drain(t.Context())
	}()
	select {
	case err := <-drained:
		t.Fatalf("drain returned with active session: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	launcher.controlMu.Lock()
	delete(launcher.active, "session")
	close(active.done)
	launcher.notifyControlChangeLocked()
	launcher.controlMu.Unlock()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if !launcher.Paused() {
		t.Fatal("drain did not preserve paused state")
	}
}

func TestLauncherTerminateUsesOwnedSessionCancellation(t *testing.T) {
	launcher := controlTestLauncher()
	runContext, cancel := context.WithCancel(t.Context())
	active := &activeRunner{cancel: cancel, done: make(chan struct{})}
	launcher.active["session"] = active
	go func() {
		<-runContext.Done()
		launcher.controlMu.Lock()
		delete(launcher.active, "session")
		close(active.done)
		launcher.notifyControlChangeLocked()
		launcher.controlMu.Unlock()
	}()
	if err := launcher.Terminate(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := launcher.Terminate(t.Context(), "missing"); !errors.Is(err, ErrRunnerSessionNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
}

func TestLauncherDrainWaitsAcrossProvisioningReservation(t *testing.T) {
	launcher := controlTestLauncher()
	if err := launcher.reserveProvisioning(t.Context()); err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() {
		drained <- launcher.Drain(t.Context())
	}()
	select {
	case err := <-drained:
		t.Fatalf("drain returned with provisioning reservation: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	active := &activeRunner{cancel: func() {}, done: make(chan struct{})}
	launcher.controlMu.Lock()
	launcher.active["session"] = active
	launcher.provisioning--
	launcher.notifyControlChangeLocked()
	launcher.controlMu.Unlock()
	select {
	case err := <-drained:
		t.Fatalf("drain returned before reserved runner completed: %v", err)
	case <-time.After(10 * time.Millisecond):
	}

	launcher.controlMu.Lock()
	delete(launcher.active, "session")
	close(active.done)
	launcher.notifyControlChangeLocked()
	launcher.controlMu.Unlock()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
}

func controlTestLauncher() *Launcher {
	return &Launcher{
		active:  make(map[string]*activeRunner),
		changed: make(chan struct{}),
	}
}
