package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Why not client-go? A cache-backed client-go informer is the right tool for
// a controller, but it drags ~40MB of dependencies into a binary that has to
// live on a 128MB flash chip. The agent needs exactly three verbs against one
// resource — GET, PUT /status, and patience — and the K8s API is just JSON
// over HTTP. Hand-rolling those three calls keeps the agent 100% stdlib.
//
// The structs below deliberately re-declare (a lean view of) the CRD instead
// of importing api/v1alpha1: the wire contract is JSON, not Go types, and a
// real embedded agent (often C/Rust) could never share the operator's types
// anyway. Loose coupling here is honesty about the deployment boundary.

type Device struct {
	APIVersion string       `json:"apiVersion,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	Metadata   ObjectMeta   `json:"metadata"`
	Spec       DeviceSpec   `json:"spec"`
	Status     DeviceStatus `json:"status,omitempty"`
}

type ObjectMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Generation      int64  `json:"generation,omitempty"`
	UID             string `json:"uid,omitempty"`
}

type DeviceSpec struct {
	DesiredFirmwareVersion string            `json:"desiredFirmwareVersion"`
	FirmwareURL            string            `json:"firmwareURL"`
	ChecksumSHA256         string            `json:"checksumSHA256"`
	Region                 string            `json:"region,omitempty"`
	DeviceLabels           map[string]string `json:"deviceLabels,omitempty"`
	RolloutPaused          bool              `json:"rolloutPaused,omitempty"`
}

type DeviceStatus struct {
	Phase                  string      `json:"phase,omitempty"`
	Conditions             []Condition `json:"conditions,omitempty"`
	CurrentFirmwareVersion string      `json:"currentFirmwareVersion,omitempty"`
	ActiveSlot             string      `json:"activeSlot,omitempty"`
	LastHeartbeat          string      `json:"lastHeartbeat,omitempty"`
	ObservedGeneration     int64       `json:"observedGeneration,omitempty"`
}

// Condition mirrors metav1.Condition's JSON shape.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
}

const (
	CondFirmwareSynced = "FirmwareSynced"
	CondTrue           = "True"
	CondFalse          = "False"
)

// ErrConflict marks an optimistic-concurrency loss (HTTP 409): our write was
// based on a resourceVersion that is no longer the latest. It is the one
// error class the caller must NOT retry blindly with the same payload — the
// only correct reaction is re-read, re-decide, re-write.
var ErrConflict = errors.New("resource version conflict (409)")

type KubeClient struct {
	base      string // e.g. https://127.0.0.1:6443
	namespace string
	name      string
	httpc     *http.Client
	// Log, when set, narrates conflict handling — the 409 dance is the
	// point of the demo, so it deserves a log line you can grep for.
	Log *slog.Logger
}

func NewKubeClient(server, kubeconfig, namespace, name string) (*KubeClient, error) {
	c := &KubeClient{base: server, namespace: namespace, name: name, httpc: &http.Client{Timeout: 30 * time.Second}}
	if kubeconfig != "" {
		server, tlsCfg, err := loadKubeconfig(kubeconfig)
		if err != nil {
			return nil, err
		}
		c.base = server
		c.httpc.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	if c.base == "" {
		return nil, errors.New("either --server or --kubeconfig is required")
	}
	return c, nil
}

func (c *KubeClient) url(sub string) string {
	return fmt.Sprintf("%s/apis/edge.example.com/v1alpha1/namespaces/%s/edgedevices/%s%s",
		strings.TrimSuffix(c.base, "/"), c.namespace, c.name, sub)
}

func (c *KubeClient) Get(ctx context.Context) (*Device, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(""), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s: %s", c.url(""), resp.Status, body)
	}
	var dev Device
	if err := json.NewDecoder(resp.Body).Decode(&dev); err != nil {
		return nil, err
	}
	return &dev, nil
}

// PutStatus writes the status subresource using the resourceVersion carried
// inside dev — i.e. "apply my update only if the object is still the one I
// read". That precondition is the entire optimistic-concurrency mechanism.
func (c *KubeClient) PutStatus(ctx context.Context, dev *Device) error {
	dev.APIVersion = "edge.example.com/v1alpha1"
	dev.Kind = "EdgeDevice"
	body, err := json.Marshal(dev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url("/status"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		return ErrConflict
	case resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT status: %s: %s", resp.Status, b)
	}
	return nil
}

// UpdateStatus is the read-modify-write loop around PutStatus.
//
// A 409 here is not an error, it is a *signal that the latest state must
// win*: someone (the operator, a rollout controller) changed the object
// after we read it. Our stale in-memory copy is worthless — so we throw it
// away, GET the fresh object, re-apply the mutation to fresh state, and try
// again. Retrying the old payload would silently overwrite their write.
func (c *KubeClient) UpdateStatus(ctx context.Context, mutate func(*Device)) (*Device, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		dev, err := c.Get(ctx)
		if err != nil {
			return nil, err
		}
		mutate(dev)
		if err := c.PutStatus(ctx, dev); err != nil {
			if errors.Is(err, ErrConflict) {
				if c.Log != nil {
					c.Log.Info("status write conflicted (409): discarding stale view, re-reading latest",
						"attempt", attempt+1, "staleResourceVersion", dev.Metadata.ResourceVersion)
				}
				lastErr = err
				continue // stale view discarded; loop re-reads the winner
			}
			return nil, err
		}
		return dev, nil
	}
	return nil, fmt.Errorf("giving up after repeated conflicts: %w", lastErr)
}

// SetCondition upserts a condition, bumping LastTransitionTime only when the
// status value actually flips (same contract as metav1 helpers).
func SetCondition(st *DeviceStatus, ctype, status, reason, message string, gen int64) {
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range st.Conditions {
		if st.Conditions[i].Type == ctype {
			if st.Conditions[i].Status != status {
				st.Conditions[i].LastTransitionTime = now
			}
			st.Conditions[i].Status = status
			st.Conditions[i].Reason = reason
			st.Conditions[i].Message = message
			st.Conditions[i].ObservedGeneration = gen
			return
		}
	}
	st.Conditions = append(st.Conditions, Condition{
		Type: ctype, Status: status, Reason: reason, Message: message,
		ObservedGeneration: gen, LastTransitionTime: now,
	})
}

// loadKubeconfig extracts the four fields we need (server, CA, client cert,
// client key) from a kubeconfig. It is deliberately NOT a YAML parser: it
// handles the flat single-cluster file that `kind` and kubeadm emit, which is
// exactly the demo's scope. Production agents don't use kubeconfigs at all —
// they get a per-device token via an enrollment flow (see the credential
// Secret the operator provisions).
func loadKubeconfig(path string) (string, *tls.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		for _, key := range []string{"server", "certificate-authority-data", "client-certificate-data", "client-key-data"} {
			if v, ok := strings.CutPrefix(line, key+": "); ok {
				if _, seen := fields[key]; !seen { // first cluster/user wins
					fields[key] = strings.TrimSpace(v)
				}
			}
		}
	}
	server := fields["server"]
	if server == "" {
		return "", nil, fmt.Errorf("no server found in %s", path)
	}
	if strings.HasPrefix(server, "http://") {
		return server, nil, nil
	}
	dec := func(k string) ([]byte, error) { return base64.StdEncoding.DecodeString(fields[k]) }
	ca, err := dec("certificate-authority-data")
	if err != nil {
		return "", nil, fmt.Errorf("bad CA data: %w", err)
	}
	cert, err := dec("client-certificate-data")
	if err != nil {
		return "", nil, fmt.Errorf("bad client cert: %w", err)
	}
	key, err := dec("client-key-data")
	if err != nil {
		return "", nil, fmt.Errorf("bad client key: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return "", nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return "", nil, errors.New("failed to parse CA certificate")
	}
	return server, &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}}, nil
}
