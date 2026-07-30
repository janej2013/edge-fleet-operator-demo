package agent

import (
	"context"
	"log/slog"
	rand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Name              string
	Namespace         string
	DataDir           string
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	Download          DownloadOptions
}

type Agent struct {
	cfg   Config
	kube  *KubeClient
	slots *SlotManager
	fsm   *FSM
	log   *slog.Logger
	httpc *http.Client

	// Single-flight guard state — see startUpgrade for the design note.
	mu        sync.Mutex
	upgrading bool

	// lastFailedKey dampens retries: a target that just failed is not
	// re-attempted until the Spec actually changes. Without this, a bad
	// checksum in the CR would make every poll re-download the same broken
	// image forever.
	lastFailedKey string

	// lastSeenGen is what we report as observedGeneration: "the Spec
	// generation this agent has acted upon", updated by the poll loop and
	// read by the heartbeat loop (hence atomic).
	lastSeenGen atomic.Int64
}

func New(cfg Config, kube *KubeClient, log *slog.Logger) (*Agent, error) {
	slots, err := NewSlotManager(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, kube: kube, slots: slots, log: log, httpc: &http.Client{}}
	a.fsm = NewFSM(func(from, to State) {
		log.Info("state transition", "from", from, "to", to)
	})
	return a, nil
}

// Run drives the agent until ctx is cancelled.
//
// Sync model: the demo agent *polls* the API server with jitter. Production
// (KubeEdge) is push: the cloud hub streams updates over MQTT/websocket and
// an offline queue replays what the device missed. The semantic difference
// that matters: both are level-based in the end — on reconnect the device
// reads the *latest* desired state, not a backlog of every intermediate
// version. Our poll loop gets that latest-wins property for free, which is
// why polling is an honest simplification rather than a cheat.
func (a *Agent) Run(ctx context.Context) error {
	// "Power-on": run the bootloader logic before anything else. If the
	// process died mid-upgrade last time, this is where the rollback (or
	// the deferred confirm) happens.
	if err := a.bootSequence(); err != nil {
		return err
	}

	go a.heartbeatLoop(ctx)

	for {
		// Jitter spreads N devices' polls across the interval so the API
		// server sees a steady trickle, not a synchronized thundering herd.
		interval := a.cfg.PollInterval/2 + rand.N(a.cfg.PollInterval)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
		dev, err := a.kube.Get(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.log.Warn("poll failed", "err", err)
			continue
		}
		a.evaluate(ctx, dev)
	}
}

// bootSequence mirrors what a real device does after power loss at any
// arbitrary instant — the crash-consistency story in one place.
func (a *Agent) bootSequence() error {
	result, err := a.slots.BootCheck()
	if err != nil {
		return err
	}
	switch result {
	case BootTrial:
		// We died after the symlink flip but before Confirm. Finish the
		// job now: self-check decides confirm vs rollback.
		if err := a.slots.SelfCheck(); err != nil {
			a.log.Warn("boot self-check failed, rolling back", "err", err)
			return a.slots.Rollback()
		}
		if err := a.slots.Confirm(); err != nil {
			return err
		}
		a.log.Info("recovered trial slot confirmed on boot")
	case BootRolledBack:
		a.log.Warn("bootloader rolled back to previous slot (unconfirmed trial)")
	}
	active, _ := a.slots.Active()
	a.log.Info("booted", "slot", active, "version", a.slots.ActiveVersion())
	return nil
}

// evaluate compares desired vs actual and decides what, if anything, to do.
// It never blocks: upgrades run in their own goroutine behind the guard.
func (a *Agent) evaluate(ctx context.Context, dev *Device) {
	a.lastSeenGen.Store(dev.Metadata.Generation)
	current := a.slots.ActiveVersion()

	if dev.Spec.DesiredFirmwareVersion == current {
		a.syncSteadyStatus(ctx, dev, current)
		return
	}
	if dev.Spec.RolloutPaused {
		// Pause blocks *starting* transactions only. An in-flight one is
		// allowed to finish: interrupting a half-flashed device is exactly
		// the failure mode A/B exists to prevent.
		a.log.Info("rollout paused, holding", "current", current, "desired", dev.Spec.DesiredFirmwareVersion)
		return
	}
	key := dev.Spec.DesiredFirmwareVersion + "|" + dev.Spec.ChecksumSHA256
	a.mu.Lock()
	failed := a.lastFailedKey == key
	a.mu.Unlock()
	if failed {
		return // this exact target already failed; wait for a new Spec
	}
	a.startUpgrade(ctx, dev)
}

// syncSteadyStatus makes sure the API reflects reality when no upgrade is
// needed (first boot, post-rollback, post-restart). Writes only on drift —
// the agent-side equivalent of the operator's "gate 1".
func (a *Agent) syncSteadyStatus(ctx context.Context, dev *Device, current string) {
	slot, _ := a.slots.Active()
	if dev.Status.CurrentFirmwareVersion == current && dev.Status.ActiveSlot == slot {
		return
	}
	a.reportStatus(ctx, func(st *DeviceStatus, gen int64) {
		st.CurrentFirmwareVersion = current
		st.ActiveSlot = slot
		SetCondition(st, CondFirmwareSynced, CondTrue, "Synced", "device firmware matches desired version", gen)
	})
}

