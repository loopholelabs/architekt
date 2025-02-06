package common

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDirtyManager(t *testing.T) {

	devName := "test"

	devices := map[string]*DeviceStatus{
		devName: {
			MinCycles:      5,
			MaxCycles:      10,
			CycleThrottle:  100 * time.Millisecond,
			MaxDirtyBlocks: 10,
		},
	}

	var msyncCalled sync.WaitGroup
	var suspendCalled sync.WaitGroup
	var authTransferCalled sync.WaitGroup

	authTransferCalled.Add(1)
	authTransfer := func() error {
		authTransferCalled.Done()
		return nil
	}

	suspendCalled.Add(1)
	suspendFunc := func(ctx context.Context, timeout time.Duration) error {
		suspendCalled.Done()
		return nil
	}

	suspendTimeout := 100 * time.Millisecond

	msyncCalled.Add(1)
	msyncFunc := func(ctx context.Context) error {
		msyncCalled.Done()
		return nil
	}

	onBeforeSuspend := func() {}

	onAfterSuspend := func() {}

	vm := NewVMStateMgr(context.TODO(), suspendFunc, suspendTimeout, msyncFunc, onBeforeSuspend, onAfterSuspend)

	dm := NewDirtyManager(vm, devices, authTransfer)

	// Try something simple...

	noMoreDirtyBlocks := 8

	count := 0
	for {
		blocks := []uint{0} // Dummy, under the threshold.
		if count >= noMoreDirtyBlocks {
			blocks = []uint{} // No dirty blocks...
		}

		err := dm.PreGetDirty(devName)
		assert.NoError(t, err)
		more, err := dm.PostGetDirty(devName, blocks)
		assert.NoError(t, err)

		if count >= noMoreDirtyBlocks {
			assert.False(t, more)
			break
		}

		assert.True(t, more)
		more, err = dm.PostMigrateDirty(devName, blocks)
		assert.NoError(t, err)
		assert.True(t, more)
		count++

		// It should have run some things since it's past min
		if count > 5 {
			suspendCalled.Wait()
			msyncCalled.Wait()
		}

		// Auth is tranfered on NEXT loop to allow dirtyList to be sent
		if count > 6 {
			// Make sure auth was transferred
			authTransferCalled.Wait()
		}
	}

}
