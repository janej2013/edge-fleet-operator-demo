// The edge agent: one static, stdlib-only binary that impersonates a device.
// Run N of these to simulate a fleet. Verify the "no dependencies" claim with:
//
//	go list -deps ./cmd/agent | grep -c k8s.io   # → 0
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/janej2013/edge-fleet-operator-demo/internal/agent"
)

func main() {
	var (
		name       = flag.String("name", "", "device name (EdgeDevice CR name), required")
		namespace  = flag.String("namespace", "default", "namespace of the EdgeDevice CR")
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig (kind-style flat file)")
		server     = flag.String("server", "", "plain API server URL (e.g. kubectl proxy) — alternative to --kubeconfig")
		dataDir    = flag.String("data-dir", "", "device storage dir (default ~/.edge-fleet/<name>); must be on a filesystem with symlinks")
		poll       = flag.Duration("poll-interval", 3*time.Second, "spec poll interval (jittered)")
		heartbeat  = flag.Duration("heartbeat-interval", 5*time.Second, "heartbeat interval")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("device", *name)
	if *name == "" {
		log.Error("--name is required")
		os.Exit(2)
	}
	if *dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Error("cannot resolve home dir", "err", err)
			os.Exit(1)
		}
		*dataDir = filepath.Join(home, ".edge-fleet", *name)
	}

	kube, err := agent.NewKubeClient(*server, *kubeconfig, *namespace, *name)
	if err != nil {
		log.Error("kube client", "err", err)
		os.Exit(1)
	}
	a, err := agent.New(agent.Config{
		Name:              *name,
		Namespace:         *namespace,
		DataDir:           *dataDir,
		PollInterval:      *poll,
		HeartbeatInterval: *heartbeat,
	}, kube, log)
	if err != nil {
		log.Error("agent init", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
}
