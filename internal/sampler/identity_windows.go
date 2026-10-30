//go:build windows

package sampler

import (
	"errors"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// serviceRefreshInterval limits how often an unresolved svchost.exe re-enumerates services.
const serviceRefreshInterval = 30 * time.Second

type instanceKey struct {
	pid        uint32
	createTime int64
}

type identity struct {
	name          string
	path          string
	serviceGroup  string
	groupResolved bool
	pass          uint64
}

// identityCache resolves name, path and svchost group once per process instance and
// forgets instances that are gone, so it stays as large as the process list.
type identityCache struct {
	byInstance map[instanceKey]*identity
	services   serviceGroups
	pass       uint64
	lastCount  int
}

func newIdentityCache() *identityCache {
	return &identityCache{byInstance: map[instanceKey]*identity{}, services: serviceGroups{configGroup: map[string]string{}}}
}

func (c *identityCache) beginPass() { c.pass++ }

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
	id, ok := c.byInstance[key]
	if !ok {
		id = &identity{name: imageName.String(), path: imagePath(pid)}
		id.groupResolved = !strings.EqualFold(id.name, "svchost.exe")
		c.byInstance[key] = id
	}
	if !id.groupResolved {
		id.serviceGroup, id.groupResolved = c.services.lookup(pid, time.Now())
	}
	id.pass = c.pass
	return id
}

// imagePath returns the Win32 path of the process image, or "" when the process denies access.
func imagePath(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
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
	refreshed   time.Time
}

// lookup returns the group of pid and whether it is known; it re-enumerates services at most
// once per serviceRefreshInterval, since a svchost.exe can appear before its service reports its PID.
func (g *serviceGroups) lookup(pid uint32, now time.Time) (string, bool) {
	if group, ok := g.byPID[pid]; ok {
		return group, true
	}
	if !g.refreshed.IsZero() && now.Sub(g.refreshed) < serviceRefreshInterval {
		return "", false
	}
	g.refreshed = now
	if byPID, err := g.enumerate(); err == nil {
		g.byPID = byPID
	}
	group, ok := g.byPID[pid]
	return group, ok
}

func (g *serviceGroups) enumerate() (map[uint32]string, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	defer windows.CloseServiceHandle(scm)
	byPID := map[uint32]string{}
	buf := alignedBuffer(64 << 10)
	var resume uint32
	for {
		var needed, returned uint32
		err := windows.EnumServicesStatusEx(scm, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_ACTIVE,
			&buf[0], uint32(len(buf)), &needed, &returned, &resume, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		if returned > 0 {
			entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), returned)
			for _, e := range entries {
				pid := e.ServiceStatusProcess.ProcessId
				if pid == 0 {
					continue
				}
				if group := g.groupOf(scm, windows.UTF16PtrToString(e.ServiceName)); group != "" {
					byPID[pid] = group
				}
			}
		}
		if err == nil {
			return byPID, nil
		}
		if int(needed) > len(buf) {
			buf = alignedBuffer(int(needed))
		}
	}
}

// groupOf returns the -k group in the service's configured command line, cached per service.
func (g *serviceGroups) groupOf(scm windows.Handle, name string) string {
	if group, ok := g.configGroup[name]; ok {
		return group
	}
	group := ""
	if cmdline, err := serviceCommandLine(scm, name); err == nil {
		group = svchostGroup(cmdline)
	}
	g.configGroup[name] = group
	return group
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
