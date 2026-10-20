#!/usr/bin/env bash
# Records a gpu-burn-in result on the server's inventory record: the "external automation" half of
# the qualification flow. A sample, not a controller: run it once per qualification, after the
# Stack has finished.
#
#   record-qualification.sh --tenant-kubeconfig tenant.kubeconfig \
#     --bmh rack-a-u12 --bmh-namespace metal3-system --metal3-context spark-cluster \
#     [--netbox-device rack-a-u12 --netbox-admit-tag vcluster-platform --netbox-qualified-tag gpu-qualified] \
#     [--apply]
#
# Without --apply it prints what it would change and changes nothing.
#
# Reads ConfigMap gpu-burn-in/gpu-burn-in-report from the tenant cluster (get a kubeconfig with
# `vcluster platform connect vcluster <name> --project <project> --print`), or from --report-file,
# a copy saved with --save-report. Save one before you delete the tenant cluster.
#
# BareMetalHost (Metal3): sets label gpu-qualification.vcluster.com/result=passed|failed, and
# annotations with the profile, time, failed tests, summary and report URI. The Metal3 provider's
# teardown removes only its own project and NodeClaim labels, so these survive the release of the
# server. A label alone does not stop allocation: require it in the selector of every production
# NodeType, for example
#     bareMetalHosts:
#       selector:
#         matchLabels:
#           gpu-qualification.vcluster.com/result: passed
# and give the qualification NodeType a selector that excludes passed hosts.
#
# NetBox (optional, NETBOX_URL and NETBOX_TOKEN in the environment): on a failure, sets the device
# status to failed and removes --netbox-admit-tag, the tag that brings the device into vCluster
# Platform. On a pass, adds --netbox-qualified-tag if given. Either way it adds a journal entry
# with the summary. Check what your NetBox import does when a device loses its tag before you use
# --netbox-admit-tag.
set -euo pipefail

usage() { sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//;/^set -euo/d'; exit "${1:-0}"; }

tenant_kubeconfig="" report_file="" save_report=""
bmh="" bmh_namespace="metal3-system" metal3_context=""
nb_device="" nb_admit_tag="" nb_qualified_tag="" apply=false
while [[ $# -gt 0 ]]; do
  case $1 in
    --tenant-kubeconfig) tenant_kubeconfig=$2; shift 2 ;;
    --report-file) report_file=$2; shift 2 ;;
    --save-report) save_report=$2; shift 2 ;;
    --bmh) bmh=$2; shift 2 ;;
    --bmh-namespace) bmh_namespace=$2; shift 2 ;;
    --metal3-context) metal3_context=$2; shift 2 ;;
    --netbox-device) nb_device=$2; shift 2 ;;
    --netbox-admit-tag) nb_admit_tag=$2; shift 2 ;;
    --netbox-qualified-tag) nb_qualified_tag=$2; shift 2 ;;
    --apply) apply=true; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown argument: $1" >&2; usage 1 ;;
  esac
done
for tool in kubectl jq; do command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 2; }; done

run() {
  if $apply; then "$@"; else printf 'would run:'; printf ' %q' "$@"; echo; fi
}

# --- Read the report ----------------------------------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [[ -n "$report_file" ]]; then
  cp "$report_file" "$tmp/report.json"
elif [[ -n "$tenant_kubeconfig" ]]; then
  kubectl --kubeconfig "$tenant_kubeconfig" -n gpu-burn-in get configmap gpu-burn-in-report \
    -o jsonpath='{.data.report\.json}' >"$tmp/report.json"
else
  echo "need --tenant-kubeconfig or --report-file" >&2
  exit 1
fi
[[ -s "$tmp/report.json" ]] || { echo "no report found; has the report task run?" >&2; exit 1; }
[[ -z "$save_report" ]] || cp "$tmp/report.json" "$save_report"

