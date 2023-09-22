package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/loopholelabs/architekt/pkg/firecracker"
	"github.com/loopholelabs/architekt/pkg/network"
	"github.com/loopholelabs/architekt/pkg/utils"
	"github.com/loopholelabs/architekt/pkg/vsock"
)

func main() {
	pwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	firecrackerBin := flag.String("firecracker-bin", "firecracker", "Firecracker binary")
	firecrackerSocketPath := flag.String("firecracker-socket-path", filepath.Join(pwd, "firecracker.sock"), "Firecracker socket path (must be absolute)")

	verbose := flag.Bool("verbose", false, "Whether to enable verbose logging")
	enableOutput := flag.Bool("enable-output", true, "Whether to enable VM stdout and stderr")
	enableInput := flag.Bool("enable-input", false, "Whether to enable VM stdin")

	hostInterface := flag.String("host-interface", "vm0", "Host interface name")
	hostMAC := flag.String("host-mac", "02:0e:d9:fd:68:3d", "Host MAC address")
	bridgeInterface := flag.String("bridge-interface", "firecracker0", "Bridge interface name")

	vsockPath := flag.String("vsock-path", "vsock.sock", "VSock path")
	agentVSockPort := flag.Int("agent-vsock-port", 26, "Agent VSock port")

	packagePath := flag.String("package-path", filepath.Join("out", "redis.ark"), "Path to package to use")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	packageDir, err := os.MkdirTemp("", "")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(packageDir)

	loop := utils.NewLoop(*packagePath)

	devicePath, err := loop.Open()
	if err != nil {
		panic(err)
	}
	defer loop.Close()

	mount := utils.NewMount(devicePath, packageDir)

	if err := mount.Open(); err != nil {
		panic(err)
	}
	defer mount.Close()

	tap := network.NewTAP(
		*hostInterface,
		*hostMAC,
		*bridgeInterface,
	)

	if err := tap.Open(); err != nil {
		panic(err)
	}
	defer tap.Close()

	srv := firecracker.NewServer(
		*firecrackerBin,
		*firecrackerSocketPath,
		packageDir,

		*verbose,
		*enableOutput,
		*enableInput,
	)

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := srv.Wait(); err != nil {
			panic(err)
		}
	}()

	defer srv.Close()
	if err := srv.Open(); err != nil {
		panic(err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", *firecrackerSocketPath)
			},
		},
	}

	before := time.Now()

	if err := firecracker.ResumeSnapshot(client); err != nil {
		panic(err)
	}
	defer os.Remove(filepath.Join(packageDir, *vsockPath))

	handler := vsock.NewHandler(
		filepath.Join(packageDir, *vsockPath),
		uint32(*agentVSockPort),

		time.Second*10,
	)

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := handler.Wait(); err != nil {
			panic(err)
		}
	}()

	defer handler.Close()
	peer, err := handler.Open(ctx, time.Millisecond*100, time.Second*10)
	if err != nil {
		panic(err)
	}

	if err := peer.AfterResume(ctx); err != nil {
		panic(err)
	}

	log.Println("Resume:", time.Since(before))

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)

	<-done

	before = time.Now()

	if err := peer.BeforeSuspend(ctx); err != nil {
		panic(err)
	}

	_ = handler.Close() // Connection needs to be closed before flushing the snapshot

	if err := firecracker.FlushSnapshot(client); err != nil {
		panic(err)
	}

	log.Println("Suspend:", time.Since(before))
}
