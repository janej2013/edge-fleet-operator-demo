package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFirmware(t *testing.T, dir, content string) (path, checksum string) {
	t.Helper()
	p := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return p, hex.EncodeToString(sum[:])
}

// stageAndPromote runs the flash sequence up to (but not including) the
// trial boot, i.e. the point where a real device would reboot.
func stageAndPromote(t *testing.T, s *SlotManager, src, version, checksum string) {
	t.Helper()
	if err := s.Stage(src, version, checksum); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := s.VerifyStaged(); err != nil {
		t.Fatalf("verify staged: %v", err)
	}
	if _, err := s.PromoteStaged(); err != nil {
		t.Fatalf("promote: %v", err)
	}
}

func TestFactoryStateBootsConfirmedSlotA(t *testing.T) {
	s, err := NewSlotManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if slot, _ := s.Active(); slot != "A" {
		t.Fatalf("factory active slot = %s, want A", slot)
	}
	if v := s.ActiveVersion(); v != "factory" {
		t.Fatalf("factory version = %q", v)
	}
	if res, err := s.BootCheck(); err != nil || res != BootNormal {
		t.Fatalf("factory boot = %v/%v, want normal", res, err)
	}
}

func TestHappyPathUpgradeConfirms(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSlotManager(dir)
	src, sum := writeFirmware(t, dir, "firmware v2 bytes")
	stageAndPromote(t, s, src, "2.0.0", sum)

	// "Reboot": bootloader grants one trial.
	res, err := s.BootCheck()
	if err != nil || res != BootTrial {
		t.Fatalf("boot = %v/%v, want trial", res, err)
	}
	if err := s.SelfCheck(); err != nil {
		t.Fatalf("self-check: %v", err)
	}
	if err := s.Confirm(); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	if slot, _ := s.Active(); slot != "B" {
		t.Fatalf("active = %s, want B", slot)
	}
	if v := s.ActiveVersion(); v != "2.0.0" {
		t.Fatalf("version = %q, want 2.0.0", v)
	}
	// Next boot is normal — the new slot is now the trusted one.
	if res, _ := s.BootCheck(); res != BootNormal {
		t.Fatalf("post-confirm boot = %v, want normal", res)
	}
}

func TestCrashBeforeConfirmRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSlotManager(dir)
	src, sum := writeFirmware(t, dir, "firmware v2 bytes")
	stageAndPromote(t, s, src, "2.0.0", sum)

	// First boot consumes the trial attempt... and then the "device" dies
	// before Confirm (simulated by simply not calling it).
	if res, _ := s.BootCheck(); res != BootTrial {
		t.Fatal("expected trial boot")
	}

	// Power-cycle: a fresh SlotManager over the same dir is the new process.
	s2, err := NewSlotManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s2.BootCheck()
	if err != nil {
		t.Fatal(err)
	}
	if res != BootRolledBack {
		t.Fatalf("boot = %v, want rolled-back", res)
	}
	if slot, _ := s2.Active(); slot != "A" {
		t.Fatalf("active = %s, want A (the previous good slot)", slot)
	}
	if v := s2.ActiveVersion(); v != "factory" {
		t.Fatalf("version = %q, want factory", v)
	}
}

func TestChecksumMismatchNeverSwitches(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSlotManager(dir)
	src, _ := writeFirmware(t, dir, "corrupted bytes")

	// Stage with a checksum that does not match the bytes (bit-rot in
	// transit or on flash). VerifyStaged is the brick-safety gate.
	wrongSum := "deadbeef" + strings.Repeat("0", 56) // syntactically valid sha256, wrong value
	if err := s.Stage(src, "2.0.0", wrongSum); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := s.VerifyStaged(); err == nil {
		t.Fatal("corrupted staged firmware passed verification")
	}
	// The invariant that matters: active slot untouched, no switch happened.
	if slot, _ := s.Active(); slot != "A" {
		t.Fatalf("active = %s, want A", slot)
	}
	if res, _ := s.BootCheck(); res != BootNormal {
		t.Fatal("device must still boot the old confirmed slot")
	}
}

func TestSelfCheckFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewSlotManager(dir)
	src, sum := writeFirmware(t, dir, "firmware v2 bytes")
	stageAndPromote(t, s, src, "2.0.0", sum)

	if res, _ := s.BootCheck(); res != BootTrial {
		t.Fatal("expected trial boot")
	}
	// Corrupt the active (trial) slot between boot and self-check.
	active, _ := s.Active()
	if err := os.WriteFile(filepath.Join(s.slotDir(active), firmwareFile), []byte("flipped bits"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SelfCheck(); err == nil {
		t.Fatal("self-check accepted corrupted firmware")
	}
	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
	if slot, _ := s.Active(); slot != "A" {
		t.Fatalf("active = %s, want A after rollback", slot)
	}
	if v := s.ActiveVersion(); v != "factory" {
		t.Fatalf("version = %q, want factory", v)
	}
}
