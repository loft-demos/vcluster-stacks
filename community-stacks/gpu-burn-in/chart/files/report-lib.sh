# build_report RESULTS_JSON HOST_JSON: the qualification report on stdout. Sourced by report.sh,
# and by tests/evaluate-test.sh against fixtures. Needs only jq.
#
# RESULTS_JSON is an array of result ConfigMap data maps; details.json is a JSON string in each.
build_report() {
  jq -n --slurpfile results "$1" --slurpfile host "$2" \
    --arg tests "$TESTS" --arg assetID "$ASSET_ID" --arg profile "$PROFILE" \
    --argjson expected "$EXPECTED_GPUS" '
    ($results[0] | map({key: .test, value: .}) | from_entries) as $by |
    [$tests | split(",")[] | select(length > 0)] as $want |
    [$want[] as $t | ($by[$t] // {test: $t, status: "missing", summary: "no result recorded"}) |
      {test: .test, status: .status, summary: .summary, node: (.node // ""),
       startedAt: (.startedAt // ""), finishedAt: (.finishedAt // ""),
       details: ((.["details.json"] // "{}") | fromjson? // {})}] as $tests |
    [$tests[] | select(.status != "passed" and .status != "disabled") | .test] as $failed |
    ([$tests[] | select(.test == "inventory") | .details][0] // {}) as $inv |
    {schema: "gpu-burn-in.vcluster.com/v1",
     result: (if ($failed | length) == 0 then "passed" else "failed" end),
     failedTests: $failed,
     summary: (if ($failed | length) == 0
       then "qualified: \([$tests[] | select(.status == "passed") | .test] | join(", "))"
       else [$tests[] | select(.status != "passed" and .status != "disabled") | "\(.test) \(.status): \(.summary)"] | join("; ") end),
     assetID: (if $assetID != "" then $assetID else null end),
     profile: $profile,
     host: ($host[0] + {dmi: ($inv.dmi // {})}),
     expectedGPUs: $expected,
     detectedGPUs: ($inv.gpuCount // 0),
     gpus: ($inv.gpus // []),
     startedAt: ([$tests[].startedAt | select(. != "")] | min // ""),
     tests: $tests}'
}
