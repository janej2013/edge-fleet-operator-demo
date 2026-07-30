package agent

import "testing"

func TestFSMTransitions(t *testing.T) {
	tests := []struct {
		name string
		path []State // applied in order from Idle
		ok   bool    // whether the *last* hop must succeed
	}{
		{"happy path full cycle", []State{StateDownloading, StateVerifying, StateFlashing, StateConfirming, StateIdle}, true},
		{"fail during download", []State{StateDownloading, StateFailed}, true},
		{"fail during verify", []State{StateDownloading, StateVerifying, StateFailed}, true},
		{"fail during flash", []State{StateDownloading, StateVerifying, StateFlashing, StateFailed}, true},
		{"fail during confirm", []State{StateDownloading, StateVerifying, StateFlashing, StateConfirming, StateFailed}, true},
		{"retry after failure", []State{StateDownloading, StateFailed, StateDownloading}, true},
		{"cannot skip verification", []State{StateDownloading, StateFlashing}, false},
		{"cannot flash from idle", []State{StateFlashing}, false},
		{"cannot confirm from idle", []State{StateConfirming}, false},
		{"cannot go idle from failed without retrying", []State{StateDownloading, StateFailed, StateIdle}, false},
		{"cannot download twice", []State{StateDownloading, StateDownloading}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFSM(nil)
			var err error
			for i, s := range tt.path {
				err = f.To(s)
				if i < len(tt.path)-1 && err != nil {
					t.Fatalf("setup hop %d to %s failed: %v", i, s, err)
				}
			}
			if tt.ok && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("expected the last transition to be rejected")
			}
		})
	}
}

func TestFSMCallbackFiresOnlyOnValidTransition(t *testing.T) {
	var fired int
	f := NewFSM(func(from, to State) { fired++ })
	if err := f.To(StateDownloading); err != nil {
		t.Fatal(err)
	}
	_ = f.To(StateIdle) // invalid: Downloading → Idle
	if fired != 1 {
		t.Fatalf("callback fired %d times, want 1", fired)
	}
	if f.State() != StateDownloading {
		t.Fatalf("state mutated by invalid transition: %s", f.State())
	}
}