result=$(jq -r .result "$tmp/report.json")
profile=$(jq -r .profile "$tmp/report.json")
failed=$(jq -r '.failedTests | join(",")' "$tmp/report.json")
summary=$(jq -r .summary "$tmp/report.json")
uri=$(jq -r '.reportURI // ""' "$tmp/report.json")
finished=$(jq -r '[.tests[].finishedAt | select(. != "")] | max // ""' "$tmp/report.json")
host=$(jq -r .host.name "$tmp/report.json")
echo "report: $result on $host (asset $(jq -r '.assetID // "unset"' "$tmp/report.json"), profile $profile)"
echo "  $summary"
case $result in passed|failed) ;; *) echo "unexpected result '$result'" >&2; exit 1 ;; esac

# --- BareMetalHost ------------------------------------------------------------------------------
if [[ -n "$bmh" ]]; then
  k=(kubectl -n "$bmh_namespace")
  [[ -z "$metal3_context" ]] || k=(kubectl --context "$metal3_context" -n "$bmh_namespace")
  "${k[@]}" get baremetalhost "$bmh" >/dev/null
  run "${k[@]}" label baremetalhost "$bmh" --overwrite "gpu-qualification.vcluster.com/result=$result"
  # Annotation values are free text; keep the summary short enough to read in kubectl describe.
  run "${k[@]}" annotate baremetalhost "$bmh" --overwrite \
    "gpu-qualification.vcluster.com/profile=$profile" \
    "gpu-qualification.vcluster.com/finished-at=$finished" \
    "gpu-qualification.vcluster.com/failed-tests=$failed" \
    "gpu-qualification.vcluster.com/summary=${summary:0:1000}" \
    "gpu-qualification.vcluster.com/report=$uri"
fi

# --- NetBox -------------------------------------------------------------------------------------
if [[ -n "$nb_device" ]]; then
  : "${NETBOX_URL:?set NETBOX_URL}" "${NETBOX_TOKEN:?set NETBOX_TOKEN}"
  # NetBox 4.5 v2 tokens (nbt_...) use Bearer; v1 tokens use Token.
  if [[ "$NETBOX_TOKEN" == nbt_* ]]; then auth="Bearer $NETBOX_TOKEN"; else auth="Token $NETBOX_TOKEN"; fi
  nb() { curl -fsS -H "Authorization: $auth" -H 'Content-Type: application/json' -H 'Accept: application/json' "$@"; }
  api=${NETBOX_URL%/}/api
  device=$(nb "$api/dcim/devices/?name=$(jq -rn --arg n "$nb_device" '$n | @uri')" | jq '.results[0] // empty')
  [[ -n "$device" ]] || { echo "NetBox device $nb_device not found" >&2; exit 1; }
  id=$(jq -r .id <<<"$device")
  tags=$(jq '[.tags[].slug]' <<<"$device")

  if [[ $result == failed ]]; then
    [[ -z "$nb_admit_tag" ]] || tags=$(jq --arg t "$nb_admit_tag" 'map(select(. != $t))' <<<"$tags")
    patch=$(jq -n --argjson tags "$tags" '{status: "failed", tags: ($tags | map({slug: .}))}')
    kind=danger
  else
    [[ -z "$nb_qualified_tag" ]] || tags=$(jq --arg t "$nb_qualified_tag" '. + [$t] | unique' <<<"$tags")
    patch=$(jq -n --argjson tags "$tags" '{tags: ($tags | map({slug: .}))}')
    kind=success
  fi
  journal=$(jq -n --argjson id "$id" --arg kind "$kind" \
    --arg text "GPU qualification $result ($profile)${failed:+: $failed}. $summary${uri:+ Report: $uri}" \
    '{assigned_object_type: "dcim.device", assigned_object_id: $id, kind: $kind, comments: $text}')
  run nb -X PATCH "$api/dcim/devices/$id/" -d "$patch"
  run nb -X POST "$api/extras/journal-entries/" -d "$journal"
fi

$apply || echo "dry run: nothing changed. Re-run with --apply."