// startUpgrade is the single-flight guard.
//
// Design choice: mutex + bool flag, not a buffered channel. A channel of
// pending targets would *queue* intents, but intermediate targets are
// worthless — if the spec moved v1→v2→v3 while we were flashing v1, flashing
// v2 next would be wasted flash cycles. The CR itself is already a mailbox
// that always holds only the latest intent, so the guard's only job is "at
// most one transaction at a time"; the next poll after we finish reads
// whatever is newest. Latest-wins comes for free from level-based sync.
func (a *Agent) startUpgrade(ctx context.Context, dev *Device) {
	a.mu.Lock()
	if a.upgrading {
		a.mu.Unlock()
		a.log.Info("upgrade in flight; newer target will be evaluated after it finishes",
			"desired", dev.Spec.DesiredFirmwareVersion)
		return
	}
	a.upgrading = true
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.upgrading = false
			a.mu.Unlock()
		}()
		a.runUpgrade(ctx, dev.Spec, dev.Metadata.Generation)
	}()
}

// runUpgrade executes one full firmware transaction:
// Downloading → Verifying → Flashing → Confirming → Idle (or Failed).
func (a *Agent) runUpgrade(ctx context.Context, spec DeviceSpec, gen int64) {
	version := spec.DesiredFirmwareVersion
	log := a.log.With("target", version)
	log.Info("upgrade transaction started", "url", spec.FirmwareURL)

	fail := func(state State, reason, msg string) {
		_ = a.fsm.To(StateFailed)
		a.mu.Lock()
		a.lastFailedKey = version + "|" + spec.ChecksumSHA256
		a.mu.Unlock()
		log.Error("upgrade failed", "at", state, "reason", reason, "err", msg)
		a.reportStatus(ctx, func(st *DeviceStatus, g int64) {
			SetCondition(st, CondFirmwareSynced, CondFalse, reason, msg, g)
		})
	}

	// Announce the transaction so the fleet view shows progress, not silence.
	a.reportStatus(ctx, func(st *DeviceStatus, g int64) {
		SetCondition(st, CondFirmwareSynced, CondFalse, "UpgradeInProgress", "downloading "+version, g)
	})

	// -- Downloading ------------------------------------------------------
	if err := a.fsm.To(StateDownloading); err != nil {
		log.Error("fsm", "err", err)
		return
	}
	// Staging file is keyed by checksum so a leftover partial download from
	// a *different* target can never be resumed into this one.
	staging := filepath.Join(a.cfg.DataDir, "staging-"+spec.ChecksumSHA256[:12]+".bin")
	if err := DownloadWithResume(ctx, a.httpc, spec.FirmwareURL, staging, a.cfg.Download); err != nil {
		fail(StateDownloading, "DownloadFailed", err.Error())
		return
	}

	// -- Verifying ----------------------------------------------------------
	if err := a.fsm.To(StateVerifying); err != nil {
		return
	}
	if err := VerifySHA256(staging, spec.ChecksumSHA256); err != nil {
		os.Remove(staging) // poisoned bytes have no resume value
		fail(StateVerifying, "ChecksumMismatch", err.Error())
		return
	}

	// -- Flashing -----------------------------------------------------------
	if err := a.fsm.To(StateFlashing); err != nil {
		return
	}
	if err := a.slots.Stage(staging, version, spec.ChecksumSHA256); err != nil {
		fail(StateFlashing, "FlashFailed", err.Error())
		return
	}
	if err := a.slots.VerifyStaged(); err != nil {
		fail(StateFlashing, "FlashVerifyFailed", err.Error())
		return
	}
	if _, err := a.slots.PromoteStaged(); err != nil {
		fail(StateFlashing, "PromoteFailed", err.Error())
		return
	}

	// -- Confirming: simulated reboot into the new slot ---------------------
	if err := a.fsm.To(StateConfirming); err != nil {
		return
	}
	log.Info("simulating reboot into new slot")
	boot, err := a.slots.BootCheck()
	if err != nil || boot != BootTrial {
		fail(StateConfirming, "BootFailed", "unexpected boot result: "+string(boot))
		return
	}
	if err := a.slots.SelfCheck(); err != nil {
		_ = a.slots.Rollback()
		fail(StateConfirming, "SelfCheckFailed", err.Error())
		return
	}
	if err := a.slots.Confirm(); err != nil {
		fail(StateConfirming, "ConfirmFailed", err.Error())
		return
	}
	os.Remove(staging)

	// -- Done ---------------------------------------------------------------
	if err := a.fsm.To(StateIdle); err != nil {
		return
	}
	slot, _ := a.slots.Active()
	log.Info("upgrade complete", "slot", slot)
	a.reportStatus(ctx, func(st *DeviceStatus, g int64) {
		st.CurrentFirmwareVersion = version
		st.ActiveSlot = slot
		SetCondition(st, CondFirmwareSynced, CondTrue, "Synced", "upgraded to "+version, g)
	})
}

// heartbeatLoop is deliberately independent of the upgrade goroutine: a
// device busy downloading for 10 minutes must still look alive.
func (a *Agent) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reportStatus(ctx, func(st *DeviceStatus, gen int64) {
				st.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
			})
		}
	}
}

// reportStatus funnels every status write through the conflict-retrying
// read-modify-write in UpdateStatus, and stamps the bookkeeping fields every
// writer must maintain.
func (a *Agent) reportStatus(ctx context.Context, mutate func(*DeviceStatus, int64)) {
	_, err := a.kube.UpdateStatus(ctx, func(dev *Device) {
		gen := a.lastSeenGen.Load()
		mutate(&dev.Status, gen)
		dev.Status.ObservedGeneration = gen
	})
	if err != nil && ctx.Err() == nil {
		a.log.Warn("status update failed", "err", err)
	}
}
