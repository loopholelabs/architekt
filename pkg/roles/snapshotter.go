package roles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/loopholelabs/drafter/internal/firecracker"
	iutils "github.com/loopholelabs/drafter/internal/utils"
	"github.com/loopholelabs/drafter/pkg/ipc"
	"github.com/loopholelabs/drafter/pkg/utils"
	"github.com/loopholelabs/logging"
	"github.com/loopholelabs/sentry/pkg/rpc"
	"github.com/loopholelabs/sentry/pkg/server"
)

var (
	ErrCouldNotGetDeviceStat                 = errors.New("could not get NBD device stat")
	ErrCouldNotWaitForFirecrackerServer      = errors.New("could not wait for Firecracker server")
	ErrCouldNotOpenLivenessServer            = errors.New("could not open liveness server")
	ErrCouldNotChownLivenessServerVSock      = errors.New("could not change ownership of liveness server VSock")
	ErrCouldNotChownAgentServerVSock         = errors.New("could not change ownership of agent server VSock")
	ErrCouldNotOpenInputFile                 = errors.New("could not open input file")
	ErrCouldNotCreateOutputFile              = errors.New("could not create output file")
	ErrCouldNotCopyFile                      = errors.New("error copying file")
	ErrCouldNotWritePadding                  = errors.New("could not write padding")
	ErrCouldNotCopyDeviceFile                = errors.New("could not copy device file")
	ErrCouldNotStartVM                       = errors.New("could not start VM")
	ErrCouldNotReceiveAndCloseLivenessServer = errors.New("could not receive and close liveness server")
	ErrCouldNotAcceptAgentConnection         = errors.New("could not accept agent connection")
	ErrCouldNotBeforeSuspend                 = errors.New("error before suspend")
	ErrCouldNotMarshalPackageConfig          = errors.New("could not marshal package configuration")
	ErrCouldNotOpenPackageConfigFile         = errors.New("could not open package configuration file")
	ErrCouldNotWritePackageConfig            = errors.New("could not write package configuration")
	ErrCouldNotChownPackageConfigFile        = errors.New("could not change ownership of package configuration file")
)

type AgentConfiguration struct {
	AgentVSockPort uint32
	ResumeTimeout  time.Duration
}

type LivenessConfiguration struct {
	LivenessVSockPort uint32
	ResumeTimeout     time.Duration
}

type SnapshotDevice struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

