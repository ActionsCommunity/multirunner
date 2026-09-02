package servicehost

import (
	"fmt"

	"golang.org/x/sys/windows/svc/mgr"
)

// ConfigureRecovery replaces kardianos' repeating action with a finite SCM
// schedule. An intentional service stop is not a process failure, so it does
// not consume this schedule.
func ConfigureRecovery(name string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer manager.Disconnect()

	svc, err := manager.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer svc.Close()

	return setRecoveryActions(svc, recoveryResetSeconds)
}

// ResetRecovery clears SCM's failure counter and restores the configured
// finite action list before an operator explicitly starts the service.
func ResetRecovery(name, _ string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager: %w", err)
	}
	defer manager.Disconnect()
	svc, err := manager.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service %q: %w", name, err)
	}
	defer svc.Close()
	if err := setRecoveryActions(svc, 0); err != nil {
		return fmt.Errorf("reset recovery counter: %w", err)
	}
	return setRecoveryActions(svc, recoveryResetSeconds)
}

func setRecoveryActions(svc *mgr.Service, resetPeriod uint32) error {
	actions := make([]mgr.RecoveryAction, 0, len(restartDelays)+1)
	for _, delay := range restartDelays {
		actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: delay})
	}
	actions = append(actions, mgr.RecoveryAction{Type: mgr.NoAction})
	if err := svc.SetRecoveryActions(actions, resetPeriod); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := svc.SetRecoveryActionsOnNonCrashFailures(false); err != nil {
		return fmt.Errorf("limit recovery to process failures: %w", err)
	}
	return nil
}
