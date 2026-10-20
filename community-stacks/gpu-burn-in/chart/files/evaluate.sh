# Judges one GPU test from the raw output run.sh left in $WORK, and writes $WORK/result.json:
#   {"status": passed|failed|skipped|disabled|error, "summary": "...", "details": {...}}
# Sourced by publish.sh, and by tests/evaluate-test.sh against fixtures. Needs only sh, jq and
# POSIX awk, and never calls kubectl.
#
# failed means the hardware did not meet the bar. error means the test itself could not run, for
# example a timeout or a host engine that did not start; the report counts both as not qualified.

# result STATUS SUMMARY [DETAILS_JSON]
result() {
  details=${3:-}
  [ -n "$details" ] || details='{}'
  # Only a test that ran has measurements; zeros for a disabled or skipped test would read as
  # "ran and found nothing".
  case $1 in
    passed | failed)
      details=$(echo "$details" | jq --arg test "$TEST" "($METRICS)"' as $m | . + {metrics: ($m | with_entries(select(.value != null)))}') ;;
  esac
  jq -n --arg status "$1" --arg summary "$2" --argjson details "$details" \
    '{status: $status, summary: $summary, details: $details}' >"$WORK/result.json"
}

# The metrics contract: a flat object of numbers per test, derived from its details. The report
# collects them per test, and the recorder publishes them from QualificationReports, so any source
# of qualification results (another test suite, NICo Machine Validation) can fill the same names.
METRICS='
  def sum(f): [f] | add // 0;
  if $test == "inventory" then
    {gpus: .gpuCount, allocatable_gpus: .allocatable,
     uncorrected_ecc_errors: sum(.gpus[]?.uncorrectedECC // 0), inactive_nvlinks: .inactiveNVLinks}
  elif $test == "dcgmdiag" then
    {failed_tests: sum(.results[]?.failed // [] | length), skipped_tests: sum(.results[]?.skipped // [] | length),
     warned_tests: sum(.results[]?.warned // [] | length), level: (.level // "" | tonumber? // null)}
  elif $test == "nccl" then
    (.collectives // {} | with_entries({key: "\(.key)_bus_bandwidth_gbps", value: .value.avgBusBandwidthGBps}))
      + {gpus: .gpus}
  elif $test == "soak" then
    {peak_temp_c: ([.telemetry[]?.maxTempC] | max), peak_power_w: ([.telemetry[]?.maxPowerW] | max),
     gpus_with_xid: sum(.telemetry[]? | select(.lastXid > 0) | 1),
     throttle_samples: sum(.telemetry[]?.thermalOrPowerThrottleSamples), samples: sum(.telemetry[]?.samples),
     cycles: .cycles, minutes: .minutes}
  elif $test == "gpuready" then
    {allocatable_gpus: .allocatable}
  else {} end'


# DCGM JSON -> {"rc", "parsed", "anyFail", "failed": [names], "skipped": [names], "warned": [names]}.
# Walks the whole document for test objects with results, so it does not depend on the exact
# DCGM 4 schema; any result status of Fail anywhere fails the test.
dcgm_summary() {
  json=$1 rc=$2
  if ! jq -e . "$json" >/dev/null 2>&1; then
    jq -n --argjson rc "$rc" '{rc: $rc, parsed: false}'
    return
  fi
  jq --argjson rc "$rc" '
    def s: (if type == "string" then ascii_downcase else "" end);
    def named($want): [.. | objects | select(has("name") and has("results"))
      | select(any(.results[]?; (.status | s) as $x | $want | index($x))) | .name] | unique;
    {rc: $rc, parsed: true,
     anyFail: ([.. | objects | .status? | s] | index("fail") != null),
     passed: named(["pass"]),
     failed: named(["fail"]),
     skipped: named(["skip", "skipped", "not run"]),
     warned: named(["warn", "warning"])}' "$json"
}

# Judges dcgm-<name>.json files. Prints "failed: reason" or nothing, and the merged summary JSON
# into $WORK/dcgm-summary.json.
judge_dcgm() {
  verdict=""
  echo '{}' >"$WORK/dcgm-summary.json"
  for name in "$@"; do
    json=$WORK/$name.json
    [ -f "$json" ] || continue
    rc=$(cat "$WORK/$name.rc" 2>/dev/null || echo 0)
    s=$(dcgm_summary "$json" "$rc")
    jq --arg k "$name" --argjson v "$s" '.[$k] = $v' "$WORK/dcgm-summary.json" >"$WORK/dcgm-summary.tmp" &&
      mv "$WORK/dcgm-summary.tmp" "$WORK/dcgm-summary.json"
    if [ "$(echo "$s" | jq -r .parsed)" != "true" ]; then
      verdict="${verdict}${verdict:+; }$name: no JSON result (dcgmi exited $rc)"
      continue
    fi
    failed=$(echo "$s" | jq -r '.failed | join(",")')
    skipped=$(echo "$s" | jq -r '.skipped | join(",")')
    if [ -n "$failed" ] || [ "$(echo "$s" | jq -r .anyFail)" = "true" ]; then
      verdict="${verdict}${verdict:+; }$name failed: ${failed:-see log}"
    elif [ "$rc" != 0 ]; then
      verdict="${verdict}${verdict:+; }$name: dcgmi exited $rc"
    elif [ -n "$skipped" ] && [ "$FAIL_ON_SKIP" = "true" ]; then
      verdict="${verdict}${verdict:+; }$name skipped: $skipped (failOnSkip)"
    fi
  done
  echo "$verdict"
}

evaluate_inventory() {
  [ -s "$WORK/gpus.csv" ] || { result failed "nvidia-smi reported no GPUs"; return; }
  gpus=$(jq -R -s '
    def num: if test("^[0-9.]+$") then tonumber else null end;
    [split("\n")[] | select(length > 0) | split(",") | map(gsub("^ +| +$"; "")) |
      {index: .[0] | num, uuid: .[1], name: .[2], serial: .[3], pciBusId: .[4],
       memoryMiB: .[5] | num, driver: .[6], vbios: .[7], uncorrectedECC: .[8] | num,
       temperatureC: .[9] | num}]' "$WORK/gpus.csv")
  count=$(echo "$gpus" | jq length)
  inactive=$(grep -ci inactive "$WORK/nvlink.txt" 2>/dev/null || true)
  dmi=$(jq -R -s '[split("\n")[] | select(contains("=")) | capture("^(?<k>[^=]+)=(?<v>.*)$")] | map({(.k): .v}) | add // {}' "$WORK/dmi.txt" 2>/dev/null || echo '{}')
  details=$(jq -n --argjson gpus "$gpus" --argjson dmi "$dmi" --argjson inactive "${inactive:-0}" \
    --arg allocatable "$(cat "$WORK/allocatable" 2>/dev/null || true)" \
    '{gpuCount: ($gpus | length), gpus: $gpus, dmi: $dmi, inactiveNVLinks: $inactive,
      allocatable: ($allocatable | tonumber? // null)}')

  problems=""
  [ "$count" -eq "$EXPECTED_GPUS" ] || problems="$count of $EXPECTED_GPUS GPUs"
  if [ -n "$GPU_MODEL" ]; then
    wrong=$(echo "$gpus" | jq -r --arg m "$GPU_MODEL" '[.[] | select(.name | ascii_downcase | contains($m | ascii_downcase) | not) | .name] | unique | join(",")')
    [ -z "$wrong" ] || problems="${problems}${problems:+; }not $GPU_MODEL: $wrong"
  fi
  ecc=$(echo "$gpus" | jq -r '[.[] | select((.uncorrectedECC // 0) > 0) | "GPU \(.index)=\(.uncorrectedECC)"] | join(",")')
  [ -z "$ecc" ] || problems="${problems}${problems:+; }uncorrected ECC errors: $ecc"
  [ "${inactive:-0}" -eq 0 ] || problems="${problems}${problems:+; }$inactive inactive NVLink links"
  allocatable=$(cat "$WORK/allocatable" 2>/dev/null || true)
  if [ -n "$allocatable" ] && [ "$allocatable" -lt "$count" ]; then
    problems="${problems}${problems:+; }device plugin advertises $allocatable of $count GPUs: one is unhealthy"
  fi

  model=$(echo "$gpus" | jq -r '[.[].name] | unique | join(",")')
  if [ -n "$problems" ]; then
    result failed "$problems" "$details"
  else
    result passed "$count x $model" "$details"
  fi
}

evaluate_dcgmdiag() {
  verdict=$(judge_dcgm dcgm-diag dcgm-extra)
  details=$(jq --arg level "$DCGM_DIAG_LEVEL" --arg extra "$DCGM_EXTRA_TESTS" '{level: $level, extraTests: $extra, results: .}' "$WORK/dcgm-summary.json")
  if [ ! -f "$WORK/dcgm-diag.json" ]; then
    result error "dcgmi diag did not run" "$details"
  elif [ -n "$verdict" ]; then
    result failed "$verdict" "$details"
  else
    result passed "dcgmi diag -r $DCGM_DIAG_LEVEL${DCGM_EXTRA_TESTS:+ and $DCGM_EXTRA_TESTS} passed" "$details"
  fi
}

evaluate_nccl() {
  details='{}' problems="" passedOps=""
  for op in all_reduce all_gather reduce_scatter; do
    out=$WORK/nccl-$op.txt
    [ -f "$out" ] || { problems="${problems}${problems:+; }$op did not run"; continue; }
    rc=$(cat "$WORK/nccl-$op.rc" 2>/dev/null || echo 1)
    bw=$(awk -F: '/Avg bus bandwidth/ { gsub(/ /, "", $2); print $2 }' "$out")
    oob=$(awk -F: '/Out of bounds values/ { sub(/^ +/, "", $2); print $2 }' "$out")
    details=$(echo "$details" | jq --arg op "$op" --argjson rc "$rc" --arg bw "$bw" --arg oob "$oob" \
      '.[$op] = {rc: $rc, avgBusBandwidthGBps: ($bw | tonumber? // null), outOfBounds: $oob}')
    if [ "$rc" != 0 ]; then
      problems="${problems}${problems:+; }$op exited $rc"
    elif [ "$oob" != "0 OK" ]; then
      problems="${problems}${problems:+; }$op wrong results: ${oob:-no check line}"
    elif [ -z "$bw" ]; then
      problems="${problems}${problems:+; }$op reported no bandwidth"
    elif awk -v bw="$bw" -v min="$NCCL_MIN_BUS_BW" 'BEGIN { exit !(min > 0 && bw + 0 < min + 0) }'; then
      problems="${problems}${problems:+; }$op ${bw} GB/s below $NCCL_MIN_BUS_BW"
    else
      passedOps="${passedOps}${passedOps:+, }$op ${bw} GB/s"
    fi
  done
  gpus=$(cat "$WORK/nccl-gpus" 2>/dev/null || echo 0)
  details=$(echo "$details" | jq --argjson g "$gpus" '{gpus: $g, collectives: .}')
  if [ -n "$problems" ]; then
    result failed "$problems" "$details"
  else
    result passed "$gpus GPUs: $passedOps" "$details"
  fi
}

# dcgmi dmon lines look like "GPU 0   45   250.123   100   0   0". Non-numeric samples (N/A) are
# ignored. Clock event reason bits 3, 5, 6 and 7 are HW slowdown, SW thermal, HW thermal and HW
# power brake; they are reported, not failed on, because power capping is normal under load.
telemetry() {
  awk '
    function bit(v, b) { return int(v / (2 ^ b)) % 2 }
    $1 == "GPU" && $3 ~ /^[0-9.]+$/ {
      g = $2; n[g]++
      if ($3 + 0 > maxT[g]) maxT[g] = $3 + 0
      if ($4 ~ /^[0-9.]+$/ && $4 + 0 > maxP[g]) maxP[g] = $4 + 0
      if ($6 ~ /^[0-9]+$/ && $6 + 0 > 0) xid[g] = $6 + 0
      if ($7 ~ /^[0-9]+$/) { v = $7 + 0; if (bit(v, 3) || bit(v, 5) || bit(v, 6) || bit(v, 7)) thr[g]++ }
    }
    END {
      printf "["
      sep = ""
      for (g in n) {
        printf "%s{\"gpu\":%s,\"samples\":%d,\"maxTempC\":%s,\"maxPowerW\":%s,\"lastXid\":%d,\"thermalOrPowerThrottleSamples\":%d}", sep, g, n[g], maxT[g] + 0, maxP[g] + 0, xid[g] + 0, thr[g] + 0
        sep = ","
      }
      print "]"
    }' "$1"
}

evaluate_soak() {
  tel=$(telemetry "$WORK/dmon.txt" 2>/dev/null || echo '[]')
  [ -n "$tel" ] || tel='[]'
  cycles=$(cat "$WORK/soak-cycles" 2>/dev/null || echo 0)
  set --
  i=1
  while [ "$i" -le "$cycles" ]; do set -- "$@" "soak-$i"; i=$((i + 1)); done
  verdict=$(judge_dcgm "$@")
  details=$(jq -n --argjson tel "$tel" --argjson dcgm "$(cat "$WORK/dcgm-summary.json")" \
    --arg load "$SOAK_LOAD" --arg minutes "$SOAK_MINUTES" --argjson cycles "$cycles" \
    '{load: $load, minutes: ($minutes | tonumber), cycles: $cycles, telemetry: ($tel | sort_by(.gpu)), results: $dcgm}')

  problems="$verdict"
  samples=$(echo "$tel" | jq '[.[].samples] | add // 0')
  [ "$samples" -gt 0 ] || problems="${problems}${problems:+; }no telemetry samples"
  hot=$(echo "$tel" | jq -r --argjson max "$MAX_GPU_TEMP_C" '[.[] | select(.maxTempC > $max) | "GPU \(.gpu) \(.maxTempC)C"] | join(",")')
  [ -z "$hot" ] || problems="${problems}${problems:+; }above ${MAX_GPU_TEMP_C}C: $hot"
  xids=$(echo "$tel" | jq -r '[.[] | select(.lastXid > 0) | "GPU \(.gpu) Xid \(.lastXid)"] | join(",")')
  [ -z "$xids" ] || problems="${problems}${problems:+; }$xids"
  if [ "$SOAK_LOAD" = "dcgm" ]; then
    # A GPU DCGM does not support skips targeted_stress and targeted_power. Without a passed load
    # test the soak never loaded the GPU, which must not count as a pass.
    loaded=$(jq '[.[] | .passed? // [] | length] | add // 0' "$WORK/dcgm-summary.json")
    if [ "$cycles" -eq 0 ]; then
      problems="${problems}${problems:+; }no load cycle ran"
    elif [ "$loaded" -eq 0 ]; then
      problems="${problems}${problems:+; }DCGM ran no load test; every cycle was skipped"
    fi
  fi

  peak=$(echo "$tel" | jq -r '[.[].maxTempC] | max // "n/a"')
  if [ -n "$problems" ]; then
    result failed "$problems" "$details"
  else
    result passed "${SOAK_MINUTES}m $SOAK_LOAD soak, $cycles cycles, peak ${peak}C" "$details"
  fi
}

evaluate() {
  if [ "${ENABLED:-true}" = "false" ]; then
    result disabled "disabled by a Stack parameter"
  elif [ -f "$WORK/skip" ]; then
    result skipped "$(cat "$WORK/skip")"
  elif [ ! -f "$WORK/rc" ]; then
    result error "the test did not run"
  elif [ "$(cat "$WORK/rc")" = 124 ]; then
    result error "timed out after ${RUN_TIMEOUT_SECONDS}s"
  elif [ "$TEST" = inventory ]; then
    # nvidia-smi failing to list GPUs is a hardware or driver finding, not a test error.
    evaluate_inventory
  elif [ "$(cat "$WORK/rc")" != 0 ]; then
    result error "test-$TEST.sh exited $(cat "$WORK/rc"); see the log"
  else
    "evaluate_$TEST"
  fi
}
