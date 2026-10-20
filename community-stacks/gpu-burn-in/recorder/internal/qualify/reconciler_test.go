package qualify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	ns       = "p-platform"
	stack    = "gpu-burn-in-rtx2000-burnin-114479"
	tenant   = "gpu-burn-in-rtx2000"
	provider = "metal3-dc1"
	host     = "rack-c-u12"
	machine  = provider + "." + host
	domain   = "gpu-qualification.example.com"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	for _, gvk := range []schema.GroupVersionKind{StackInstanceGVK, stackTemplateGVK, nodeClaimGVK, storageMachine,
		nodeProviderGVK, tenantClusterGVK, managementMachine, bareMetalHost} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func obj(gvk schema.GroupVersionKind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

type fixture struct {
	reportPhase string
	result      string
	params      map[string]any
	defaults    map[string]string
	netboxURL   string
}

// objects returns a Platform state for one finished run on one Metal3 server.
func (f fixture) objects() []client.Object {
	if f.reportPhase == "" {
		f.reportPhase = "Healthy"
	}
	params := map[string]any{"profile": "single-gpu-smoke"}
	for k, v := range f.params {
		params[k] = v
	}
	var tparams []any
	for k, v := range f.defaults {
		tparams = append(tparams, map[string]any{"variable": k, "defaultValue": v})
	}
	si := obj(StackInstanceGVK, ns, stack, map[string]any{
		"spec": map[string]any{
			"templateRef": map[string]any{"name": "gpu-burn-in"},
			"destination": map[string]any{"virtualCluster": map[string]any{"name": tenant}},
			"parameters":  params,
		},
		"status": map[string]any{"tasks": []any{
			map[string]any{"name": "soak", "phase": "Healthy"},
			map[string]any{"name": "report", "phase": f.reportPhase},
		}},
	})
	si.SetUID("uid-1")
	data := map[string][]byte{"gpuready.gpus": []byte("1")}
	if f.result != "" {
		data["report.result"] = []byte(f.result)
		data["report.failedtests"] = []byte("soak")
		data["report.summary"] = []byte("soak failed: above 85C: GPU 0 91C")
		data["report.finishedat"] = []byte("2026-10-20T21:10:00Z")
	}
	sm := obj(storageMachine, "", machine, map[string]any{"spec": map[string]any{"providerRef": provider}})
	annotations := map[string]string{machineNodeClaimAnnotation: ns + "/" + tenant + "-qm92s"}
	if f.netboxURL != "" {
		annotations[netboxDeviceURLAnnotation] = f.netboxURL
	}
	sm.SetAnnotations(annotations)
	return []client.Object{
		si,
		obj(stackTemplateGVK, "", "gpu-burn-in", map[string]any{"spec": map[string]any{"parameters": tparams}}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: outputsSecretName(stack)}, Data: data},
		obj(nodeClaimGVK, ns, tenant+"-qm92s", map[string]any{"spec": map[string]any{"vClusterRef": tenant}}),
		obj(nodeClaimGVK, ns, "other-claim", map[string]any{"spec": map[string]any{"vClusterRef": "someone-else"}}),
		sm,
		obj(managementMachine, "", machine, map[string]any{"spec": map[string]any{"providerRef": provider}}),
		obj(nodeProviderGVK, "", provider, map[string]any{"spec": map[string]any{"metal3": map[string]any{
			"clusterRef": map[string]any{"cluster": "spark-cluster", "namespace": "metal3-system"}}}}),
		obj(tenantClusterGVK, ns, tenant, nil),
	}
}

type staticClusters map[string]client.Client

func (s staticClusters) For(c string) (client.Client, error) { return s[c], nil }

func reconcile(t *testing.T, r *Reconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: stack}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func newReconciler(c client.Client, target Target) *Reconciler {
	return &Reconciler{Client: c, Reader: c, Config: Config{
		TemplateNames: []string{"gpu-burn-in"}, LabelDomain: domain, Target: target,
		NetBoxAdmitTag: "vcluster-platform", NetBoxQualifiedTag: "gpu-qualified",
	}}
}

