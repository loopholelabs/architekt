package common

import (
	"context"
	crand "crypto/rand"
	"math/rand"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
)

var tempPackageDirIn = "test_package_in"
var tempPackageDirOut = "test_package_out"
var tempPackageFilename = "test_package"

func TestPackage(t *testing.T) {
	err := os.Mkdir(tempPackageDirIn, 0777)
	assert.NoError(t, err)
	err = os.Mkdir(tempPackageDirOut, 0777)
	assert.NoError(t, err)

	t.Cleanup(func() {
		err := os.RemoveAll(tempPackageDirIn)
		assert.NoError(t, err)
		err = os.RemoveAll(tempPackageDirOut)
		assert.NoError(t, err)
	})

	// TODO: Create some random files, package them, unpackage, assert they're the same.

	inDevices := make([]PackagerDevice, 0)
	outDevices := make([]PackagerDevice, 0)

	for _, n := range KnownNames {
		f := DeviceFilenames[n]
		// Create a file, and fill in the info

		// Between 1m and 5m
		size := 1024*1024 + (rand.Intn(4 * 1024 * 1024))
		data := make([]byte, size)
		nbytes, err := crand.Read(data)
		assert.NoError(t, err)
		assert.Equal(t, size, nbytes)

		err = os.WriteFile(path.Join(tempPackageDirIn, f), data, 0777)
		assert.NoError(t, err)

		inDevices = append(inDevices, PackagerDevice{
			Name: n,
			Path: path.Join(tempPackageDirIn, f),
		})

		outDevices = append(outDevices, PackagerDevice{
			Name: n,
			Path: path.Join(tempPackageDirOut, f),
		})
	}

	ctx := context.TODO()

	// Package
	err = ArchivePackage(ctx, inDevices, tempPackageFilename)
	assert.NoError(t, err)

	t.Cleanup(func() {
		err = os.Remove(tempPackageFilename)
		assert.NoError(t, err)
	})

	// Unpackage
	err = ExtractPackage(ctx, tempPackageFilename, outDevices)
	assert.NoError(t, err)

	// Check all the data matches
	for i, ind := range inDevices {
		outd := outDevices[i]

		dataIn, err := os.ReadFile(ind.Path)
		assert.NoError(t, err)

		dataOut, err := os.ReadFile(outd.Path)
		assert.NoError(t, err)

		assert.Equal(t, dataIn, dataOut)
	}
}
