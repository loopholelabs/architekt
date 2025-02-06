package peer

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
	"github.com/loopholelabs/logging"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
	"github.com/stretchr/testify/assert"
)

const testPeerDirCow = "test_peer_cow"

func setupDevicesCow(t *testing.T, num int) ([]common.MigrateToDevice, [][]common.MigrateFromDevice, map[string]int) {
	for n := 0; n < num; n++ {
		err := os.Mkdir(fmt.Sprintf("%s_%d", testPeerDirCow, n), 0777)
		assert.NoError(t, err)

		t.Cleanup(func() {
			err := os.RemoveAll(fmt.Sprintf("%s_%d", testPeerDirCow, n))
			assert.NoError(t, err)
		})

	}

	devicesTo := make([]common.MigrateToDevice, 0)
	devicesFrom := make([][]common.MigrateFromDevice, num)

	for n := 0; n < num; n++ {
		devicesFrom[n] = make([]common.MigrateFromDevice, 0)
	}

	deviceSizes := make(map[string]int, 0)

	// Create some device source files, and setup devicesTo and devicesFrom for migration.
	for _, n := range common.KnownNames {
		// Create some initial devices...
		fn := common.DeviceFilenames[n]

		dataSize := (1 + rand.Intn(5)) * 1024 * 1024
		buffer := make([]byte, dataSize)
		_, err := crand.Read(buffer)
		assert.NoError(t, err)
		err = os.WriteFile(path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, 0), fn), buffer, 0777)
		assert.NoError(t, err)

		deviceSizes[n] = dataSize

		for i := 0; i < num; i++ {
			devicesFrom[i] = append(devicesFrom[i], common.MigrateFromDevice{
				Name:      n,
				Base:      path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, i), fn),
				Overlay:   path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, i), fmt.Sprintf("%s.overlay", fn)),
				State:     path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, i), fmt.Sprintf("%s.state", fn)),
				BlockSize: 1024 * 1024,
				Shared:    false,
			})
		}

		devicesTo = append(devicesTo, common.MigrateToDevice{
			Name:           n,
			MaxDirtyBlocks: 10,
			MinCycles:      1,
			MaxCycles:      3,
			CycleThrottle:  1 * time.Second,
		})

	}
	return devicesTo, devicesFrom, deviceSizes
}

