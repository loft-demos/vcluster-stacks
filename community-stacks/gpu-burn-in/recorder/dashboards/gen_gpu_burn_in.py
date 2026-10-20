#!/usr/bin/env python3
"""Generates ../chart/dashboards/gpu-burn-in.json, the GPU Burn-In dashboard. Edit here, rerun:

    python3 community-stacks/gpu-burn-in/recorder/dashboards/gen_gpu_burn_in.py

GPU server qualification across the fleet, from the gpu_qualification_* metrics the
gpu-burn-in-recorder computes from QualificationReports. Every source of reports (the
gpu-burn-in Stack, record-qualification.sh, NICo Machine Validation) feeds the same panels.

Every metric describes each machine's latest report, except gpu_qualification_reports, which
counts the reports kept. Live DCGM telemetry during a run stays on the Fleet GPU dashboards.
"""
import json
import pathlib

DS = {"type": "prometheus", "uid": "${datasource}"}
SEL = 'provider=~"$provider",profile=~"$profile",machine=~"$machine"'
MSEL = 'machine=~"$machine"'

panels = []
next_id = [1]
y = [0]


def pid():
    next_id[0] += 1
    return next_id[0]


def place(w, h, x):
    return {"h": h, "w": w, "x": x, "y": y[0]}


def row(title):
    panels.append({"type": "row", "title": title, "id": pid(), "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": y[0]}, "panels": []})
    y[0] += 1


def target(expr, ref="A", legend="", instant=False, table=False):
    t = {"datasource": DS, "expr": expr, "refId": ref, "legendFormat": legend}
    if instant or table:
        t.update({"instant": True, "range": False})
    if table:
        t["format"] = "table"
    return t


def stat(title, expr, x, w=4, unit="none", thresholds=None, desc="", mappings=None):
    steps = thresholds or [{"color": "green", "value": None}]
    panels.append({
        "type": "stat", "title": title, "id": pid(), "datasource": DS, "description": desc,
        "gridPos": place(w, 4, x),
        "targets": [target(expr, instant=True)],
        "options": {"colorMode": "value", "graphMode": "none", "justifyMode": "center",
                    "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}},
        "fieldConfig": {"defaults": {"unit": unit, "mappings": mappings or [],
                                     "thresholds": {"mode": "absolute", "steps": steps}},
                        "overrides": []},
    })


def bargauge(title, expr, legend, x, w, unit="none", thresholds=None, desc="", min_=None, max_=None):
    steps = thresholds or [{"color": "blue", "value": None}]
    defaults = {"unit": unit, "thresholds": {"mode": "absolute", "steps": steps}}
    if min_ is not None:
        defaults["min"] = min_
    if max_ is not None:
        defaults["max"] = max_
    panels.append({
        "type": "bargauge", "title": title, "id": pid(), "datasource": DS, "description": desc,
        "gridPos": place(w, 8, x),
        "targets": [target(expr, legend=legend, instant=True)],
        "options": {"displayMode": "gradient", "orientation": "horizontal", "showUnfilled": True,
                    "valueMode": "color", "namePlacement": "left",
                    "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}},
        "fieldConfig": {"defaults": defaults, "overrides": []},
    })


RESULT_MAPPING = [{"type": "value", "options": {
    "1": {"text": "Qualified", "color": "green", "index": 0},
    "0": {"text": "Not qualified", "color": "red", "index": 1}}}]

# --- Fleet ---------------------------------------------------------------------------------------
row("Fleet")
stat("Qualified servers", f"count(gpu_qualification_passed{{{SEL}}} == 1) or vector(0)", 0,
     desc="Servers whose latest qualification passed.")
stat("Not qualified", f"count(gpu_qualification_passed{{{SEL}}} == 0) or vector(0)", 4,
     thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}],
     desc="Servers whose latest qualification failed or errored. Keep them out of production NodeTypes.")
stat("Pass rate", f"sum(gpu_qualification_passed{{{SEL}}}) / count(gpu_qualification_passed{{{SEL}}})", 8,
     unit="percentunit",
     thresholds=[{"color": "red", "value": None}, {"color": "orange", "value": 0.8}, {"color": "green", "value": 0.95}],
     desc="Share of servers whose latest qualification passed.")
stat("Servers with uncorrected ECC",
     f'count(gpu_qualification_test_metric{{{MSEL},test="inventory",metric="uncorrected_ecc_errors"}} > 0) or vector(0)', 12,
     thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}])
stat("Servers with an XID during soak",
     f'count(gpu_qualification_test_metric{{{MSEL},test="soak",metric="gpus_with_xid"}} > 0) or vector(0)', 16,
     thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}])
stat("Last qualification", f"max(gpu_qualification_finished_timestamp_seconds{{{MSEL}}}) * 1000", 20,
     unit="dateTimeFromNow", desc="When the most recent qualification finished.")
y[0] += 4

