package main

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/loopholelabs/drafter/pkg/common"
)

type CompositeDevices struct {
	Name string `json:"name"`

	Base    string `json:"base"`
	Overlay string `json:"overlay"`
	State   string `json:"state"`

	BlockSize uint32 `json:"blockSize"`

	Expiry time.Duration `json:"expiry"`

	MaxDirtyBlocks int `json:"maxDirtyBlocks"`
	MinCycles      int `json:"minCycles"`
	MaxCycles      int `json:"maxCycles"`

	CycleThrottle time.Duration `json:"cycleThrottle"`

	MakeMigratable bool `json:"makeMigratable"`
	Shared         bool `json:"shared"`

	SharedBase bool `json:"sharedbase"`

	S3Sync      bool   `json:"s3sync"`
	S3AccessKey string `json:"s3accesskey"`
	S3SecretKey string `json:"s3secretkey"`
	S3Endpoint  string `json:"s3endpoint"`
	S3Bucket    string `json:"s3bucket"`
}

func decodeDevices(data string) ([]CompositeDevices, error) {
	var devices []CompositeDevices
	err := json.Unmarshal([]byte(data), &devices)
	return devices, err
}

func getDefaultDevices() string {
	defaultDevices, err := json.Marshal([]CompositeDevices{
		{
			Name: common.DeviceStateName,

			Base:    filepath.Join("out", "package", "state.bin"),
			Overlay: filepath.Join("out", "overlay", "state.bin"),
			State:   filepath.Join("out", "state", "state.bin"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
			Shared:         false,
		},
		{
			Name: common.DeviceMemoryName,

			Base:    filepath.Join("out", "package", "memory.bin"),
			Overlay: filepath.Join("out", "overlay", "memory.bin"),
			State:   filepath.Join("out", "state", "memory.bin"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
			Shared:         false,
		},

		{
			Name: common.DeviceKernelName,

			Base:    filepath.Join("out", "package", "vmlinux"),
			Overlay: filepath.Join("out", "overlay", "vmlinux"),
			State:   filepath.Join("out", "state", "vmlinux"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
			Shared:         false,
		},
		{
			Name: common.DeviceDiskName,

			Base:    filepath.Join("out", "package", "rootfs.ext4"),
			Overlay: filepath.Join("out", "overlay", "rootfs.ext4"),
			State:   filepath.Join("out", "state", "rootfs.ext4"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
			Shared:         false,
		},

		{
			Name: common.DeviceConfigName,

			Base:    filepath.Join("out", "package", "config.json"),
			Overlay: filepath.Join("out", "overlay", "config.json"),
			State:   filepath.Join("out", "state", "config.json"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
			Shared:         false,
		},

		{
			Name: "oci",

			Base:    filepath.Join("out", "package", "oci.ext4"),
			Overlay: filepath.Join("out", "overlay", "oci.ext4"),
			State:   filepath.Join("out", "state", "oci.ext4"),

			BlockSize: 1024 * 64,

			Expiry: time.Second,

			MaxDirtyBlocks: 200,
			MinCycles:      5,
			MaxCycles:      20,

			CycleThrottle: time.Millisecond * 500,

			MakeMigratable: true,
			Shared:         false,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(defaultDevices)
}
