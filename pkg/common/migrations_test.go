package common

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loopholelabs/silo/pkg/storage/config"
	"github.com/loopholelabs/silo/pkg/storage/devicegroup"
	"github.com/loopholelabs/silo/pkg/storage/migrator"
	"github.com/stretchr/testify/assert"
)

func readAllFromDevice(name string, size int) ([]byte, error) {
	fd, err := os.Open(name)
	if err != nil {
		return nil, err
	}

	// Try reading the device...
	buffer := make([]byte, size)
	n, err := fd.ReadAt(buffer, 0)
	if n != size {
		return nil, errors.New("unable to read full data")
	}
	if err != nil {
		return nil, err
	}

	err = fd.Close()
	if err != nil {
		return nil, err
	}
	return buffer, nil
}

const testFileSize = 16 * 1024 * 1024
const tempDirFrom = "drafter_test_from"
const tempDirTo = "drafter_test_to"

func setupFromFs(t *testing.T) (string, *devicegroup.DeviceGroup) {
	// First lets setup a temp dir
	err := os.Mkdir(tempDirFrom, 0777)
	assert.NoError(t, err)

	t.Cleanup(func() {
		err := os.RemoveAll(tempDirFrom)
		assert.NoError(t, err)
	})

	// Create a dummy file to hold data for Silo.
	data := make([]byte, testFileSize) // 16M
	_, err = rand.Read(data)
	assert.NoError(t, err)
	os.WriteFile(path.Join(tempDirFrom, "file_data"), data, 0777)

	devices := []MigrateFromDevice{
		{
			Name:      "test",
			Base:      path.Join(tempDirFrom, "file_data"),
			Overlay:   "",
			State:     "",
			BlockSize: 1024 * 1024,
			Shared:    false,
		},
	}

	tweak := func(_ int, _ string, s *config.DeviceSchema) *config.DeviceSchema {
		return s
	}

	dg, err := MigrateFromFS(nil, nil, tempDirFrom, devices, tweak)
	assert.NoError(t, err)

	// Check the devices look ok and contain what we think they should...
	// The devices should have been created as 'tdir/<name>'
	if err == nil {
		names := dg.GetAllNames()
		assert.Equal(t, 1, len(names))
		assert.Equal(t, "test", names[0])

		ddata, err := readAllFromDevice(path.Join(tempDirFrom, "test"), len(data))
		assert.NoError(t, err)
		assert.Equal(t, data, ddata)

		t.Cleanup(func() {
			err = dg.CloseAll()
			assert.NoError(t, err)
		})
	}

	return tempDirFrom, dg
}

func TestMigrateFromFs(t *testing.T) {
	currentUser, err := user.Current()
	if err != nil {
		panic(err)
	}
	if currentUser.Username != "root" {
		fmt.Printf("Cannot run test unless we are root.\n")
		return
	}

	setupFromFs(t)
}

func TestMigrateFromFsThenBetween(t *testing.T) {
	currentUser, err := user.Current()
	if err != nil {
		panic(err)
	}
	if currentUser.Username != "root" {
		fmt.Printf("Cannot run test unless we are root.\n")
		return
	}

	// First lets setup a temp dir
	err = os.Mkdir(tempDirTo, 0777)
	assert.NoError(t, err)
	t.Cleanup(func() {
		err := os.RemoveAll(tempDirTo)
		assert.NoError(t, err)
	})

	tdir, dg := setupFromFs(t)

	if dg != nil {

		r1, w1 := io.Pipe()
		r2, w2 := io.Pipe()

		progress := func(p map[string]*migrator.MigrationProgress) {
			fmt.Printf("Progress...\n")
		}

		devices := []MigrateToDevice{
			{
				Name:           "test",
				MaxDirtyBlocks: 10,
				MinCycles:      4,
				MaxCycles:      10,
				CycleThrottle:  100 * time.Millisecond,
			},
		}

		var dg2 *devicegroup.DeviceGroup
		var migrateWait sync.WaitGroup

		var customDataLock sync.Mutex
		var customDataReceived []byte

		migrateWait.Add(1)
		go func() {
			tweak := func(index int, name string, schema *config.DeviceSchema) *config.DeviceSchema {
				schema.Location = strings.ReplaceAll(schema.Location, tdir, tempDirTo)
				return schema
			}

			cdh := func(cdata []byte) {
				customDataLock.Lock()
				customDataReceived = cdata
				customDataLock.Unlock()
				// cdata is custom xfer data
			}

			dg2, err = MigrateFromPipe(nil, nil, tempDirTo,
				context.TODO(), []io.Reader{r2}, []io.Writer{w1}, tweak, cdh)
			assert.NoError(t, err)

			// TODO: Check dg2 looks good...
			names := dg2.GetAllNames()
			assert.Equal(t, 1, len(names))
			assert.Equal(t, "test", names[0])

			// TODO: Read data from tdir2 and make sure it matches.
			ddata1, err := readAllFromDevice(path.Join(tdir, "test"), testFileSize)
			assert.NoError(t, err)
			ddata2, err := readAllFromDevice(path.Join(tempDirTo, "test"), testFileSize)
			assert.NoError(t, err)

			assert.Equal(t, ddata1, ddata2)

			err = dg2.WaitForCompletion()
			assert.NoError(t, err)

			migrateWait.Done()
		}()

		suspendFunc := func(ctx context.Context, timeout time.Duration) error {
			fmt.Printf("suspendFunc\n")
			return nil
		}
		msyncFunc := func(ctx context.Context) error {
			fmt.Printf("msyncFunc\n")
			return nil
		}
		onBeforeSuspend := func() {
			fmt.Printf("onBeforeSuspend\n")
		}
		onAfterSuspend := func() {
			fmt.Printf("onAfterSuspend\n")
		}

		vmState := NewVMStateMgr(context.TODO(), suspendFunc, 100*time.Millisecond,
			msyncFunc, onBeforeSuspend, onAfterSuspend)

		customData := []byte("hello")

		getCustomPayload := func() []byte {
			fmt.Printf("getCustomPayload\n")
			return customData
		}

		err = MigrateToPipe(context.TODO(), []io.Reader{r1}, []io.Writer{w2},
			dg, 100, progress, vmState, devices, getCustomPayload)
		assert.NoError(t, err)

		// Make sure it all completed
		migrateWait.Wait()

		customDataLock.Lock()
		assert.Equal(t, customData, customDataReceived)
		customDataLock.Unlock()
	} else {
		assert.Fail(t, "dg failed to setup")
	}
}