# --- Servers table -------------------------------------------------------------------------------
servers = {
    "type": "table", "title": "Servers", "id": pid(), "datasource": DS,
    "description": "Each server's latest qualification. Measurements are blank for tests that did not run.",
    "gridPos": place(24, 9, 0),
    "targets": [
        target(f"max by (machine, provider, profile) (gpu_qualification_passed{{{SEL}}})", "A", table=True),
        target(f"max by (machine) (gpu_qualification_finished_timestamp_seconds{{{MSEL}}}) * 1000", "B", table=True),
        target(f'max by (machine) (gpu_qualification_gpus{{{MSEL},kind="found"}})', "C", table=True),
        target(f'max by (machine) (gpu_qualification_gpus{{{MSEL},kind="expected"}})', "D", table=True),
        target(f'max by (machine) (gpu_qualification_test_metric{{{MSEL},test="inventory",metric="uncorrected_ecc_errors"}})', "E", table=True),
        target(f'max by (machine) (gpu_qualification_test_metric{{{MSEL},test="soak",metric="peak_temp_c"}})', "F", table=True),
        target(f'max by (machine) (gpu_qualification_test_metric{{{MSEL},test="soak",metric="gpus_with_xid"}})', "G", table=True),
        target(f'max by (machine) (gpu_qualification_test_metric{{{MSEL},test="nccl",metric="all_reduce_bus_bandwidth_gbps"}})', "H", table=True),
    ],
    "transformations": [
        {"id": "joinByField", "options": {"byField": "machine", "mode": "outer"}},
        {"id": "organize", "options": {
            "excludeByName": {"Time": True, "Time 1": True, "Time 2": True, "Time 3": True, "Time 4": True,
                              "Time 5": True, "Time 6": True, "Time 7": True, "Time 8": True},
            "indexByName": {"machine": 0, "provider": 1, "profile": 2, "Value #A": 3, "Value #B": 4,
                            "Value #C": 5, "Value #D": 6, "Value #E": 7, "Value #F": 8, "Value #G": 9, "Value #H": 10},
            "renameByName": {"machine": "Machine", "provider": "Provider", "profile": "Profile",
                             "Value #A": "Result", "Value #B": "Finished", "Value #C": "GPUs",
                             "Value #D": "Expected", "Value #E": "Uncorrected ECC",
                             "Value #F": "Soak peak (C)", "Value #G": "GPUs with XID",
                             "Value #H": "all_reduce (GB/s)"}}},
    ],
    "options": {"showHeader": True, "cellHeight": "sm",
                "sortBy": [{"displayName": "Result", "desc": False}]},
    "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}},
                    "overrides": [
                        {"matcher": {"id": "byName", "options": "Result"},
                         "properties": [{"id": "mappings", "value": RESULT_MAPPING},
                                        {"id": "custom.cellOptions", "value": {"type": "color-background"}}]},
                        {"matcher": {"id": "byName", "options": "Finished"},
                         "properties": [{"id": "unit", "value": "dateTimeAsIso"}]},
                        {"matcher": {"id": "byName", "options": "Uncorrected ECC"},
                         "properties": [{"id": "thresholds", "value": {"mode": "absolute", "steps": [
                             {"color": "green", "value": None}, {"color": "red", "value": 1}]}},
                             {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
                        {"matcher": {"id": "byName", "options": "GPUs with XID"},
                         "properties": [{"id": "thresholds", "value": {"mode": "absolute", "steps": [
                             {"color": "green", "value": None}, {"color": "red", "value": 1}]}},
                             {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
                        {"matcher": {"id": "byName", "options": "Soak peak (C)"},
                         "properties": [{"id": "thresholds", "value": {"mode": "absolute", "steps": [
                             {"color": "green", "value": None}, {"color": "orange", "value": 85},
                             {"color": "red", "value": 90}]}},
                             {"id": "custom.cellOptions", "value": {"type": "color-text"}}]},
                    ]},
}
panels.append(servers)
y[0] += 9

# --- Tests ---------------------------------------------------------------------------------------
row("Tests")
panels.append({
    "type": "table", "title": "Test results", "id": pid(), "datasource": DS,
    "description": "Each test's status in each server's latest qualification.",
    "gridPos": place(24, 8, 0),
    "targets": [target(f"gpu_qualification_test_status{{{MSEL}}} == 1", table=True)],
    "transformations": [
        {"id": "groupingToMatrix", "options": {"columnField": "test", "rowField": "machine", "valueField": "status"}},
        {"id": "organize", "options": {"indexByName": {"machine\\test": 0, "gpuready": 1, "inventory": 2,
                                                       "dcgmdiag": 3, "nccl": 4, "soak": 5},
                                       "renameByName": {"machine\\test": "Machine"}}},
    ],
    "fieldConfig": {"defaults": {
        "custom": {"align": "center", "cellOptions": {"type": "color-background"}},
        "mappings": [{"type": "value", "options": {
            "passed": {"color": "green", "index": 0}, "failed": {"color": "red", "index": 1},
            "error": {"color": "dark-red", "index": 2}, "skipped": {"color": "orange", "index": 3},
            "disabled": {"color": "transparent", "index": 4}, "missing": {"color": "purple", "index": 5}}}]},
        "overrides": [{"matcher": {"id": "byName", "options": "Machine"},
                       "properties": [{"id": "custom.cellOptions", "value": {"type": "auto"}},
                                      {"id": "custom.align", "value": "left"}]}]},
    "options": {"showHeader": True, "cellHeight": "sm"},
})
y[0] += 8

# --- Measurements --------------------------------------------------------------------------------
row("Measurements")
bargauge("Soak peak GPU temperature",
         f'gpu_qualification_test_metric{{{MSEL},test="soak",metric="peak_temp_c"}}', "{{machine}}", 0, 8,
         unit="celsius", min_=0, max_=110,
         thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 85}, {"color": "red", "value": 90}],
         desc="Hottest GPU during the soak. The soak fails above its maxGPUTempC.")
bargauge("Soak peak power",
         f'gpu_qualification_test_metric{{{MSEL},test="soak",metric="peak_power_w"}}', "{{machine}}", 8, 8,
         unit="watt", min_=0)
bargauge("NCCL bus bandwidth",
         f'gpu_qualification_test_metric{{{MSEL},test="nccl",metric=~".+_bus_bandwidth_gbps"}}',
         "{{machine}} {{metric}}", 16, 8, unit="none",
         desc="Average bus bandwidth per collective, GB/s. Compare servers of the same model; 0 with one GPU.")
y[0] += 8
bargauge("GPUs found and expected", f"gpu_qualification_gpus{{{MSEL}}}", "{{machine}} {{kind}}", 0, 8, min_=0)
bargauge("Uncorrected ECC errors",
         f'gpu_qualification_test_metric{{{MSEL},test="inventory",metric="uncorrected_ecc_errors"}}', "{{machine}}", 8, 8,
         thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}], min_=0)
bargauge("Soak throttle samples",
         f'gpu_qualification_test_metric{{{MSEL},test="soak",metric="throttle_samples"}}', "{{machine}}", 16, 8,
         thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 1}], min_=0,
         desc="dmon samples with a thermal or power-brake clock event reason. Reported, not failed on.")
