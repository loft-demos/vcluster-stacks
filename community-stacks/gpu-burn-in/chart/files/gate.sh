#!/bin/sh
# Init container, tools image. When stopOnFailure is on and an earlier step failed, leaves
# $WORK/skip for the run and publish containers, so a bad server is not stressed for hours.
set -eu
. /scripts/lib.sh

[ "$STOP_ON_FAILURE" = "true" ] || exit 0
failed=$(kubectl get configmaps -l "$RESULT_LABEL=true" -o json |
  jq -r --arg self "$TEST" '[.items[].data | select(.test != $self and (.status == "failed" or .status == "error")) | .test] | join(",")')
if [ -n "$failed" ]; then
  echo "an earlier test failed: $failed" >"$WORK/skip"
  log "skipping: an earlier test failed: $failed"
fi
