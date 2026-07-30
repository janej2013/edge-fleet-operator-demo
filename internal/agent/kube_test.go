package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeAPI is a minimal API-server stand-in with real optimistic concurrency:
// PUTs with a stale resourceVersion get 409, exactly like the real thing.
type fakeAPI struct {
	mu   sync.Mutex
	dev  Device
	rv   int
	gets int
	puts int
	c409 int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		rv: 100,
		dev: Device{
			Metadata: ObjectMeta{Name: "dev-0", Namespace: "default", Generation: 1},
			Spec:     DeviceSpec{DesiredFirmwareVersion: "1.0.0"},
		},
	}
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			f.gets++
			f.dev.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			json.NewEncoder(w).Encode(f.dev)
		case http.MethodPut:
			f.puts++
			var in Device
			json.NewDecoder(r.Body).Decode(&in)
			if in.Metadata.ResourceVersion != strconv.Itoa(f.rv) {
				f.c409++
				w.WriteHeader(http.StatusConflict)
				return
			}
			f.rv++
			f.dev.Status = in.Status
			f.dev.Metadata.ResourceVersion = strconv.Itoa(f.rv)
			json.NewEncoder(w).Encode(f.dev)
		}
	})
}

func (f *fakeAPI) bumpSpecExternally(version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dev.Spec.DesiredFirmwareVersion = version
	f.dev.Metadata.Generation++
	f.rv++ // any write moves the resourceVersion — this is what stales old reads
}

func TestUpdateStatusRetriesOnConflict(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	kube, err := NewKubeClient(srv.URL, "", "default", "dev-0")
	if err != nil {
		t.Fatal(err)
	}

	// Read happens, then the spec changes underneath us (rv bumps), so the
	// first PUT must 409 and UpdateStatus must re-read and win on retry.
	first := true
	dev, err := kube.UpdateStatus(context.Background(), func(d *Device) {
		if first {
			first = false
			api.bumpSpecExternally("2.0.0") // interleave AFTER our GET
		}
		d.Status.CurrentFirmwareVersion = "1.0.0"
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if api.c409 != 1 {
		t.Fatalf("expected exactly 1 conflict, got %d", api.c409)
	}
	// The winning write was applied to the *fresh* object: the mutation
	// survived, and the re-read saw the externally-updated spec.
	if dev.Status.CurrentFirmwareVersion != "1.0.0" {
		t.Fatal("mutation lost after conflict retry")
	}
	if dev.Spec.DesiredFirmwareVersion != "2.0.0" {
		t.Fatal("retry did not re-read the latest object")
	}
	if api.gets != 2 || api.puts != 2 {
		t.Fatalf("expected GET,PUT(409),GET,PUT — got %d GETs / %d PUTs", api.gets, api.puts)
	}
}

func TestSetConditionTransitionTimeSemantics(t *testing.T) {
	var st DeviceStatus
	SetCondition(&st, CondFirmwareSynced, CondFalse, "UpgradeInProgress", "downloading", 1)
	t0 := st.Conditions[0].LastTransitionTime

	// Same status value → transition time must NOT move.
	SetCondition(&st, CondFirmwareSynced, CondFalse, "DownloadFailed", "gave up", 1)
	if st.Conditions[0].LastTransitionTime != t0 {
		t.Fatal("transition time bumped without a status flip")
	}
	if st.Conditions[0].Reason != "DownloadFailed" {
		t.Fatal("reason not updated")
	}
	if len(st.Conditions) != 1 {
		t.Fatalf("condition duplicated: %d entries", len(st.Conditions))
	}
}
