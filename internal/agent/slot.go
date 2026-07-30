package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A/B slot simulation.
//
// Real devices have two firmware partitions; the *bootloader* (U-Boot
// bootcount, Android boot_control, ...) decides which one to boot and rolls
// back if a freshly-flashed slot fails to come up. Software's responsibility
// ends at: flash the inactive slot, mark it "try me once", and confirm after
// a successful self-check. We model exactly that contract on a filesystem:
//
//	<dir>/slotA/, slotB/   — the two "partitions" (firmware.bin, version, checksum)
//	<dir>/current          — symlink to the booted slot (the bootloader's choice)
//	<slot>/.pending        — "try this slot once"; the file content is the
//	                         boot-attempt counter (the bootloader's bootcount)
//	<slot>/.confirmed      — self-check passed, slot is a safe rollback target
//
// The symlink flip via rename() is the one atomic primitive: a crash at any
// point leaves `current` pointing at a complete slot, never at garbage —
// which is the whole point of A/B over in-place updates.

const (
	firmwareFile    = "firmware.bin"
	versionFile     = "version"
	checksumFile    = "checksum"
	pendingMarker   = ".pending"
	confirmedMarker = ".confirmed"
	currentLink     = "current"
)

type BootResult string

const (
	BootNormal     BootResult = "normal"      // confirmed slot, nothing to do
	BootTrial      BootResult = "trial"       // first boot of a new slot: self-check + Confirm, or Rollback
	BootRolledBack BootResult = "rolled-back" // trial slot burned its attempt → we reverted
)

type SlotManager struct {
	dir string
}

