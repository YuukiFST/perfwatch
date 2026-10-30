//go:build windows

package sampler

import (
	"errors"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// groupRetryInterval spaces re-enumerations for a svchost.exe whose service has not reported its PID.
	groupRetryInterval = 10 * time.Second
	// maxGroupAttempts bounds those retries; after them the instance keeps an empty group.
	maxGroupAttempts = 6
)

type instanceKey struct {
	pid        uint32
	createTime int64
}

type identity struct {
	name          string
	path          string
	serviceGroup  string
	groupResolved bool
	groupAttempts int
	pass          uint64
}

// identityCache resolves name, path and svchost group once per process instance and
// forgets instances that are gone, so it stays as large as the process list.
type identityCache struct {
	byInstance map[instanceKey]*identity
	services   serviceGroups
	pass       uint64
	passTime   time.Time
	lastCount  int
}

func newIdentityCache() *identityCache {
	return &identityCache{byInstance: map[instanceKey]*identity{}, services: serviceGroups{configGroup: map[string]string{}}}
}

func (c *identityCache) beginPass(now time.Time) {
	c.pass++
	c.passTime = now
}

func (c *identityCache) endPass(count int) {
	c.lastCount = count
	for key, id := range c.byInstance {
		if id.pass != c.pass {
			delete(c.byInstance, key)
		}
	}
}

// lookup returns the instance's identity; imageName points into the buffer of the current pass.
func (c *identityCache) lookup(pid uint32, createTime int64, imageName *windows.NTUnicodeString) *identity {
	key := instanceKey{pid: pid, createTime: createTime}
	id, seen := c.byInstance[key]
	if !seen {
		id = &identity{name: imageName.String(), path: imagePath(pid, createTime)}
		id.groupResolved = !strings.EqualFold(id.name, "svchost.exe")
		c.byInstance[key] = id
	}
	if !id.groupResolved {
		id.serviceGroup, id.groupResolved = c.services.lookup(key, !seen, c.passTime)
		id.groupAttempts++
		if id.groupAttempts >= maxGroupAttempts {
			id.groupResolved = true
		}
	}
	id.pass = c.pass
	return id
}

// imagePath returns the Win32 path of the process image, or "" when the process denies access
// or the PID now belongs to a process other than the instance created at createTime.
func imagePath(pid uint32, createTime int64) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil || filetimeTicks(created) != uint64(createTime) {
		return ""
	}
	for _, capacity := range []uint32{windows.MAX_PATH, windows.MAX_LONG_PATH} {
		buf := make([]uint16, capacity)
		size := capacity
		err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size)
		if err == nil {
			return windows.UTF16ToString(buf[:size])
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return ""
		}
	}
	return ""
}

// serviceGroups maps svchost.exe PIDs to the -k group in their services' command lines,
// read from the service control manager, which any authenticated user may enumerate.
type serviceGroups struct {
	byPID       map[uint32]string
	configGroup map[string]string // service name to -k group, "" for services outside svchost
	// enumerated is the FILETIME at which byPID was built; an entry describes an instance only
	// if that instance was already running then, because PIDs are reused.
	enumerated int64
	retryAfter time.Time
}

// lookup returns the group of a svchost.exe instance and whether it is known. A new instance
// re-enumerates at once when the map predates it; later retries wait groupRetryInterval,
// since a svchost.exe can appear before its service reports the PID.
func (g *serviceGroups) lookup(key instanceKey, newInstance bool, now time.Time) (string, bool) {
	fresh := g.enumerated > key.createTime
	if group, ok := g.byPID[key.pid]; ok && fresh {
		return group, true
	}
	if (fresh || !newInstance) && now.Before(g.retryAfter) {
		return "", false
	}
	start := windows.NsecToFiletime(now.UnixNano())
	g.retryAfter = now.Add(groupRetryInterval)
	if err := g.enumerate(); err != nil {
		return "", false
	}
	g.enumerated = int64(filetimeTicks(start))
	group, ok := g.byPID[key.pid]
	return group, ok
}

// enumerate rebuilds byPID, and configGroup from the services seen in this pass, so neither
// grows with services that are gone and a failed config read is retried next time.
func (g *serviceGroups) enumerate() error {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(scm)
	byPID := map[uint32]string{}
	configGroup := map[string]string{}
	buf := alignedBuffer(64 << 10)
	var resume uint32
	for {
		var needed, returned uint32
		err := windows.EnumServicesStatusEx(scm, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_ACTIVE,
			&buf[0], uint32(len(buf)), &needed, &returned, &resume, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return err
		}
		if returned > 0 {
			entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), returned)
			for _, e := range entries {
				pid := e.ServiceStatusProcess.ProcessId
				if pid == 0 {
					continue
				}
				name := windows.UTF16PtrToString(e.ServiceName)
				group, ok := g.groupOf(scm, name)
				if !ok {
					continue
				}
				configGroup[name] = group
				if group != "" {
					byPID[pid] = group
				}
			}
		}
		if err == nil {
			break
		}
		if int(needed) > len(buf) {
			buf = alignedBuffer(int(needed))
		}
	}
	g.byPID, g.configGroup = byPID, configGroup
	return nil
}

// groupOf returns the -k group in the service's configured command line, from the previous
// pass when known; ok is false when the configuration could not be read.
func (g *serviceGroups) groupOf(scm windows.Handle, name string) (string, bool) {
	if cached, known := g.configGroup[name]; known {
		return cached, true
	}
	cmdline, err := serviceCommandLine(scm, name)
	if err != nil {
		return "", false
	}
	return svchostGroup(cmdline), true
}

func serviceCommandLine(scm windows.Handle, name string) (string, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	svc, err := windows.OpenService(scm, namePtr, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return "", err
	}
	defer windows.CloseServiceHandle(svc)
	// 8 KB is the documented maximum size of a QUERY_SERVICE_CONFIG buffer.
	buf := alignedBuffer(8 << 10)
	var needed uint32
	config := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
	if err := windows.QueryServiceConfig(svc, config, uint32(len(buf)), &needed); err != nil {
		return "", err
	}
	return windows.UTF16PtrToString(config.BinaryPathName), nil
}

// svchostGroup extracts the group name that follows -k in a svchost.exe command line.
func svchostGroup(cmdline string) string {
	fields := strings.Fields(cmdline)
	for i, field := range fields {
		if strings.EqualFold(field, "-k") && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
