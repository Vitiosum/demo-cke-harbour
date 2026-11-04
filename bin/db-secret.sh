#!/usr/bin/env bash
# Creates the Kubernetes Secret "harbour-db" from the variables of a Clever Cloud
# PostgreSQL add-on. Nothing is printed: the values go from clever-tools to the
# cluster through a pipe, and the Secret is applied server-side (no copy of the
# values in a last-applied annotation).
#
# Usage: bin/db-secret.sh <addon_id>     (KUBECONFIG must point at the cluster)
set -euo pipefail

addon_id="${1:?usage: bin/db-secret.sh <addon_id>}"
namespace="${NAMESPACE:-harbour}"

clever addon env "$addon_id" --format json \
  | jq --arg ns "$namespace" '
      (if type == "array" then map({(.name): .value}) | add else . end)
      | with_entries(select(.key | startswith("POSTGRESQL_ADDON_")))
      | if length == 0 then error("no POSTGRESQL_ADDON_ variable found for this add-on") else . end
      | {apiVersion: "v1", kind: "Secret", type: "Opaque",
         metadata: {name: "harbour-db", namespace: $ns},
         stringData: .}' \
  | kubectl apply --server-side --field-manager=harbour-db-secret -f - >/dev/null

keys="$(kubectl -n "$namespace" get secret harbour-db -o json | jq -r '.data | keys | join(", ")')"
echo "Secret harbour-db ready in namespace $namespace with: $keys (values not shown)"
