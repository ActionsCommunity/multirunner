//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type darwinOutputWatcher struct {
	kqueue int
}

func newDarwinOutputWatcher(output *rotatingOutput) (*darwinOutputWatcher, error) {
	kqueue, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("create output event queue: %w", err)
	}
	unix.CloseOnExec(kqueue)
	watcher := &darwinOutputWatcher{kqueue: kqueue}
	if err := watcher.Rebind(output); err != nil {
		watcher.Close()
		return nil, err
	}
	return watcher, nil
}

func (watcher *darwinOutputWatcher) Rebind(output *rotatingOutput) error {
	descriptor, err := darwinOutputDescriptor(output)
	if err != nil {
		return err
	}
	change := unix.Kevent_t{
		Ident:  uint64(descriptor),
		Filter: unix.EVFILT_VNODE,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
		Fflags: unix.NOTE_EXTEND,
	}
	if _, err := unix.Kevent(watcher.kqueue, []unix.Kevent_t{change}, nil, nil); err != nil {
		return fmt.Errorf("register output growth event: %w", err)
	}
	return nil
}

func (watcher *darwinOutputWatcher) Wait(stop <-chan struct{}) (bool, error) {
	select {
	case <-stop:
		return false, nil
	default:
	}
	events := make([]unix.Kevent_t, 1)
	timeout := unix.NsecToTimespec(darwinRotationCheck.Nanoseconds())
	count, err := unix.Kevent(watcher.kqueue, nil, events, &timeout)
	if errors.Is(err, unix.EINTR) {
		return false, nil
	}
	return count > 0, err
}

func (watcher *darwinOutputWatcher) Close() {
	if watcher.kqueue >= 0 {
		_ = unix.Close(watcher.kqueue)
		watcher.kqueue = -1
	}
}

func darwinOutputDescriptor(output *rotatingOutput) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.closed || output.file == nil {
		return 0, os.ErrClosed
	}
	return int(output.file.Fd()), nil
}
