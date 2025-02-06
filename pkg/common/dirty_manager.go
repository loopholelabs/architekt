package common

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrCouldNotSuspendAndMsyncVM = errors.New("could not suspend and msync VM")
)

type DeviceStatus struct {
	TotalCycles                   int
	CycleThrottle                 time.Duration
	MaxDirtyBlocks                int
	CyclesBelowDirtyBlockTreshold int
	MinCycles                     int
	MaxCycles                     int
	Ready                         bool
	ReadyAndSentDirty             bool
}

type DirtyManager struct {
	VMState                    *VMStateMgr
	Devices                    map[string]*DeviceStatus
	ReadyDevices               map[string]*DeviceStatus
	ReadyDevicesLock           sync.Mutex
	AuthorityTransfer          func() error
	authorityTransferLock      sync.Mutex
	authorityTransferCompleted bool
	suspendLock                sync.Mutex
	suspendCompleted           bool
	suspendDoneCh              chan bool
}

func NewDirtyManager(vmState *VMStateMgr, devices map[string]*DeviceStatus, authorityTransfer func() error) *DirtyManager {
	return &DirtyManager{
		VMState:           vmState,
		Devices:           devices,
		ReadyDevices:      make(map[string]*DeviceStatus),
		AuthorityTransfer: authorityTransfer,
		suspendDoneCh:     make(chan bool),
	}
}

func (dm *DirtyManager) PreGetDirty(name string) error {
	// If the VM is still running, do an Msync for the memory...
	if !dm.VMState.CheckSuspendedVM() && name == DeviceMemoryName {
		err := dm.VMState.Msync()
		if err != nil {
			return errors.Join(ErrCouldNotMsyncRunner, err)
		}
	}
	return nil
}

func (dm *DirtyManager) PostGetDirty(name string, blocks []uint) (bool, error) {

	// If there were no dirty blocks, and the VM is stopped, return false (finish doing dirty sync)
	if len(blocks) == 0 && dm.VMState.CheckSuspendedVM() {
		err := dm.markReadyDeviceSentDirty(name)
		return false, err
	}
	return true, nil
}

func (dm *DirtyManager) markDeviceReady(name string, di *DeviceStatus) {
	if !di.Ready {
		di.Ready = true
		dm.ReadyDevicesLock.Lock()
		dm.ReadyDevices[name] = di
		dm.ReadyDevicesLock.Unlock()
	}
}

func (dm *DirtyManager) markReadyDeviceSentDirty(name string) error {
	dm.ReadyDevicesLock.Lock()
	defer dm.ReadyDevicesLock.Unlock()
	di, ok := dm.ReadyDevices[name]
	if ok {
		// This should only get sent IF the suspend has happened...
		di.ReadyAndSentDirty = true

		// Now check if ALL devices are ready and have sent dirty, and if so, call auth transfer ONCE
		for _, dii := range dm.Devices {
			if !dii.Ready || !dii.ReadyAndSentDirty {
				return nil // Something isn't ready or dirty sent
			}
		}

		dm.suspendLock.Lock()
		done := dm.suspendCompleted
		dm.suspendLock.Unlock()
		if !done {
			return nil // The suspend hasn't completed yet
		}

		dm.authorityTransferLock.Lock()
		if !dm.authorityTransferCompleted {
			dm.authorityTransferCompleted = true
			dm.authorityTransferLock.Unlock()
			err := dm.AuthorityTransfer()
			if err != nil {
				return err
			}
		} else {
			dm.authorityTransferLock.Unlock()
		}
	}
	return nil
}

func (dm *DirtyManager) PostMigrateDirty(name string, blocks []uint) (bool, error) {

	// If it is suspended, mark as ready and sent dirty list
	if dm.VMState.CheckSuspendedVM() {
		err := dm.markReadyDeviceSentDirty(name)
		if err != nil {
			return false, err
		}
	}

	di := dm.Devices[name]

	delay := time.After(di.CycleThrottle)

	select {
	case <-delay:
	case <-dm.suspendDoneCh: // Interrupt the cycleThrottle because it's been suspended.
		dm.ReadyDevicesLock.Lock()
		sd := di.ReadyAndSentDirty
		dm.ReadyDevicesLock.Unlock()

		if !sd {
			break // Shortcut out of here. The device hasn't sent a dirtyList
		}

		<-delay // Wait for the full delay
	}

	di.TotalCycles++

	if len(blocks) < di.MaxDirtyBlocks {
		di.CyclesBelowDirtyBlockTreshold++
		if di.CyclesBelowDirtyBlockTreshold > di.MinCycles {
			dm.markDeviceReady(name, di)
		}
	} else if di.TotalCycles > di.MaxCycles {
		dm.markDeviceReady(name, di)
	} else {
		di.CyclesBelowDirtyBlockTreshold = 0
	}

	// If all devices are ready, do the authority transfer once...
	// TODO: Clean this up a bit
	dm.ReadyDevicesLock.Lock()
	readyDevices := len(dm.ReadyDevices)
	dm.ReadyDevicesLock.Unlock()

	if readyDevices == len(dm.Devices) {

		dm.suspendLock.Lock()
		if !dm.suspendCompleted {
			// Do the SuspsnedAndMsync
			err := dm.VMState.SuspendAndMsync()

			dm.suspendCompleted = true
			close(dm.suspendDoneCh)
			dm.suspendLock.Unlock()

			if err != nil {
				return true, errors.Join(ErrCouldNotSuspendAndMsyncVM, err)
			}
		} else {
			dm.suspendLock.Unlock()
		}
	}
	return true, nil
}
