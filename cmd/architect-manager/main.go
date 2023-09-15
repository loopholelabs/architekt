package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"

	v1 "github.com/loopholelabs/architekt/pkg/api/http/firecracker/v1"
)

func putJSON(client *http.Client, body any, resource string) error {
	p, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, "http://localhost/"+resource, bytes.NewReader(p))
	if err != nil {
		return err
	}

	res, err := client.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode >= 300 {
		b, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}

		return errors.New(string(b))
	}

	return nil
}

func main() {
	firecrackerSocket := flag.String("firecracker-socket", "firecracker.sock", "Firecracker socket")

	initramfsPath := flag.String("initramfs-path", "out/template/architekt.initramfs", "initramfs path")
	kernelPath := flag.String("kernel-path", "out/template/architekt.kernel", "Kernel path")
	diskPath := flag.String("disk-path", "out/template/architekt.disk", "Disk path")
	cpuCount := flag.Int("cpu-count", 1, "CPU count")
	memorySize := flag.Int("memory-size", 1024, "Memory size (in MB)")

	flag.Parse()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", *firecrackerSocket)
			},
		},
	}

	if err := putJSON(
		client,
		&v1.BootSource{
			InitrdPath:      *initramfsPath,
			KernelImagePath: *kernelPath,
			BootArgs:        "console=ttyS0 panic=1 pci=off modules=ext4 rootfstype=ext4 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd rootflags=rw",
		},
		"boot-source",
	); err != nil {
		panic(err)
	}

	if err := putJSON(
		client,
		&v1.Drive{
			DriveID:      "root",
			PathOnHost:   *diskPath,
			IsRootDevice: true,
			IsReadOnly:   false,
		},
		"drives/root",
	); err != nil {
		panic(err)
	}

	if err := putJSON(
		client,
		&v1.MachineConfig{
			VCPUCount:  *cpuCount,
			MemSizeMib: *memorySize,
		},
		"machine-config",
	); err != nil {
		panic(err)
	}

	if err := putJSON(
		client,
		&v1.Action{
			ActionType: "InstanceStart",
		},
		"actions",
	); err != nil {
		panic(err)
	}
}
