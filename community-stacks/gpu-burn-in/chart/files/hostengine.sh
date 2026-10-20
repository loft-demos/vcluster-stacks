# Sourced by the DCGM tests. Starts a DCGM host engine inside the pod, on the GPUs the pod was
# given. The GPU Operator's own DCGM and dcgm-exporter are disabled in this Stack, because active
# diagnostics need the GPUs to themselves.
start_hostengine() {
  nv-hostengine >/dev/null 2>&1 || true
  i=0
  until dcgmi discovery -l >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 30 ] || { echo "DCGM host engine did not start"; return 1; }
    sleep 1
  done
  dcgmi discovery -l
}
