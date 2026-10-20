package qualify

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ReportCollector publishes Prometheus metrics computed from QualificationReports on every scrape:
// the latest result, test statuses and measurements per machine, and how many reports each
// machine has. It reads the controller's cache, so a scrape costs no API calls.
type ReportCollector struct {
	Reader client.Reader
}

var (
	descResult = prometheus.NewDesc("gpu_qualification_result",
		"Latest qualification result per machine: 1 for the result label that applies.",
		[]string{"machine", "provider", "profile", "source", "result"}, nil)
	descPassed = prometheus.NewDesc("gpu_qualification_passed",
		"1 when the machine's latest qualification passed, 0 when it did not.",
		[]string{"machine", "provider", "profile"}, nil)
	descFinished = prometheus.NewDesc("gpu_qualification_finished_timestamp_seconds",
		"When the machine's latest qualification finished.",
		[]string{"machine", "provider"}, nil)
	descTestStatus = prometheus.NewDesc("gpu_qualification_test_status",
		"Status of each test in the machine's latest qualification: 1 for the status that applies.",
		[]string{"machine", "test", "status"}, nil)
	descTestMetric = prometheus.NewDesc("gpu_qualification_test_metric",
		"A measurement from the machine's latest qualification, such as soak peak_temp_c.",
		[]string{"machine", "test", "metric"}, nil)
	descReports = prometheus.NewDesc("gpu_qualification_reports",
		"QualificationReports kept per machine and result, the history the cluster holds.",
		[]string{"machine", "result"}, nil)
	descGPUs = prometheus.NewDesc("gpu_qualification_gpus",
		"GPUs found in the machine's latest qualification, and how many were expected.",
		[]string{"machine", "kind"}, nil)
)

func (c *ReportCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descResult, descPassed, descFinished, descTestStatus, descTestMetric, descReports, descGPUs} {
		ch <- d
	}
}

func (c *ReportCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	list := newList(ReportGVK)
	if err := c.Reader.List(ctx, list); err != nil {
		if !meta.IsNoMatchError(err) {
			log.FromContext(ctx).Error(err, "list QualificationReports for metrics")
		}
		return
	}
	latest := map[string]*unstructured.Unstructured{}
	counts := map[[2]string]int{}
	for i := range list.Items {
		r := &list.Items[i]
		machine, _, _ := unstructured.NestedString(r.Object, "spec", "machineRef")
		result, _, _ := unstructured.NestedString(r.Object, "spec", "result")
		counts[[2]string{machine, result}]++
		if cur, ok := latest[machine]; !ok || reportTime(r) > reportTime(cur) {
			latest[machine] = r
		}
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(descReports, prometheus.GaugeValue, float64(n), k[0], k[1])
	}
	for machine, r := range latest {
		str := func(f ...string) string {
			v, _, _ := unstructured.NestedString(r.Object, append([]string{"spec"}, f...)...)
			return v
		}
		provider, profile, result := str("providerRef"), str("profile"), str("result")
		ch <- prometheus.MustNewConstMetric(descResult, prometheus.GaugeValue, 1, machine, provider, profile, str("source", "kind"), result)
		passed := 0.0
		if result == "passed" {
			passed = 1
		}
		ch <- prometheus.MustNewConstMetric(descPassed, prometheus.GaugeValue, passed, machine, provider, profile)
		if t, err := time.Parse(time.RFC3339, str("finishedAt")); err == nil {
			ch <- prometheus.MustNewConstMetric(descFinished, prometheus.GaugeValue, float64(t.Unix()), machine, provider)
		}
		for _, kind := range []string{"gpuCount", "expectedGPUs"} {
			if n, found, _ := unstructured.NestedInt64(r.Object, "spec", "hardware", kind); found {
				label := map[string]string{"gpuCount": "found", "expectedGPUs": "expected"}[kind]
				ch <- prometheus.MustNewConstMetric(descGPUs, prometheus.GaugeValue, float64(n), machine, label)
			}
		}
		tests, _, _ := unstructured.NestedSlice(r.Object, "spec", "tests")
		for _, t := range tests {
			m, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			status, _ := m["status"].(string)
			ch <- prometheus.MustNewConstMetric(descTestStatus, prometheus.GaugeValue, 1, machine, name, status)
			metrics, _ := m["metrics"].(map[string]any)
			for k, v := range metrics {
				if f, ok := number(v); ok {
					ch <- prometheus.MustNewConstMetric(descTestMetric, prometheus.GaugeValue, f, machine, name, k)
				}
			}
		}
	}
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}