func get(t *testing.T, c client.Client, gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	u := newObj(gvk)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, u); err != nil {
		t.Fatalf("get %s %s: %v", gvk.Kind, name, err)
	}
	return u
}

func TestRecordsOnMachine(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fixture{result: "failed"}.objects()...).Build()
	r := newReconciler(c, TargetMachine)
	reconcile(t, r)

	m := get(t, c, managementMachine, "", machine)
	if got := m.GetLabels()[domain+"/result"]; got != "failed" {
		t.Fatalf("result label %q, want failed", got)
	}
	a := m.GetAnnotations()
	if a[domain+"/failed-tests"] != "soak" || a[domain+"/profile"] != "single-gpu-smoke" || a[domain+"/run"] != ns+"/"+stack+"/uid-1" {
		t.Fatalf("annotations %v", a)
	}
}

func TestRecordsOnBareMetalHost(t *testing.T) {
	platform := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fixture{result: "passed"}.objects()...).Build()
	spark := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(obj(bareMetalHost, "metal3-system", host, nil)).Build()
	r := newReconciler(platform, TargetBareMetalHost)
	r.Clusters = staticClusters{"spark-cluster": spark}
	reconcile(t, r)

	if got := get(t, spark, bareMetalHost, "metal3-system", host).GetLabels()[domain+"/result"]; got != "passed" {
		t.Fatalf("BareMetalHost result label %q, want passed", got)
	}
	if got := get(t, platform, managementMachine, "", machine).GetLabels()[domain+"/result"]; got != "" {
		t.Fatalf("bmh target also labeled the Machine: %q", got)
	}
}

func TestWaitsForTheReport(t *testing.T) {
	for name, f := range map[string]fixture{
		"report task still running": {reportPhase: "Progressing", result: "failed"},
		"outputs not captured yet":  {},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(f.objects()...).Build()
			res := reconcile(t, newReconciler(c, TargetMachine))
			if f.reportPhase == "" && res.RequeueAfter == 0 {
				t.Fatal("captured outputs pending, but no requeue")
			}
			if got := get(t, c, managementMachine, "", machine).GetLabels(); len(got) != 0 {
				t.Fatalf("recorded too early: %v", got)
			}
		})
	}
}

func TestRunParameters(t *testing.T) {
	for name, tc := range map[string]struct {
		f       fixture
		labeled bool
		deleted bool
	}{
		"defaults record and keep the server":    {f: fixture{result: "passed"}, labeled: true},
		"recordResult false on the instance":     {f: fixture{result: "passed", params: map[string]any{ParamRecordResult: "false"}}},
		"recordResult false as template default": {f: fixture{result: "passed", defaults: map[string]string{ParamRecordResult: "false"}}},
		"instance overrides template default": {f: fixture{result: "passed", params: map[string]any{ParamRecordResult: "true"},
			defaults: map[string]string{ParamRecordResult: "false"}}, labeled: true},
		"releaseServer deletes the tenant cluster": {f: fixture{result: "failed", params: map[string]any{ParamReleaseServer: "true"}},
			labeled: true, deleted: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(tc.f.objects()...).Build()
			reconcile(t, newReconciler(c, TargetMachine))
			labeled := get(t, c, managementMachine, "", machine).GetLabels()[domain+"/result"] != ""
			if labeled != tc.labeled {
				t.Fatalf("labeled=%v, want %v", labeled, tc.labeled)
			}
			err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: tenant}, newObj(tenantClusterGVK))
			if deleted := apierrors.IsNotFound(err); deleted != tc.deleted {
				t.Fatalf("tenant cluster deleted=%v, want %v", deleted, tc.deleted)
			}
		})
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(
		fixture{result: "failed", params: map[string]any{ParamReleaseServer: "true"}}.objects()...).Build()
	r := newReconciler(c, TargetMachine)
	r.Config.DryRun = true
	reconcile(t, r)
	if got := get(t, c, managementMachine, "", machine).GetLabels(); len(got) != 0 {
		t.Fatalf("dry run labeled the Machine: %v", got)
	}
	get(t, c, tenantClusterGVK, ns, tenant)
}

