package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

type instanceLock struct {
	handle windows.Handle
}

func acquireInstanceLock() (*instanceLock, bool, error) {
	name, err := windows.UTF16PtrFromString(`Local\SenseNovaPoolDesktop-9f951809-14b9-466e-91dd-6492d0e761d8`)
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
