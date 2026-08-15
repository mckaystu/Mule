//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName        = "DominoMule"
	windowsServiceDisplayName = "Domino Mule"
	windowsServiceDescription = "HCL Domino StatPub and Keep metrics sidecar (OTLP)"
)

func runningAsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

func serviceCommand(opt options) (handled bool, code int) {
	cmd := strings.ToLower(strings.TrimSpace(opt.service))
	if cmd == "" {
		return false, 0
	}

	var err error
	switch cmd {
	case "install":
		err = installWindowsService(opt)
	case "uninstall", "remove":
		err = uninstallWindowsService()
	case "start":
		err = startWindowsService()
	case "stop":
		err = stopWindowsService()
	default:
		fmt.Fprintf(os.Stderr, "unknown -service %q (use install, uninstall, start, or stop)\n", opt.service)
		return true, 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return true, 1
	}
	return true, 0
}

func runWindowsService(opt options) int {
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}
	err := svc.Run(windowsServiceName, &windowsService{opt: opt})
	if err != nil {
		fmt.Fprintln(os.Stderr, "windows service:", err)
		return 1
	}
	return 0
}

type windowsService struct {
	opt options
}

func (w *windowsService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	elog, elogErr := eventlog.Open(windowsServiceName)
	if elogErr == nil {
		defer elog.Close()
		_ = elog.Info(1, "Domino Mule starting")
	}

	log, closer, err := openLogger(w.opt, true)
	if err != nil {
		if elog != nil {
			_ = elog.Error(2, "open log: "+err.Error())
		}
		changes <- svc.Status{State: svc.Stopped}
		return true, 1
	}
	defer closer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)
	go func() { done <- runForever(ctx, w.opt, log) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				code := <-done
				if elog != nil {
					_ = elog.Info(1, "Domino Mule stopped")
				}
				if code != 0 {
					return true, uint32(code)
				}
				return false, 0
			}
		case code := <-done:
			if elog != nil && code != 0 {
				_ = elog.Error(2, "Domino Mule exited with a non-zero status")
			}
			if code != 0 {
				return true, uint32(code)
			}
			return false, 0
		}
	}
}

func installWindowsService(opt options) error {
	if opt.dryRun {
		return fmt.Errorf("refusing to install a Windows service with --dry-run")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}

	configPath, err := filepath.Abs(opt.configPath)
	if err != nil {
		return fmt.Errorf("resolve -config: %w", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		return fmt.Errorf("config file %s: %w", configPath, err)
	}

	logPath := opt.logPath
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(exe), "mule.log")
	}
	logPath, err = filepath.Abs(logPath)
	if err != nil {
		return fmt.Errorf("resolve -log: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run this as Administrator): %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(windowsServiceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s is already installed", windowsServiceName)
	}

	args := []string{"-config", configPath, "-log", logPath}
	if opt.verbose {
		args = append(args, "-v")
	}

	s, err := m.CreateService(windowsServiceName, exe, mgr.Config{
		DisplayName:  windowsServiceDisplayName,
		Description:  windowsServiceDescription,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	}, args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, uint32((24 * time.Hour).Seconds()))

	err = eventlog.InstallAsEventCreate(windowsServiceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		_ = s.Delete()
		return fmt.Errorf("event log source: %w", err)
	}

	fmt.Printf("Installed Windows service %s\n", windowsServiceName)
	fmt.Printf("  exe:    %s\n", exe)
	fmt.Printf("  config: %s\n", configPath)
	fmt.Printf("  log:    %s\n", logPath)
	fmt.Printf("Start with: %s -service start\n", filepath.Base(exe))
	return nil
}

func uninstallWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run this as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", windowsServiceName)
	}
	defer s.Close()

	_, _ = s.Control(svc.Stop)
	if err := waitForStop(s, 20*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	_ = eventlog.Remove(windowsServiceName)
	fmt.Printf("Removed Windows service %s\n", windowsServiceName)
	return nil
}

func startWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run this as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", windowsServiceName)
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Running {
		fmt.Printf("Service %s is already running\n", windowsServiceName)
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	fmt.Printf("Started Windows service %s\n", windowsServiceName)
	return nil
}

func stopWindowsService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run this as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", windowsServiceName)
	}
	defer s.Close()

	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		fmt.Printf("Service %s is already stopped\n", windowsServiceName)
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if err := waitForStop(s, 20*time.Second); err != nil {
		return err
	}
	fmt.Printf("Stopped Windows service %s\n", windowsServiceName)
	return nil
}

func waitForStop(s *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s to stop", windowsServiceName)
}
