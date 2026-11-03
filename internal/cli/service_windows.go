package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	// adminOnlyDACL gives SYSTEM and Administrators full control of a directory and what it holds,
	// and no one else any access; P blocks the inherited entries, OICI passes these down.
	adminOnlyDACL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
)

// serviceDirs returns where the service's binary and data live. The service runs as LocalSystem,
// so both must be writable by SYSTEM and Administrators only: a standard user who could replace
// the binary, or link the data directory elsewhere, would run code or write files as SYSTEM.
func serviceDirs() (program, data string, err error) {
	pf, pd := os.Getenv("ProgramFiles"), os.Getenv("ProgramData")
	if pf == "" || pd == "" {
		return "", "", errors.New("ProgramFiles or ProgramData is not set")
	}
	return filepath.Join(pf, "perfwatch"), filepath.Join(pd, "perfwatch"), nil
}

func installService(streams Streams, exe string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("install --service needs an elevated prompt: run it as administrator")
	}
	programDir, data, err := serviceDirs()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	// Running, the service holds its binary open: stop it before replacing that.
	s, err := m.OpenService(serviceName())
	if err == nil {
		defer s.Close()
		if err := stopService(s); err != nil {
			return err
		}
	}
	for _, dir := range []string{programDir, data} {
		if err := makeAdminDir(dir); err != nil {
			return err
		}
	}
	target := filepath.Join(programDir, "perfwatch.exe")
	if !strings.EqualFold(exe, target) {
		if err := copyFile(exe, target); err != nil {
			return fmt.Errorf("copy perfwatch to %s: %w", programDir, err)
		}
	}
	if s == nil {
		s, err = m.CreateService(serviceName(), target, mgr.Config{
			DisplayName: "perfwatch",
			Description: "Records CPU and RAM usage and the processes behind each peak.",
			StartType:   mgr.StartAutomatic,
		}, "run", "--data-dir", data)
		if err != nil {
			return fmt.Errorf("create the service: %w", err)
		}
		defer s.Close()
		fmt.Fprintf(streams.Stdout, "Service %s installed: starts at boot, runs %s, data in %s.\n", serviceName(), target, data)
	} else {
		// An earlier install may have named another binary or directory: point it at these.
		c, err := s.Config()
		if err != nil {
			return fmt.Errorf("read the service: %w", err)
		}
		c.BinaryPathName = commandLine(target, []string{"run", "--data-dir", data})
		c.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(c); err != nil {
			return fmt.Errorf("update the service: %w", err)
		}
		fmt.Fprintf(streams.Stdout, "Service %s updated: runs %s, data in %s.\n", serviceName(), target, data)
	}

	// One way to start is enough: the service replaces the logon entry and its collector.
	command, removed, err := removeAutostart()
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintln(streams.Stdout, "Removed the logon autostart entry; the service starts the collector now.")
	}
	userDir, err := registeredDataDir(command)
	if err != nil {
		return err
	}
	if err := stopCollector(io.Discard, userDir); err != nil {
		return err
	}

	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start the service: %w", err)
	}
	// Start returns once the process reaches the service manager, before the collector is up.
	var st svc.Status
	if !waitFor(func() bool {
		st, err = s.Query()
		return err != nil || st.State == svc.Running || st.State == svc.Stopped
	}) || err != nil || st.State != svc.Running {
		return fmt.Errorf("the service did not start (state %d, exit code %d, %v); see %s",
			st.State, st.ServiceSpecificExitCode, err, filepath.Join(data, logFileName))
	}
	fmt.Fprintln(streams.Stdout, "Service started.")
	return nil
}

// makeAdminDir creates dir, or takes over the one there, with only SYSTEM and Administrators
// allowed in. Any user may create a folder in %ProgramData%, so a folder found there must not be
// a link and must already belong to SYSTEM or Administrators, or a user could have planted it.
func makeAdminDir(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("check %s: %w", dir, err)
	case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		return fmt.Errorf("%s is a link or a file, not a folder; remove it and run install --service again", dir)
	default:
		sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the owner of %s: %w", dir, err)
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return fmt.Errorf("read the owner of %s: %w", dir, err)
		}
		if !owner.IsWellKnown(windows.WinLocalSystemSid) && !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return fmt.Errorf("%s belongs to %s, not to SYSTEM or Administrators; remove it and run install --service again", dir, owner)
		}
	}
	sd, err := windows.SecurityDescriptorFromString(adminOnlyDACL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		admins, nil, dacl, nil); err != nil {
		return fmt.Errorf("restrict %s to administrators: %w", dir, err)
	}
	return nil
}

// copyFile writes src to dst through a temporary file beside it, so dst is never half written.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// serviceInstalled reports whether the service exists; asking needs no admin.
func serviceInstalled() (bool, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, fmt.Errorf("connect to the service manager: %w", err)
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(serviceName())
	if err != nil {
		return false, err
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up the service: %w", err)
	}
	windows.CloseServiceHandle(h)
	return true, nil
}

// stopService stops s and waits until it has.
func stopService(s *mgr.Service) error {
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop the service: %w", err)
	}
	if !waitFor(func() bool {
		st, err := s.Query()
		return err == nil && st.State == svc.Stopped
	}) {
		return fmt.Errorf("the service did not stop within %s", collectorWait)
	}
	return nil
}

// uninstallService stops and deletes the service and its binary when one is installed; that
// needs admin. The service's data stays, like the logon collector's.
func uninstallService(streams Streams) error {
	installed, err := serviceInstalled()
	if err != nil || !installed {
		return err
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("perfwatch is installed as a service: run uninstall from an elevated prompt")
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName())
	if err != nil {
		return fmt.Errorf("open the service: %w", err)
	}
	defer s.Close()
	if err := stopService(s); err != nil {
		return err
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete the service: %w", err)
	}
	fmt.Fprintf(streams.Stdout, "Removed service %s.\n", serviceName())
	programDir, data, err := serviceDirs()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(programDir); err != nil {
		fmt.Fprintf(streams.Stdout, "Could not remove %s: %v\n", programDir, err)
	}
	fmt.Fprintf(streams.Stdout, "Service data kept in %s.\n", data)
	return nil
}