// fakeNetBox records requests and serves one device with tags.
type fakeNetBox struct {
	mu       sync.Mutex
	requests []string
	patch    map[string]any
}

func (f *fakeNetBox) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req.Method+" "+req.URL.Path)
	if req.Header.Get("Authorization") != "Bearer nbt_test" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/api/dcim/devices/10/":
		_, _ = io.WriteString(w, `{"id":10,"tags":[{"slug":"vcluster-platform"},{"slug":"rack-c"}]}`)
	case req.Method == http.MethodPatch && req.URL.Path == "/api/dcim/devices/10/":
		_ = json.NewDecoder(req.Body).Decode(&f.patch)
		_, _ = io.WriteString(w, `{}`)
	case req.Method == http.MethodPost && req.URL.Path == "/api/extras/journal-entries/":
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestNetBoxOnFailureAndOnlyOnce(t *testing.T) {
	nb := &fakeNetBox{}
	srv := httptest.NewServer(nb)
	defer srv.Close()
	f := fixture{result: "failed", params: map[string]any{ParamNetBoxUpdate: "true"}, netboxURL: srv.URL + "/dcim/devices/10/"}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(f.objects()...).Build()
	r := newReconciler(c, TargetMachine)
	r.NetBox = &NetBox{Token: "nbt_test", HTTP: srv.Client()}

	// The storage Machine mirrors the management one in the Platform; copy the write across so the
	// second pass sees the run marker, as it would there.
	reconcile(t, r)
	m := get(t, c, managementMachine, "", machine)
	sm := get(t, c, storageMachine, "", machine)
	labels, annotations := sm.GetLabels(), sm.GetAnnotations()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range m.GetLabels() {
		labels[k] = v
	}
	for k, v := range m.GetAnnotations() {
		annotations[k] = v
	}
	sm.SetLabels(labels)
	sm.SetAnnotations(annotations)
	if err := c.Update(context.Background(), sm); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)

	got := strings.Join(nb.requests, ", ")
	want := "GET /api/dcim/devices/10/, PATCH /api/dcim/devices/10/, POST /api/extras/journal-entries/"
	if got != want {
		t.Fatalf("NetBox requests %q, want %q (once)", got, want)
	}
	if nb.patch["status"] != "failed" {
		t.Fatalf("status %v, want failed", nb.patch["status"])
	}
	tags, _ := json.Marshal(nb.patch["tags"])
	if string(tags) != `[{"slug":"rack-c"}]` {
		t.Fatalf("tags %s: the admit tag must go, the rest stay", tags)
	}
}

func TestConfigRejectsReservedDomains(t *testing.T) {
	for _, d := range []string{"vcluster.com", "gpu.vcluster.com", "loft.sh", "x.loft.sh"} {
		c := Config{TemplateNames: []string{"gpu-burn-in"}, LabelDomain: d, Target: TargetMachine}
		if c.Validate() == nil {
			t.Errorf("label domain %s accepted", d)
		}
	}
	ok := Config{TemplateNames: []string{"gpu-burn-in"}, LabelDomain: "gpu-qualification.example.com", Target: TargetBareMetalHost}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNames(t *testing.T) {
	if got := outputsSecretName(stack); got != "stack-outputs-"+stack {
		t.Fatalf("short name %q", got)
	}
	long := outputsSecretName(strings.Repeat("a", 60))
	if len(long) != 63 || !strings.HasPrefix(long, "stack-outputs-aaaa") || long[55] != '-' {
		t.Fatalf("long name %q (%d)", long, len(long))
	}
	if _, id, err := netboxDevice("https://netbox.example/dcim/devices/10/"); err != nil || id != 10 {
		t.Fatalf("netboxDevice: %d %v", id, err)
	}
}