func CreateSnapshot(
	ctx context.Context,

	devices []SnapshotDevice,

	vmConfiguration VMConfiguration,
	livenessConfiguration LivenessConfiguration,

	hypervisorConfiguration HypervisorConfiguration,
	networkConfiguration NetworkConfiguration,
	agentConfiguration AgentConfiguration,
) (errs error) {
	ctx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
		ctx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	if err := os.MkdirAll(hypervisorConfiguration.ChrootBaseDir, os.ModePerm); err != nil {
		panic(errors.Join(ErrCouldNotCreateChrootBaseDirectory, err))
	}

	fcServer, err := firecracker.StartFirecrackerServer(
		ctx,

		hypervisorConfiguration.FirecrackerBin,
		hypervisorConfiguration.JailerBin,

		hypervisorConfiguration.ChrootBaseDir,

		hypervisorConfiguration.UID,
		hypervisorConfiguration.GID,

		hypervisorConfiguration.NetNS,
		hypervisorConfiguration.NumaNode,
		hypervisorConfiguration.CgroupVersion,

		hypervisorConfiguration.EnableOutput,
		hypervisorConfiguration.EnableInput,
	)
	if err != nil {
		panic(errors.Join(ErrCouldNotStartFirecrackerServer, err))
	}
	defer fcServer.Close()
	defer os.RemoveAll(filepath.Dir(fcServer.VMPath)) // Remove `firecracker/$id`, not just `firecracker/$id/root`

	handleGoroutinePanics(true, func() {
		if err := fcServer.Wait(); err != nil {
			panic(errors.Join(ErrCouldNotWaitForFirecrackerServer, err))
		}
	})

	liveness := ipc.NewLivenessServer(
		filepath.Join(fcServer.VMPath, VSockName),
		livenessConfiguration.LivenessVSockPort,
	)

	livenessVSockPath, err := liveness.Open()
	if err != nil {
		panic(errors.Join(ErrCouldNotOpenLivenessServer, err))
	}
	defer liveness.Close()

	if err := os.Chown(livenessVSockPath, hypervisorConfiguration.UID, hypervisorConfiguration.GID); err != nil {
		panic(errors.Join(ErrCouldNotChownLivenessServerVSock, err))
	}

	sentryPath := fmt.Sprintf("%s_%d", filepath.Join(fcServer.VMPath, VSockName), agentConfiguration.AgentVSockPort)
	sentry, err := server.New(&server.Options{
		UnixPath: sentryPath,
		MaxConn:  32,
		Logger:   logging.NewNoopLogger(),
	})
	if err != nil {
		panic(errors.Join(ErrCouldNotStartAgentServer, err))
	}
	defer sentry.Close()

	if err := os.Chown(sentryPath, hypervisorConfiguration.UID, hypervisorConfiguration.GID); err != nil {
		panic(errors.Join(ErrCouldNotChownAgentServerVSock, err))
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(fcServer.VMPath, firecracker.FirecrackerSocketName))
			},
		},
	}

	defer func() {
		defer handlePanics(true)()

		if errs != nil {
			return
		}

		for _, device := range devices {
			inputFile, err := os.Open(filepath.Join(fcServer.VMPath, device.Name))
			if err != nil {
				panic(errors.Join(ErrCouldNotOpenInputFile, err))
			}
			defer inputFile.Close()

			if err := os.MkdirAll(filepath.Dir(device.Output), os.ModePerm); err != nil {
				panic(errors.Join(ErrCouldNotCreateOutputDir, err))
			}

			outputFile, err := os.OpenFile(device.Output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
			if err != nil {
				panic(errors.Join(ErrCouldNotCreateOutputFile, err))
			}
			defer outputFile.Close()

			deviceSize, err := io.Copy(outputFile, inputFile)
			if err != nil {
				panic(errors.Join(ErrCouldNotCopyFile, err))
			}

			if paddingLength := utils.GetBlockDevicePadding(deviceSize); paddingLength > 0 {
				if _, err := outputFile.Write(make([]byte, paddingLength)); err != nil {
					panic(errors.Join(ErrCouldNotWritePadding, err))
				}
			}
		}
	}()
	// We need to stop the Firecracker process from using the mount before we can unmount it
	defer fcServer.Close()

	disks := []string{}
	for _, device := range devices {
		if strings.TrimSpace(device.Input) != "" {
			if _, err := iutils.CopyFile(device.Input, filepath.Join(fcServer.VMPath, device.Name), hypervisorConfiguration.UID, hypervisorConfiguration.GID); err != nil {
				panic(errors.Join(ErrCouldNotCopyDeviceFile, err))
			}
		}

		if !slices.Contains(KnownNames, device.Name) || device.Name == DiskName {
			disks = append(disks, device.Name)
		}
	}

	if err := firecracker.StartVM(
		ctx,

		client,

		KernelName,

		disks,

		vmConfiguration.CPUCount,
		vmConfiguration.MemorySize,
		vmConfiguration.CPUTemplate,
		vmConfiguration.BootArgs,

		networkConfiguration.Interface,
		networkConfiguration.MAC,

		VSockName,
		ipc.VSockCIDGuest,
	); err != nil {
		panic(errors.Join(ErrCouldNotStartVM, err))
	}
	defer os.Remove(filepath.Join(fcServer.VMPath, VSockName))

	{
		receiveCtx, cancel := context.WithTimeout(ctx, livenessConfiguration.ResumeTimeout)
		defer cancel()

		if err := liveness.ReceiveAndClose(receiveCtx); err != nil {
			panic(errors.Join(ErrCouldNotReceiveAndCloseLivenessServer, err))
		}
	}

	{
		beforeSuspendCtx, cancel := context.WithTimeout(ctx, agentConfiguration.ResumeTimeout)
		defer cancel()

		request := rpc.Request{
			UUID: uuid.New(),
			Type: ipc.BeforeSuspendType,
			Data: nil,
		}
		var response rpc.Response

		err = sentry.Do(beforeSuspendCtx, &request, &response)
		if err != nil {
			panic(errors.Join(ErrCouldNotBeforeSuspend, err))
		}
	}

	// Connections need to be closed before creating the snapshot
	liveness.Close()
	_ = sentry.Close()

	if err := firecracker.CreateSnapshot(
		ctx,

		client,

		StateName,
		MemoryName,

		firecracker.SnapshotTypeFull,
	); err != nil {
		panic(errors.Join(ErrCouldNotCreateSnapshot, err))
	}

	packageConfig, err := json.Marshal(PackageConfiguration{
		AgentVSockPort: agentConfiguration.AgentVSockPort,
	})
	if err != nil {
		panic(errors.Join(ErrCouldNotMarshalPackageConfig, err))
	}

	outputFile, err := os.OpenFile(filepath.Join(fcServer.VMPath, ConfigName), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.ModePerm)
	if err != nil {
		panic(errors.Join(ErrCouldNotOpenPackageConfigFile, err))
	}
	defer outputFile.Close()

	if _, err := outputFile.Write(packageConfig); err != nil {
		panic(errors.Join(ErrCouldNotWritePackageConfig, err))
	}

	if err := os.Chown(filepath.Join(fcServer.VMPath, ConfigName), hypervisorConfiguration.UID, hypervisorConfiguration.GID); err != nil {
		panic(errors.Join(ErrCouldNotChownPackageConfigFile, err))
	}

	return
}