y[0] += 8

# --- History -------------------------------------------------------------------------------------
row("History")
panels.append({
    "type": "timeseries", "title": "Servers by latest result", "id": pid(), "datasource": DS,
    "gridPos": place(12, 8, 0),
    "targets": [target(f"count by (result) (gpu_qualification_result{{{SEL}}})", legend="{{result}}")],
    "fieldConfig": {"defaults": {"custom": {"drawStyle": "line", "fillOpacity": 20, "stacking": {"mode": "normal"}},
                                 "unit": "none", "min": 0},
                    "overrides": [
                        {"matcher": {"id": "byName", "options": "passed"},
                         "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": "green"}}]},
                        {"matcher": {"id": "byName", "options": "failed"},
                         "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": "red"}}]}]},
    "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}},
})
bargauge("Reports kept per server", f"sum by (machine, result) (gpu_qualification_reports{{{MSEL}}})",
         "{{machine}} {{result}}", 12, 12, min_=0,
         desc="QualificationReports the cluster holds, the history behind each server.")
y[0] += 8


def var(name, label, query):
    return {"name": name, "label": label, "type": "query", "datasource": DS,
            "query": {"query": query, "refId": "StandardVariableQuery"},
            "definition": query, "refresh": 2, "includeAll": True, "multi": True,
            "allValue": ".*", "current": {"text": ["All"], "value": ["$__all"]}, "sort": 1}


dashboard = {
    "uid": "gpu-burn-in",
    "title": "GPU Burn-In",
    "description": "GPU server qualification results from QualificationReports, across providers.",
    "tags": ["vcluster", "fleet-observability", "gpu", "qualification", "burn-in"],
    "timezone": "browser",
    "schemaVersion": 41,
    "editable": True,
    "graphTooltip": 1,
    "time": {"from": "now-7d", "to": "now"},
    "refresh": "1m",
    "links": [
        {"title": "GPU Overview", "url": "/d/vcluster-gpu-overview", "type": "link", "tags": []},
        {"title": "Tenant Cluster GPUs", "url": "/d/vcluster-tenant-gpus", "type": "link", "tags": []},
        {"title": "Fleet Overview", "url": "/d/vcluster-fleet-observability-operations", "type": "link", "tags": []},
    ],
    "templating": {"list": [
        {"name": "datasource", "label": "Datasource", "type": "datasource", "query": "prometheus",
         "current": {}, "hide": 0},
        var("provider", "Provider", "label_values(gpu_qualification_passed, provider)"),
        var("profile", "Profile", 'label_values(gpu_qualification_passed{provider=~"$provider"}, profile)'),
        var("machine", "Machine",
            'label_values(gpu_qualification_passed{provider=~"$provider",profile=~"$profile"}, machine)'),
    ]},
    "panels": panels,
}

out = pathlib.Path(__file__).resolve().parent.parent / "chart" / "dashboards" / "gpu-burn-in.json"
out.parent.mkdir(parents=True, exist_ok=True)
out.write_text(json.dumps(dashboard, indent=2) + "\n")
print(f"wrote {out}")
