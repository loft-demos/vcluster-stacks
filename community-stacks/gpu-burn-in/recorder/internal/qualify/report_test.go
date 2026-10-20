package qualify

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A report.json as the gpu-burn-in report step writes it, trimmed to what the recorder reads.
const sampleReportJSON = `{
  "schema": "gpu-burn-in.vcluster.com/v1", "result": "failed", "failedTests": ["soak"],
  "summary": "soak failed: above 85C: GPU 0 91C", "assetID": "rack-c-u12", "profile": "single-gpu-smoke",
  "host": {"name": "rack-c-u12", "dmi": {"product_serial": "S123"}},
  "expectedGPUs": 1, "detectedGPUs": 1, "startedAt": "2026-10-20T20:31:00Z",
  "gpus": [{"index": 0, "uuid": "GPU-5192", "name": "NVIDIA RTX 2000 Ada Generation", "serial": "1652", "driver": "580.95.05"}],
  "tests": [
    {"test": "inventory", "status": "passed", "summary": "1 x NVIDIA RTX 2000", "metrics": {"gpus": 1, "allocatable_gpus": 1}},
    {"test": "soak", "status": "failed", "summary": "above 85C", "metrics": {"peak_temp_c": 91, "gpus_with_xid": 0}}
  ]
}`

func listReports(t *testing.T, c client.Client) []unstructured.Unstructured {
	t.Helper()
	l := newList(ReportGVK)
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestCreatesQualificationReport(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fixture{result: "failed"}.objects()...).Build()
	r := newReconciler(c, TargetMachine)
	reconcile(t, r)
	reconcile(t, r) // once per run: the second pass creates nothing

	reports := listReports(t, c)
	if len(reports) != 1 {
		t.Fatalf("%d reports, want 1", len(reports))
	}
	qr := reports[0]
	spec := qr.Object["spec"].(map[string]any)
	if spec["machineRef"] != machine || spec["result"] != "failed" || spec["profile"] != "single-gpu-smoke" || spec["assetID"] != "rack-c-u12" {
		t.Fatalf("spec %v", spec)
	}
	if src := spec["source"].(map[string]any); src["kind"] != "Stack" || src["name"] != stack || src["uid"] != "uid-1" || src["template"] != "gpu-burn-in" {
		t.Fatalf("source %v", src)
	}
	hw := spec["hardware"].(map[string]any)
	if hw["gpuModel"] != "NVIDIA RTX 2000 Ada Generation" || hw["driver"] != "580.95.05" || fmt.Sprint(hw["gpuCount"]) != "1" {
		t.Fatalf("hardware %v", hw)
	}
	tests := spec["tests"].([]any)
	soak := tests[1].(map[string]any)
	if soak["name"] != "soak" || fmt.Sprint(soak["metrics"].(map[string]any)["peak_temp_c"]) != "91" {
		t.Fatalf("soak test %v", soak)
	}
	if l := qr.GetLabels(); l[labelMachine] != machine || l[labelResult] != "failed" || l[labelSourceKind] != "Stack" {
		t.Fatalf("labels %v", l)
	}
}

func TestReportWithoutFullReport(t *testing.T) {
	objs := fixture{result: "passed"}.objects()
	for _, o := range objs {
		if s, ok := o.(interface{ GetName() string }); ok && s.GetName() == outputsSecretName(stack) {
			delete(o.(*corev1.Secret).Data, reportJSONOutputKey)
		}
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
	reconcile(t, newReconciler(c, TargetMachine))
	reports := listReports(t, c)
	if len(reports) != 1 || reports[0].Object["spec"].(map[string]any)["result"] != "passed" {
		t.Fatalf("reports %v", reports)
	}
}

func TestPrunesOldReports(t *testing.T) {
	var objs []client.Object
	for i := 0; i < 5; i++ {
		r := obj(ReportGVK, "", fmt.Sprintf("old-%d", i), map[string]any{"spec": map[string]any{
			"machineRef": machine, "result": "passed", "source": map[string]any{"kind": "Stack"},
			"finishedAt": fmt.Sprintf("2026-10-1%dT00:00:00Z", i)}})
		r.SetLabels(map[string]string{labelMachine: machine})
		objs = append(objs, r)
	}
	objs = append(objs, fixture{result: "failed"}.objects()...)
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).Build()
	r := newReconciler(c, TargetMachine)
	r.Config.KeepReports = 3
	reconcile(t, r)

	var names []string
	for _, qr := range listReports(t, c) {
		names = append(names, qr.GetName())
	}
	got := strings.Join(names, ",")
	if len(names) != 3 || !strings.Contains(got, "old-4") || !strings.Contains(got, "old-3") || strings.Contains(got, "old-0") {
		t.Fatalf("kept %s; want the new report, old-4 and old-3", got)
	}
}

func TestReportMetrics(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fixture{result: "failed"}.objects()...).Build()
	reconcile(t, newReconciler(c, TargetMachine))
	older := obj(ReportGVK, "", "older", map[string]any{"spec": map[string]any{
		"machineRef": machine, "result": "passed", "source": map[string]any{"kind": "Stack"},
		"finishedAt": "2026-10-01T00:00:00Z"}})
	if err := c.Create(context.Background(), older); err != nil {
		t.Fatal(err)
	}

	want := fmt.Sprintf(`
# HELP gpu_qualification_passed 1 when the machine's latest qualification passed, 0 when it did not.
# TYPE gpu_qualification_passed gauge
gpu_qualification_passed{machine=%[1]q,profile="single-gpu-smoke",provider=%[2]q} 0
# HELP gpu_qualification_reports QualificationReports kept per machine and result, the history the cluster holds.
# TYPE gpu_qualification_reports gauge
gpu_qualification_reports{machine=%[1]q,result="failed"} 1
gpu_qualification_reports{machine=%[1]q,result="passed"} 1
# HELP gpu_qualification_test_metric A measurement from the machine's latest qualification, such as soak peak_temp_c.
# TYPE gpu_qualification_test_metric gauge
gpu_qualification_test_metric{machine=%[1]q,metric="allocatable_gpus",test="inventory"} 1
gpu_qualification_test_metric{machine=%[1]q,metric="gpus",test="inventory"} 1
gpu_qualification_test_metric{machine=%[1]q,metric="gpus_with_xid",test="soak"} 0
gpu_qualification_test_metric{machine=%[1]q,metric="peak_temp_c",test="soak"} 91
`, machine, provider)
	err := testutil.CollectAndCompare(&ReportCollector{Reader: c}, strings.NewReader(want),
		"gpu_qualification_passed", "gpu_qualification_reports", "gpu_qualification_test_metric")
	if err != nil {
		t.Fatal(err)
	}
}

func TestSafeLabelAndReportName(t *testing.T) {
	if got := safeLabel("b300 burn-in/v1"); got != "b300-burn-in-v1" {
		t.Fatalf("safeLabel %q", got)
	}
	if got := safeLabel(strings.Repeat("a", 80)); len(got) > 63 {
		t.Fatalf("safeLabel too long: %d", len(got))
	}
	if got := reportName("Metal3.Host", "abcd1234-ef00-1111"); got != "metal3.host-abcd1234" {
		t.Fatalf("reportName %q", got)
	}
}
