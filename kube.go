package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kube is a tiny read-only client for the Kubernetes API, used from inside the pod
// with its own ServiceAccount (see k8s/10-rbac.yaml). No client library: plain HTTPS.
type kube struct {
	base      string
	namespace string
	tokenPath string
	client    *http.Client

	mu       sync.Mutex
	cached   fleet
	cachedAt time.Time
	server   string // Kubernetes version, read once
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// newKubeInCluster returns nil outside a Kubernetes pod (local run).
func newKubeInCluster() (*kube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, nil
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	ns, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &kube{
		base:      "https://" + net.JoinHostPort(host, port),
		namespace: strings.TrimSpace(string(ns)),
		tokenPath: serviceAccountDir + "/token",
		client: &http.Client{
			Timeout:   3 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

func (k *kube) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+path, nil)
	if err != nil {
		return err
	}
	if k.tokenPath != "" {
		// Re-read on every call: the projected token is rotated by the kubelet.
		token, err := os.ReadFile(k.tokenPath)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	res, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// --- What the page shows ---------------------------------------------------

type fleet struct {
	Namespace  string          `json:"namespace"`
	Deployment *deploymentView `json:"deployment,omitempty"`
	HPA        *hpaView        `json:"hpa,omitempty"`
	Cranes     []crane         `json:"cranes"`
	Rush       *rushView       `json:"rush,omitempty"`
	Events     []event         `json:"events"`
	Metrics    bool            `json:"metrics"`
	Kubernetes string          `json:"kubernetes,omitempty"`
	Local      bool            `json:"local,omitempty"`
	At         time.Time       `json:"at"`
}

type deploymentView struct {
	Revision  string `json:"revision"`
	Image     string `json:"image"`
	Desired   int    `json:"desired"`
	Updated   int    `json:"updated"`
	Ready     int    `json:"ready"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
	Rolling   bool   `json:"rolling"`
	Stalled   string `json:"stalled,omitempty"`
}

type hpaView struct {
	Min     int  `json:"min"`
	Max     int  `json:"max"`
	Current int  `json:"current"`
	Desired int  `json:"desired"`
	CPU     *int `json:"cpu,omitempty"`
	Target  int  `json:"target"`
}

type crane struct {
	Name        string    `json:"name"`
	Hash        string    `json:"hash"`
	Node        string    `json:"node"`
	Phase       string    `json:"phase"`
	Ready       bool      `json:"ready"`
	Terminating bool      `json:"terminating"`
	Started     time.Time `json:"started"`
	Image       string    `json:"image"`
	Restarts    int       `json:"restarts"`
	Waiting     string    `json:"waiting,omitempty"` // why the container is not running yet (ContainerCreating, ImagePullBackOff…)
	CPU         *int      `json:"cpu,omitempty"`     // millicores, from metrics.k8s.io
	Memory      *int      `json:"memory,omitempty"`
	Request     int       `json:"request,omitempty"` // CPU request in millicores (what the HPA divides by)
}

type rushView struct {
	Pods    int    `json:"pods"`
	Running int    `json:"running"`
	RPS     string `json:"rps"`
}

type event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Object  string    `json:"object"`
	Message string    `json:"message"`
	Count   int       `json:"count"`
}

// --- Raw API shapes (only the fields we read) --------------------------------

type meta struct {
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
}

type podList struct {
	Items []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Args      []string `json:"args"`
				Resources struct {
					Requests struct {
						CPU string `json:"cpu"`
					} `json:"requests"`
				} `json:"resources"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				ImageID      string `json:"imageID"`
				RestartCount int    `json:"restartCount"`
				State        struct {
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type deployment struct {
	Metadata meta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas          int `json:"replicas"`
		UpdatedReplicas   int `json:"updatedReplicas"`
		ReadyReplicas     int `json:"readyReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
		Conditions        []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type hpa struct {
	Spec struct {
		MinReplicas *int `json:"minReplicas"`
		MaxReplicas int  `json:"maxReplicas"`
		Metrics     []struct {
			Resource *struct {
				Target struct {
					AverageUtilization *int `json:"averageUtilization"`
				} `json:"target"`
			} `json:"resource"`
		} `json:"metrics"`
	} `json:"spec"`
	Status struct {
		CurrentReplicas int `json:"currentReplicas"`
		DesiredReplicas int `json:"desiredReplicas"`
		CurrentMetrics  []struct {
			Resource *struct {
				Current struct {
					AverageUtilization *int `json:"averageUtilization"`
				} `json:"current"`
			} `json:"resource"`
		} `json:"currentMetrics"`
	} `json:"status"`
}

type eventList struct {
	Items []struct {
		Metadata       meta       `json:"metadata"`
		Type           string     `json:"type"`
		Reason         string     `json:"reason"`
		Message        string     `json:"message"`
		Count          int        `json:"count"`
		LastTimestamp  *time.Time `json:"lastTimestamp"`
		EventTime      *time.Time `json:"eventTime"`
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
	} `json:"items"`
}

type podMetricsList struct {
	Items []struct {
		Metadata   meta `json:"metadata"`
		Containers []struct {
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// fleet reads pods, the Deployment, the HPA, events and pod metrics in parallel.
// The result is cached for a second: every open page polls it.
func (k *kube) fleet(ctx context.Context, app string) (fleet, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Since(k.cachedAt) < time.Second {
		return k.cached, nil
	}
	ns := "/namespaces/" + k.namespace
	var (
		pods    podList
		dep     deployment
		scaler  hpa
		events  eventList
		metrics podMetricsList
		wg      sync.WaitGroup
		errs    [5]error
	)
	calls := []func() error{
		func() error { return k.get(ctx, "/api/v1"+ns+"/pods", &pods) },
		func() error { return k.get(ctx, "/apis/apps/v1"+ns+"/deployments/"+app, &dep) },
		func() error { return k.get(ctx, "/apis/autoscaling/v2"+ns+"/horizontalpodautoscalers/"+app, &scaler) },
		func() error { return k.get(ctx, "/api/v1"+ns+"/events", &events) },
		func() error { return k.get(ctx, "/apis/metrics.k8s.io/v1beta1"+ns+"/pods", &metrics) },
	}
	for i, call := range calls {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = call() }()
	}
	wg.Wait()
	if errs[0] != nil {
		return fleet{}, errs[0]
	}

	if k.server == "" {
		var v struct {
			GitVersion string `json:"gitVersion"`
		}
		if k.get(ctx, "/version", &v) == nil {
			k.server = v.GitVersion
		}
	}
	f := fleet{Namespace: k.namespace, Kubernetes: k.server, Cranes: []crane{}, Events: []event{}, Metrics: errs[4] == nil, At: time.Now()}
	usage := map[string][2]int{}
	for _, m := range metrics.Items {
		var cpu, mem int
		for _, c := range m.Containers {
			cpu += milliCPU(c.Usage.CPU)
			mem += mebibytes(c.Usage.Memory)
		}
		usage[m.Metadata.Name] = [2]int{cpu, mem}
	}

	rush := rushView{}
	for _, p := range pods.Items {
		switch p.Metadata.Labels["app.kubernetes.io/name"] {
		case app:
			c := crane{
				Name:        p.Metadata.Name,
				Hash:        p.Metadata.Labels["pod-template-hash"],
				Node:        p.Spec.NodeName,
				Phase:       p.Status.Phase,
				Terminating: p.Metadata.DeletionTimestamp != nil,
				Started:     p.Metadata.CreationTimestamp,
			}
			for _, cond := range p.Status.Conditions {
				if cond.Type == "Ready" {
					c.Ready = cond.Status == "True"
				}
			}
			if len(p.Spec.Containers) > 0 {
				c.Request = milliCPU(p.Spec.Containers[0].Resources.Requests.CPU)
			}
			if len(p.Status.ContainerStatuses) > 0 {
				c.Image = shortDigest(p.Status.ContainerStatuses[0].ImageID)
				c.Restarts = p.Status.ContainerStatuses[0].RestartCount
				if w := p.Status.ContainerStatuses[0].State.Waiting; w != nil {
					c.Waiting = w.Reason
				}
			} else if p.Status.Phase == "Pending" {
				c.Waiting = "Scheduling"
			}
			if u, ok := usage[c.Name]; ok {
				c.CPU, c.Memory = &u[0], &u[1]
			}
			f.Cranes = append(f.Cranes, c)
		case "rush-hour":
			rush.Pods++
			if p.Status.Phase == "Running" || p.Status.Phase == "Pending" {
				rush.Running++
			}
			if len(p.Spec.Containers) > 0 {
				rush.RPS = flagValue(p.Spec.Containers[0].Args, "-rps")
			}
		}
	}
	if rush.Pods > 0 {
		f.Rush = &rush
	}
	sort.Slice(f.Cranes, func(i, j int) bool { return f.Cranes[i].Started.Before(f.Cranes[j].Started) })

	if errs[1] == nil {
		d := &deploymentView{
			Revision:  dep.Metadata.Annotations["deployment.kubernetes.io/revision"],
			Updated:   dep.Status.UpdatedReplicas,
			Ready:     dep.Status.ReadyReplicas,
			Available: dep.Status.AvailableReplicas,
			Total:     dep.Status.Replicas,
		}
		if dep.Spec.Replicas != nil {
			d.Desired = *dep.Spec.Replicas
		}
		if len(dep.Spec.Template.Spec.Containers) > 0 {
			d.Image = dep.Spec.Template.Spec.Containers[0].Image
		}
		// A rollout is running while pods of an older template remain. A scale-up
		// (HPA) adds pods of the current template: it is not a rollout.
		d.Rolling = d.Total > d.Updated
		for _, cond := range dep.Status.Conditions {
			if cond.Type == "Progressing" && cond.Reason == "ProgressDeadlineExceeded" {
				d.Stalled = cond.Message
			}
		}
		f.Deployment = d
	}
	if errs[2] == nil {
		h := &hpaView{Max: scaler.Spec.MaxReplicas, Current: scaler.Status.CurrentReplicas, Desired: scaler.Status.DesiredReplicas, Min: 1}
		if scaler.Spec.MinReplicas != nil {
			h.Min = *scaler.Spec.MinReplicas
		}
		for _, m := range scaler.Spec.Metrics {
			if m.Resource != nil && m.Resource.Target.AverageUtilization != nil {
				h.Target = *m.Resource.Target.AverageUtilization
			}
		}
		for _, m := range scaler.Status.CurrentMetrics {
			if m.Resource != nil && m.Resource.Current.AverageUtilization != nil {
				h.CPU = m.Resource.Current.AverageUtilization
			}
		}
		f.HPA = h
	}
	if errs[3] == nil {
		for _, e := range events.Items {
			at := e.Metadata.CreationTimestamp
			if e.EventTime != nil && !e.EventTime.IsZero() {
				at = *e.EventTime
			}
			if e.LastTimestamp != nil && !e.LastTimestamp.IsZero() {
				at = *e.LastTimestamp
			}
			f.Events = append(f.Events, event{
				Time: at, Type: e.Type, Reason: e.Reason, Count: e.Count, Message: e.Message,
				Object: strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name,
			})
		}
		sort.Slice(f.Events, func(i, j int) bool { return f.Events[i].Time.After(f.Events[j].Time) })
		if len(f.Events) > 9 {
			f.Events = f.Events[:9]
		}
	}
	k.cached, k.cachedAt = f, time.Now()
	return f, nil
}

// milliCPU converts a Kubernetes CPU quantity ("12345678n", "250m", "1") to millicores.
func milliCPU(q string) int {
	if q == "" {
		return 0
	}
	units := map[byte]float64{'n': 1e-6, 'u': 1e-3, 'm': 1}
	if f, ok := units[q[len(q)-1]]; ok {
		n, _ := strconv.ParseFloat(q[:len(q)-1], 64)
		return int(n*f + 0.5)
	}
	n, _ := strconv.ParseFloat(q, 64)
	return int(n*1000 + 0.5)
}

// mebibytes converts a memory quantity ("23456Ki", "40Mi", "1Gi", bytes) to MiB.
func mebibytes(q string) int {
	units := map[string]float64{"Ki": 1.0 / 1024, "Mi": 1, "Gi": 1024}
	for suffix, f := range units {
		if strings.HasSuffix(q, suffix) {
			n, _ := strconv.ParseFloat(strings.TrimSuffix(q, suffix), 64)
			return int(n*f + 0.5)
		}
	}
	n, _ := strconv.ParseFloat(q, 64)
	return int(n/(1024*1024) + 0.5)
}

// shortDigest turns "ttl.sh/x@sha256:abcdef…" into "sha256:abcdef12".
func shortDigest(imageID string) string {
	if i := strings.Index(imageID, "sha256:"); i >= 0 && len(imageID) >= i+15 {
		return imageID[i : i+15]
	}
	return imageID
}

// flagValue reads "-rps 30" or "-rps=30" from container args.
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}
