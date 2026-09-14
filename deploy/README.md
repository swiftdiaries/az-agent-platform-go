# Task 6 deployment artifacts

These manifests are deliberately scoped to the task-6 application labels and,
for Kubernetes, the `agent-platform-task-6` namespace. No cluster apply is part
of this change.

The checked-in values are configuration markers, not production credentials or
service endpoints. Supply the real Keycloak issuer/JWKS/audience and claim
paths, Foundry project endpoint/deployment, Java MCP endpoint, and Azure
workload identity client ID before starting the service. The journey ConfigMap
keeps the Java `Cookie` and `Authorization` forwarding allowlist and expands
`TEST_JAVA_MCP_ENDPOINT` at process startup. Raw header values and database
credentials stay outside the journey file and ConfigMap.

## Compose

Use a task-specific project and an environment outside the repository. The
application has three internal replicas and no host-published app port, so
replicas cannot collide on a host port. Publish a separate ingress in the
environment that owns ingress routing when one is available.

```sh
export POSTGRES_PASSWORD='from-your-secret-store'
export DATABASE_URL='postgres://agent_platform:from-your-secret-store@postgres:5432/agent_platform?sslmode=disable'
export TEST_JAVA_MCP_ENDPOINT='https://java.example.invalid/mcp'
export KEYCLOAK_JWT_ISSUER='https://keycloak.example.invalid/realms/example'
export KEYCLOAK_JWKS_URL='https://keycloak.example.invalid/realms/example/protocol/openid-connect/certs'
export KEYCLOAK_JWT_AUDIENCE='agent-platform'
export KEYCLOAK_JWT_SOURCE='authorization_bearer'
export KEYCLOAK_JWT_HEADER='Authorization'
export KEYCLOAK_USER_CLAIM_PATH='/sub'
export KEYCLOAK_TENANT_CLAIM_PATH='/tenant'
export KEYCLOAK_JWT_ALLOWED_ALGORITHMS='RS256'
export AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT='https://example.projects.ai.azure.com/projects/example'
export AZ_AGENT_FOUNDRY_DEPLOYMENT='example-deployment'
export AZURE_CLIENT_ID='workload-identity-client-id'
export OTEL_SERVICE_NAME='az-agent-platform'
export OTEL_SERVICE_VERSION='local-task-6'
export OTEL_EXPORTER_OTLP_ENDPOINT=''
export OTEL_EXPORTER_OTLP_HEADERS=''

export COMPOSE_PROJECT_NAME=agent-platform-task-6
docker compose -f deploy/compose.yaml config --quiet
docker compose -f deploy/compose.yaml up --build
```

The example endpoints and IDs above are command-shape examples only; replace
them before use. `docker compose down --volumes` from this project removes only
the task-6 Compose network and `agent-platform-task-6-pgdata` volume.

## Kubernetes

Create the task-scoped database Secret before applying the manifest. It is
intentionally absent from Git. The `database-url` value must use the in-cluster
service name `agent-platform-postgres`.

```sh
kubectl create namespace agent-platform-task-6 --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic agent-platform-postgres \
  --namespace agent-platform-task-6 \
  --from-literal=database=agent_platform \
  --from-literal=username=agent_platform \
  --from-literal=password="$POSTGRES_PASSWORD" \
  --from-literal=database-url="$DATABASE_URL"
```

If OTLP export is enabled, create `agent-platform-telemetry` separately with
the Secret key `headers`; omit that Secret to disable header injection. The
OTLP endpoint itself is nonsecret and belongs in the ConfigMap. The service
version is required so traces identify the deployed revision.

Before applying, replace every `<required-...>` marker in
`deploy/kubernetes.yaml` with the supplied nonsecret deployment values. Set the
workload identity client ID on the task-6 ServiceAccount and configure the
cluster's Azure workload identity webhook. The application uses
`DefaultAzureCredential`; no API key or credential value belongs in this
repository.

Validate and inspect the rendered objects without changing the current context:

```sh
kubectl apply --dry-run=client -f deploy/kubernetes.yaml
kubectl diff --server-side --field-manager=task-6 --dry-run=server -f deploy/kubernetes.yaml
```

When an approved deployment is needed, use the task namespace explicitly:

```sh
kubectl apply -f deploy/kubernetes.yaml
kubectl -n agent-platform-task-6 rollout status deployment/agent-platform
kubectl -n agent-platform-task-6 get pods,svc,statefulset,pvc -l app.kubernetes.io/managed-by=task-6
```

`Deployment` keeps three replicas through an additive rolling update and has no
session affinity. Readiness checks `/readyz`; liveness checks `/livez`. Both
application and PostgreSQL pods use a 30-second termination grace period while
the app's configured drain is 25 seconds, leaving a small signal/cleanup
buffer. A graceful stop fails readiness before draining owned work. A forced
stop leaves the existing lease/reaper semantics to mark work interrupted; the
manifest does not add handoff or replay behavior.

Definition rollout is additive: mount a retained, immutable journey ConfigMap
key at a second `/app/configs/...` subPath and add its path to
`AGENT_PLATFORM_RETAINED_CONFIGS` before activating the new candidate. Keep old
keys until no database row references their digest. Do not overwrite a retained
bundle in place.

Cleanup is explicit and task-scoped. Verify the target first, then remove only
resources labelled `app.kubernetes.io/managed-by=task-6` in the task namespace:

```sh
kubectl -n agent-platform-task-6 get all,pvc,configmap,secret -l app.kubernetes.io/managed-by=task-6
kubectl delete namespace agent-platform-task-6
```

Deleting the namespace also deletes its task-6 PVC and Secret. Never substitute
the current namespace or a broad cluster-wide selector.

The final image build and cluster acceptance remain blocked until the integrated
Task 6 process/auth/provider sources and real Keycloak, Foundry, Java, image
registry, workload identity, and cluster values are available.