func TestPeerCowMulti(t *testing.T) {

	log := logging.New(logging.Zerolog, "test", os.Stderr)
	//	log.SetLevel(types.TraceLevel)

	numMigrations := 5

	devicesTo, devicesFrom, deviceSizes := setupDevicesCow(t, 1+numMigrations)

	for n, s := range deviceSizes {
		fmt.Printf("Size %s - %d\n", n, s)
	}

	rp := &MockRuntimeProvider{
		HomePath:    fmt.Sprintf("%s_%d", testPeerDirCow, 0),
		DoWrites:    true,
		DeviceSizes: deviceSizes,
	}
	peer, err := StartPeer(context.TODO(), context.Background(), log, nil, rp)
	assert.NoError(t, err)

	hooks1 := MigrateFromHooks{
		OnLocalDeviceRequested:     func(id uint32, path string) {},
		OnLocalDeviceExposed:       func(id uint32, path string) {},
		OnLocalAllDevicesRequested: func() {},
		OnXferCustomData:           func(data []byte) {},
	}

	err = peer.MigrateFrom(context.TODO(), devicesFrom[0], nil, nil, hooks1)
	assert.NoError(t, err)

	err = peer.Resume(context.TODO(), 10*time.Second, 10*time.Second)
	assert.NoError(t, err)

	// Now we have a FIRST "resumed peer"

	// Lets send it on a migration journey...

	var lastPeer = peer

	for migration := 0; migration < numMigrations; migration++ {

		// Create a new RuntimeProvider
		rp2 := &MockRuntimeProvider{
			HomePath:    fmt.Sprintf("%s_%d", testPeerDirCow, migration+1),
			DoWrites:    true,
			DeviceSizes: deviceSizes,
		}
		nextPeer, err := StartPeer(context.TODO(), context.Background(), log, nil, rp2)
		assert.NoError(t, err)

		r1, w1 := io.Pipe()
		r2, w2 := io.Pipe()

		hooks := MigrateToHooks{
			OnBeforeSuspend:          func() {},
			OnAfterSuspend:           func() {},
			OnAllMigrationsCompleted: func() {},
			OnProgress:               func(p map[string]*migrator.MigrationProgress) {},
			GetXferCustomData:        func() []byte { return []byte{} },
		}

		var wg sync.WaitGroup
		var sendingErr error
		wg.Add(1)
		go func() {
			err := lastPeer.MigrateTo(context.TODO(), devicesTo, 10*time.Second, 10, []io.Reader{r1}, []io.Writer{w2}, hooks)
			assert.NoError(t, err)
			sendingErr = err
			wg.Done()
		}()

		var completedWg sync.WaitGroup
		completedWg.Add(1)
		hooks2 := MigrateFromHooks{
			OnLocalDeviceRequested:     func(id uint32, path string) {},
			OnLocalDeviceExposed:       func(id uint32, path string) {},
			OnLocalAllDevicesRequested: func() {},
			OnXferCustomData:           func(data []byte) {},
			OnCompletion: func() {
				fmt.Printf("Completed!\n")
				completedWg.Done()
			},
		}
		err = nextPeer.MigrateFrom(context.TODO(), devicesFrom[1+migration], []io.Reader{r2}, []io.Writer{w1}, hooks2)
		assert.NoError(t, err)

		// Wait for sending side to complete
		wg.Wait()

		// Make sure we got the completion callback
		completedWg.Wait()

		if err == nil && sendingErr == nil {
			// Make sure everything migrated as expected...
			for _, n := range common.KnownNames {
				buff1, err := os.ReadFile(path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, migration), n))
				assert.NoError(t, err)
				buff2, err := os.ReadFile(path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, migration+1), n))
				assert.NoError(t, err)

				// Compare hashes so we don't get tons of output if they do differ.
				hash1 := sha256.Sum256(buff1)
				hash2 := sha256.Sum256(buff2)

				fmt.Printf(" # Migration %d End hash %s ~ %x => %x\n", migration+1, n, hash1, hash2)

				// Check the data is identical
				assert.Equal(t, hash1, hash2)
			}
		}

		// HACK

		hack := false

		if hack {
			for _, devName := range common.KnownNames {
				os.Remove(path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, migration+1), devName))
			}

			nextPeer.Close()
			nextPeer, err = StartPeer(context.TODO(), context.Background(), log, nil, rp2)
			assert.NoError(t, err)

			hooks1 := MigrateFromHooks{
				OnLocalDeviceRequested:     func(id uint32, path string) {},
				OnLocalDeviceExposed:       func(id uint32, path string) {},
				OnLocalAllDevicesRequested: func() {},
				OnXferCustomData:           func(data []byte) {},
			}

			err = nextPeer.MigrateFrom(context.TODO(), devicesFrom[migration+1], nil, nil, hooks1)
			assert.NoError(t, err)
			// END OF HACK
		}

		// We can resume here, and will start writes again.
		err = nextPeer.Resume(context.TODO(), 10*time.Second, 10*time.Second)
		assert.NoError(t, err)

		// Close from receiving side
		r2.Close()
		w1.Close()

		// Make sure we can close the last peer.
		for _, devName := range common.KnownNames {
			os.Remove(path.Join(fmt.Sprintf("%s_%d", testPeerDirCow, migration), devName))
		}

		err = lastPeer.Close()
		assert.NoError(t, err)

		lastPeer = nextPeer

		fmt.Printf("\nMIGRATION %d COMPLETED\n\n", migration+1)
	}

	// Close the final peer
	err = lastPeer.Close()
	assert.NoError(t, err)

}
