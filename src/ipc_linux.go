//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	dsnoopIPCKeyMin = 8421
	dsnoopIPCKeyMax = 8431
	sysvIPCRmid     = 0
)

func cleanDsnoopIPC() error {
	var firstErr error
	for _, spec := range []struct {
		procPath string
		idCol    string
		sysno    uintptr
		nargs    int
	}{
		{"/proc/sysvipc/sem", "semid", syscall.SYS_SEMCTL, 4},
		{"/proc/sysvipc/shm", "shmid", syscall.SYS_SHMCTL, 3},
	} {
		data, err := os.ReadFile(spec.procPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ids, err := findRootDsnoopIPC(string(data), spec.idCol)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, id := range ids {
			var errno syscall.Errno
			if spec.nargs == 4 {
				_, _, errno = syscall.Syscall6(spec.sysno, uintptr(id), 0, sysvIPCRmid, 0, 0, 0)
			} else {
				_, _, errno = syscall.Syscall(spec.sysno, uintptr(id), sysvIPCRmid, 0)
			}
			if errno != 0 && errno != syscall.EINVAL && errno != syscall.EIDRM && firstErr == nil {
				firstErr = fmt.Errorf("%s %d IPC_RMID: %w", spec.idCol, id, errno)
			}
		}
	}
	return firstErr
}

func findRootDsnoopIPC(content, idCol string) ([]int, error) {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) < 2 {
		return nil, nil
	}
	header := strings.Fields(lines[0])
	keyIdx, idIdx, uidIdx := -1, -1, -1
	for i, name := range header {
		switch name {
		case "key":
			keyIdx = i
		case idCol:
			idIdx = i
		case "uid":
			uidIdx = i
		}
	}
	if keyIdx < 0 || idIdx < 0 || uidIdx < 0 {
		return nil, fmt.Errorf("unexpected sysvipc header for %s", idCol)
	}
	var ids []int
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) <= keyIdx || len(fields) <= idIdx || len(fields) <= uidIdx {
			continue
		}
		key, err1 := strconv.Atoi(fields[keyIdx])
		id, err2 := strconv.Atoi(fields[idIdx])
		uid, err3 := strconv.Atoi(fields[uidIdx])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		if uid == 0 && key >= dsnoopIPCKeyMin && key <= dsnoopIPCKeyMax {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