func NewSlotManager(dir string) (*SlotManager, error) {
	s := &SlotManager{dir: dir}
	for _, slot := range []string{"A", "B"} {
		if err := os.MkdirAll(s.slotDir(slot), 0o755); err != nil {
			return nil, err
		}
	}
	// Factory state: a fresh device boots slot A and trusts it (it shipped
	// with the device), so A starts confirmed and version "factory".
	if _, err := os.Lstat(s.currentPath()); os.IsNotExist(err) {
		if err := os.WriteFile(filepath.Join(s.slotDir("A"), versionFile), []byte("factory"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(s.slotDir("A"), confirmedMarker), nil, 0o644); err != nil {
			return nil, err
		}
		if err := s.switchTo("A"); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *SlotManager) slotDir(slot string) string { return filepath.Join(s.dir, "slot"+slot) }
func (s *SlotManager) currentPath() string        { return filepath.Join(s.dir, currentLink) }

func other(slot string) string {
	if slot == "A" {
		return "B"
	}
	return "A"
}

// Active returns "A" or "B" by reading the current symlink.
func (s *SlotManager) Active() (string, error) {
	target, err := os.Readlink(s.currentPath())
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(filepath.Base(target), "slot"), nil
}

func (s *SlotManager) ActiveVersion() string {
	active, err := s.Active()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(s.slotDir(active), versionFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Stage writes the new firmware into the *inactive* slot. The running slot is
// never touched — that invariant, not any amount of testing, is what makes
// the update un-brickable.
func (s *SlotManager) Stage(srcPath, version, checksum string) error {
	active, err := s.Active()
	if err != nil {
		return err
	}
	dir := s.slotDir(other(active))
	// The slot is dirty until proven good: drop old markers first so a crash
	// mid-stage can never leave a half-written slot looking bootable.
	os.Remove(filepath.Join(dir, confirmedMarker))
	os.Remove(filepath.Join(dir, pendingMarker))

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(dir, firmwareFile))
	if err != nil {
		return err
	}
	if _, err := copyAndClose(dst, src); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, versionFile), []byte(version), 0o644); err != nil {
		return err
	}
	// Persist the expected checksum *in the slot*: after a crash+restart the
	// self-check must not depend on anything held in memory.
	return os.WriteFile(filepath.Join(dir, checksumFile), []byte(checksum), 0o644)
}

// VerifyStaged re-hashes what actually landed in the inactive slot. The
// download was already verified, but flash writes can corrupt too (worn
// eMMC, power dips) — verify at every boundary where bits moved.
func (s *SlotManager) VerifyStaged() error {
	active, err := s.Active()
	if err != nil {
		return err
	}
	dir := s.slotDir(other(active))
	want, err := os.ReadFile(filepath.Join(dir, checksumFile))
	if err != nil {
		return err
	}
	return VerifySHA256(filepath.Join(dir, firmwareFile), strings.TrimSpace(string(want)))
}

// PromoteStaged marks the staged slot "try once" and atomically points the
// bootloader at it. Order matters: the pending marker must exist *before*
// the flip, so there is no window where the new slot boots unmarked (which
// BootCheck would treat as corruption and roll back).
func (s *SlotManager) PromoteStaged() (string, error) {
	active, err := s.Active()
	if err != nil {
		return "", err
	}
	target := other(active)
	if err := os.WriteFile(filepath.Join(s.slotDir(target), pendingMarker), []byte("0"), 0o644); err != nil {
		return "", err
	}
	return target, s.switchTo(target)
}

// switchTo flips the current symlink atomically: build the new link under a
// temp name, then rename() over the old one. Readers see the old slot or the
// new slot, never a missing/broken link.
func (s *SlotManager) switchTo(slot string) error {
	tmp := s.currentPath() + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink("slot"+slot, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, s.currentPath())
}

// BootCheck is "the bootloader": call it at every (simulated) boot before
// anything else. It implements try-once semantics via the attempt counter in
// the pending marker — exactly what U-Boot's bootcount does.
func (s *SlotManager) BootCheck() (BootResult, error) {
	active, err := s.Active()
	if err != nil {
		return "", err
	}
	dir := s.slotDir(active)

	if _, err := os.Stat(filepath.Join(dir, confirmedMarker)); err == nil {
		return BootNormal, nil
	}

	pendingPath := filepath.Join(dir, pendingMarker)
	b, err := os.ReadFile(pendingPath)
	if err != nil {
		// Neither confirmed nor pending: the slot was never legitimately
		// promoted. Treat as corruption and fall back to the other slot.
		return BootRolledBack, s.rollback(active)
	}
	tries, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if tries >= 1 {
		// The previous trial boot died before Confirm — the new firmware is
		// presumed bad. Burn it and revert. This is the crash-safety net:
		// "if in doubt, boot what worked yesterday."
		return BootRolledBack, s.rollback(active)
	}
	// First trial: spend the attempt, let the agent self-check and Confirm.
	if err := os.WriteFile(pendingPath, []byte(strconv.Itoa(tries+1)), 0o644); err != nil {
		return "", err
	}
	return BootTrial, nil
}

// SelfCheck is what "the firmware booted and works" means in this demo:
// the active slot's image re-hashes to the checksum it shipped with.
func (s *SlotManager) SelfCheck() error {
	active, err := s.Active()
	if err != nil {
		return err
	}
	dir := s.slotDir(active)
	want, err := os.ReadFile(filepath.Join(dir, checksumFile))
	if err != nil {
		return fmt.Errorf("no checksum in active slot: %w", err)
	}
	return VerifySHA256(filepath.Join(dir, firmwareFile), strings.TrimSpace(string(want)))
}

// Confirm seals a successful trial boot: only now does the new slot become
// the rollback target for the *next* upgrade.
func (s *SlotManager) Confirm() error {
	active, err := s.Active()
	if err != nil {
		return err
	}
	dir := s.slotDir(active)
	if err := os.WriteFile(filepath.Join(dir, confirmedMarker), nil, 0o644); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, pendingMarker))
}

// Rollback reverts to the other slot after a failed trial.
func (s *SlotManager) Rollback() error {
	active, err := s.Active()
	if err != nil {
		return err
	}
	return s.rollback(active)
}

func (s *SlotManager) rollback(badSlot string) error {
	os.Remove(filepath.Join(s.slotDir(badSlot), pendingMarker))
	return s.switchTo(other(badSlot))
}

func copyAndClose(dst *os.File, src *os.File) (int64, error) {
	n, err := dst.ReadFrom(src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return n, err
}
