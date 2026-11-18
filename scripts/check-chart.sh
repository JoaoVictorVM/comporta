#!/usr/bin/env bash
# Lints the chart, renders the example, validates the rendered configuration
# with the gateway itself and checks that invalid values are refused.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
chart=deploy/helm
example=examples/kubernetes/values.yaml
cd "$root"
work="$(mktemp -d .chart-check.XXXXXX)"
trap 'rm -rf "$work"' EXIT
# Rendering offline has no cluster to ask, and a helm built from source would
# assume a default below the chart's minimum. Without a fixed version every
# refusal below could pass for the wrong reason.
kube=(--kube-version 1.31.0)

helm lint "$chart" -f "$example" "${kube[@]}"
helm template check "$chart" -f "$example" "${kube[@]}" > "$work/manifests.yaml"
helm template check "$chart" -f "$example" "${kube[@]}" --show-only templates/configmap.yaml |
  sed -n '/comporta.yaml: |/,$p' | tail -n +2 | sed 's/^    //' > "$work/comporta.yaml"
go run ./cmd/comporta config validate "$work/comporta.yaml"

grep -q 'readOnlyRootFilesystem: true' "$work/manifests.yaml"
grep -q 'runAsNonRoot: true' "$work/manifests.yaml"
grep -q 'automountServiceAccountToken: false' "$work/manifests.yaml"
if grep -q 'subPath:' "$work/manifests.yaml"; then
  echo "secrets must be mounted without subPath" >&2
  exit 1
fi

refuse() {
  local reason
  if reason="$(helm template check "$chart" -f "$example" "${kube[@]}" "$@" 2>&1 >/dev/null)"; then
    echo "values accepted but must be refused: $*" >&2
    exit 1
  fi
  if grep -q 'kubeVersion' <<<"$reason"; then
    echo "refused for the Kubernetes version, not for the values: $*" >&2
    exit 1
  fi
}
refuse --set replicas=1
refuse --set cluster.id=Not_Valid
refuse --set cluster.configRevision=0
refuse --set storage.redis.address=user:pw@redis:6379
refuse --set-string config.deployment.mode=single
echo "chart checks passed"
