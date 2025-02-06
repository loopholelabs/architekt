package common

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

var (
	ErrMissingDevice                 = errors.New("missing resource")
	ErrCouldNotOpenPackageOutputFile = errors.New("could not open package output file")
	ErrCouldNotCreateCompressor      = errors.New("could not create compressor")
	ErrCouldNotStatDevice            = errors.New("could not stat device")
	ErrCouldNotCreateTarHeader       = errors.New("could not create tar header")
	ErrCouldNotWriteTarHeader        = errors.New("could not write tar header")
	ErrCouldNotOpenDevice            = errors.New("could not open device file")
	ErrCouldNotCopyToArchive         = errors.New("could not copy file to archive")
	ErrCouldNotOpenPackageInputFile  = errors.New("could not open package input file")
	ErrCouldNotCreateUncompressor    = errors.New("could not create uncompressor")
	ErrCouldNotReadNextHeader        = errors.New("could not read next header from archive")
	ErrCouldNotCreateOutputDir       = errors.New("could not create output directory")
	ErrCouldNotOpenOutputFile        = errors.New("could not open output file")
	ErrCouldNotCopyToOutput          = errors.New("could not copy file to output")
)

const (
	DeviceKernelName = "kernel"
	DeviceDiskName   = "disk"
	DeviceStateName  = "state"
	DeviceMemoryName = "memory"
	DeviceConfigName = "config"
	DeviceOCIName    = "oci"
)

var KnownNames = []string{
	DeviceKernelName,
	DeviceDiskName,
	DeviceStateName,
	DeviceMemoryName,
	DeviceConfigName,
}

var DeviceFilenames = map[string]string{
	DeviceKernelName: "vmlinux",
	DeviceDiskName:   "rootfs.ext4",
	DeviceStateName:  "state.bin",
	DeviceMemoryName: "memory.bin",
	DeviceConfigName: "config.json",
	DeviceOCIName:    "oci.ext4",
}

type PackagerDevice struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func ArchivePackage(ctx context.Context, devices []PackagerDevice, packageOutputPath string) error {
	packageOutputFile, err := os.OpenFile(packageOutputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
	if err != nil {
		return errors.Join(ErrCouldNotOpenPackageOutputFile, err)
	}
	defer packageOutputFile.Close()

	compressor, err := zstd.NewWriter(packageOutputFile)
	if err != nil {
		return errors.Join(ErrCouldNotCreateCompressor, err)
	}
	defer compressor.Close()

	packageOutputArchive := tar.NewWriter(compressor)
	defer packageOutputArchive.Close()

	for _, device := range devices {
	s:
		select {
		case <-ctx.Done():
			return ctx.Err()

		default:
			break s
		}

		info, err := os.Stat(device.Path)
		if err != nil {
			return errors.Join(ErrCouldNotStatDevice, err)
		}

		header, err := tar.FileInfoHeader(info, device.Path)
		if err != nil {
			return errors.Join(ErrCouldNotCreateTarHeader, err)
		}
		header.Name = device.Name

		if err := packageOutputArchive.WriteHeader(header); err != nil {
			return errors.Join(ErrCouldNotWriteTarHeader, err)
		}

		f, err := os.Open(device.Path)
		if err != nil {
			return errors.Join(ErrCouldNotOpenDevice, err)
		}
		defer f.Close()

		if _, err = io.Copy(packageOutputArchive, f); err != nil {
			return errors.Join(ErrCouldNotCopyToArchive, err)
		}
	}

	return nil
}

func ExtractPackage(ctx context.Context, packageInputPath string, devices []PackagerDevice) error {
	packageFile, err := os.Open(packageInputPath)
	if err != nil {
		return errors.Join(ErrCouldNotOpenPackageInputFile, err)
	}
	defer packageFile.Close()

	uncompressor, err := zstd.NewReader(packageFile)
	if err != nil {
		return errors.Join(ErrCouldNotCreateUncompressor, err)
	}
	defer uncompressor.Close()

	packageArchive := tar.NewReader(uncompressor)

	for _, device := range devices {
		extracted := false
		for {
		s:
			select {
			case <-ctx.Done():
				return ctx.Err()

			default:
				break s
			}

			header, err := packageArchive.Next()
			if err != nil {
				if err == io.EOF {
					break
				}

				return errors.Join(ErrCouldNotReadNextHeader, err)
			}

			if header.Name != device.Name {
				continue
			}

			if err := os.MkdirAll(filepath.Dir(device.Path), os.ModePerm); err != nil {
				return errors.Join(ErrCouldNotCreateOutputDir, err)
			}

			outputFile, err := os.OpenFile(device.Path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
			if err != nil {
				return errors.Join(ErrCouldNotOpenOutputFile, err)
			}
			defer outputFile.Close()

			if _, err = io.Copy(outputFile, packageArchive); err != nil {
				return errors.Join(ErrCouldNotCopyToOutput, err)
			}

			extracted = true

			break
		}

		if !extracted {
			// We join the more specific error here first
			return errors.Join(fmt.Errorf("missing device: %s", device.Name), ErrMissingDevice)
		}
	}

	return nil
}
