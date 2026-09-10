package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

type instanceLock struct{ handle windows.Handle }

func acquireInstanceLock() (*instanceLock, bool, error) {
	name, err := windows.UTF16PtrFromString(`Local\AgnesProxyDesktop-e4c75303-541e-48aa-a445-af91e2b32c44`)
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create instance mutex: %w", err)
	}
	return &instanceLock{handle: handle}, true, nil
}

func (lock *instanceLock) Close() {
	if lock != nil && lock.handle != 0 {
		_ = windows.CloseHandle(lock.handle)
		lock.handle = 0
	}
}
