# Task 6 deployment artifacts

These manifests are deliberately scoped to the task-6 application labels and,
for Kubernetes, the `agent-platform-task-6` namespace. No cluster apply is part
of this change.

The checked-in values are configuration markers, not production credentials or
service endpoints. Supply the real Keycloak issuer/JWKS/audience and claim
paths, Foundry project endpoint/deployment, Java MCP endpoint, and Azure
workload identity client ID before starting the service. The Java MCP URL is
supplied by `AGENT_PLATFORM_MCP_ENDPOINTS`, and the forwarded headers by
`AGENT_PLATFORM_CREDENTIAL_HEADERS`, not by the journey file. Raw header values
and database credentials stay outside the journey file and ConfigMap.

## Compose

Use a task-specific project and an environment outside the repository. The
application has three internal replicas and no host-published app port, so
replicas cannot collide on a host port. Publish a separate ingress in the
environment that owns ingress routing when one is available.

```sh
export POSTGRES_PASSWORD='from-your-secret-store'
export DATABASE_URL='postgres://agent_platform:from-your-secret-store@postgres:5432/agent_platform?sslmode=disable'
export AGENT_PLATFORM_MCP_ENDPOINTS='java-monolith=https://java.example.invalid/mcp'
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
export AZURE_TENANT_ID='azure-tenant-id'
export AZURE_CLIENT_ID='workload-identity-client-id'
export AZURE_CLIENT_SECRET='from-your-secret-store'
export OTEL_SERVICE_NAME='az-agent-platform'
export OTEL_SERVICE_VERSION='local-task-6'
export OTEL_EXPORTER_OTLP_ENDPOINT=''
export OTEL_EXPORTER_OTLP_HEADERS=''

export COMPOSE_PROJECT_NAME=agent-platform-task-6
docker compose -f deploy/compose.yaml config --quiet
docker compose -f deploy/compose.yaml up --build
```

The example endpoints and IDs above are command-shape examples only; replace
them before use. Compose supports Azure environment credentials only:
`AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and `AZURE_CLIENT_SECRET` must come from
your secret environment. A host Azure CLI login is not a container credential
contract. `docker compose down --volumes` from this project removes only
the task-6 Compose network and `agent-platform-task-6-pgdata` volume.

### Local Compose overlay

Run from the repository root. `compose.local.yaml` overrides `compose.yaml`: it
keeps PostgreSQL and the app environment, uses one app replica, publishes only
`127.0.0.1:8080`, and mounts the checkout's `configs/` read-only. Its project
name `agent-platform-local` separates its network and PostgreSQL volume from
the base `agent-platform-task-6` project.

```sh
cp deploy/config/agent-platform.env.example deploy/config/local.env
# Fill deploy/config/local.env with real values before running Compose.
docker compose --env-file deploy/config/local.env -f deploy/compose.yaml -f deploy/compose.local.yaml config --quiet
docker compose --env-file deploy/config/local.env -f deploy/compose.yaml -f deploy/compose.local.yaml up --build -d
docker compose --env-file deploy/config/local.env -f deploy/compose.yaml -f deploy/compose.local.yaml logs -f agent-platform
docker compose --env-file deploy/config/local.env -f deploy/compose.yaml -f deploy/compose.local.yaml restart agent-platform
docker compose --env-file deploy/config/local.env -f deploy/compose.yaml -f deploy/compose.local.yaml down
```

`local.env` is Git-ignored and excluded from the Docker build. Compose uses it
to interpolate the base file's required values; the app does not read the file
directly. Exported shell variables override matching values in `local.env`,
including `COMPOSE_PROJECT_NAME`. Clear stale exports before running this local
project. Use `config --quiet` because the rendered environment contains
credentials. `restart` keeps the container's existing environment; after
editing `local.env`, run `up --build -d` to recreate it. `down` retains the
local PostgreSQL volume; `down --volumes` deletes that project's data.

Before a live journey, external Keycloak must issue an access token matching
the configured issuer, audience, signing algorithm, and claim paths. The Java
MCP endpoint must be reachable from the container and provide the configured
tools. Azure AI Foundry must expose the project and deployment to the service
principal. The journey forwards only allowlisted `Cookie` and `Authorization`
headers to Java MCP from each authenticated Chat request. Langfuse is optional:
for direct export, use the full OTLP traces URL and Basic authorization plus
ingestion-version headers shown in the env example.

This authenticated vacation-planner request can use only the configured
read-only `lookup_destination` tool. Supply a real Keycloak token in your shell;
the bearer token belongs in the HTTP header. Add a `Cookie` header if Java MCP
requires a session cookie. Use a fresh `runId` for each POST:

```sh
export KEYCLOAK_ACCESS_TOKEN='your-access-token'
curl -N -sS http://127.0.0.1:8080/ \
  -H "Authorization: Bearer ${KEYCLOAK_ACCESS_TOKEN}" \
  -H 'Content-Type: application/json' \
  --data-binary '{"threadId":"local-vacation-1","runId":"local-vacation-1-run-1","messages":[{"id":"local-vacation-message-1","role":"user","content":"Plan a vacation in Kyoto using the destination lookup."}],"tools":[],"context":[],"state":{},"forwardedProps":{"journeyId":"vacation-planner"}}'
```

The SSE stream starts with a state snapshot. A final answer followed by
`RUN_FINISHED` completes the run; `interaction.requested` followed by
`RUN_FINISHED` pauses it for a reply. If the model requests a clarification,
take the actual interaction `id`, question `id`, and an
offered option `label` from the event. POST this JSON with the same headers,
thread, a fresh run ID, and no messages:

```json
{"threadId":"local-vacation-1","runId":"local-vacation-1-run-2","messages":[],"tools":[],"context":[],"state":{},"forwardedProps":{"journeyId":"vacation-planner","interactionReply":{"interactionId":"<interaction.id>","kind":"clarification","answer":{"answers":{"<question.id>":{"option":"<offered option.label>"}}}}}}
```

The `answers` object must include every question in the event; the example
shows one question. Use `{"custom":"your answer"}` instead of the option
object for a custom answer. Clarification does not approve a business action.
To exercise the effectful `request_shift_swap` approval, start a separate
`shift-swap` run. If its stream requests an approval, inspect the proposed call
and submit its actual interaction `id` and `call.binding` with a fresh run ID,
no messages, and an explicit `approve` or `deny` decision:

```json
{"threadId":"local-shift-1","runId":"local-shift-1-run-2","messages":[],"tools":[],"context":[],"state":{},"forwardedProps":{"journeyId":"shift-swap","interactionReply":{"interactionId":"<interaction.id>","kind":"approval","answer":{"decision":"deny","binding":"<interaction.call.binding>"}}}}
```

POST replies to `http://127.0.0.1:8080/` with the same Authorization and
Content-Type headers. A forged binding, expired interaction, reused reply, or
wrong principal is rejected. The vacation journey cannot produce an effectful
approval.

Check `curl -fsS http://127.0.0.1:8080/livez` and `/readyz` after startup.
For a live journey, expect a final answer and `RUN_FINISHED` with no pending
interaction to confirm completion. `interaction.requested` means reply and
continue; `RUN_ERROR` means failure. If Langfuse export is enabled, inspect
received traces for the run's generic spans; trace receipt alone does not prove
a tool call or business outcome.

The app loads and digests `journeys.yaml`, prompts, and skills at process
startup; changing a mounted file does not reload a running process. `restart`
loads changed files, but existing sessions stay pinned to their old digest.
If a pinned bundle is absent after restart, startup fails closed. For a
reviewed definition rollout, retain the old immutable bundle and set
`AGENT_PLATFORM_RETAINED_CONFIGS`; do not overwrite retained files in place.
The overlay changes only the mount, not activation or retention rules.

Current behaviour, which supersedes the wording above where they differ: the
conversation prompt (`conversation_prompt`) and each journey's skill package
are loaded and digested with `journeys.yaml` at startup. Every retained set in
`AGENT_PLATFORM_RETAINED_CONFIGS` must use the same YAML schema and be mounted
alongside the current one. Idle conversations re-pin to the current set at
their next run (`definition_changed`), so a previous set must stay retained only
while some conversation referencing it has an open wait (a pending interaction
or an unfinished reply continuation) or an unresolved operation; until then a
replica without it is not Ready (bounded readiness).

### Tenancy, settings, credentials and cost gate

The app reads these variables (see `config/agent-platform.env.example`):

- `AGENT_PLATFORM_MCP_ENDPOINTS`: required allowlist `id=url,id2=url2`. URLs
  live only here.
- `AGENT_PLATFORM_TENANT_DIRECTORY`: path to a strict YAML file mapping each
  tenant to `product_version` and an `endpoint` ID from the allowlist. Example:
  `config/tenant-directory.yaml`, mounted read-only at
  `/app/tenancy/tenant-directory.yaml`.
- `AGENT_PLATFORM_SETTINGS`: path to a required strict YAML settings file
  (deploy default, tenant and user layers; `max_active_journeys` defaults to 2;
  `cost.profile`). Example: `config/settings.yaml`, mounted at
  `/app/tenancy/settings.yaml`. A missing file fails startup.
- `AGENT_PLATFORM_CREDENTIAL_HEADERS`: inbound header names Chat copies into the
  credential forwarded to MCP; default in these manifests `Cookie,Authorization`.
- `AGENT_PLATFORM_COST_GATE`: `open` (default, also when blank) or `headers`.
- `AGENT_PLATFORM_COST_HEADERS`: `key=Header-Name` pairs with keys `tenant`,
  `principal`, `conversation`, `run`, `journey`, `profile`. Blank uses
  `X-Agent-Tenant`, `X-Agent-Principal`, `X-Agent-Conversation`,
  `X-Agent-Run`, `X-Agent-Journey`, `X-Agent-Cost-Profile`.

Trust rule: with the `headers` gate the enforcement point must accept these
attribution headers only from Agent (mTLS or network policy); never from
end-user traffic. The tenancy files sit under `/app/tenancy`, not
`/app/configs`, so the overlay's `configs/` mount does not hide them. In
Kubernetes they come from the `agent-platform-tenancy` ConfigMap and
`journeys.yaml` from `agent-platform-journeys` (its `conversation.md` and
`skills/` are baked into the image).

### agentgateway profile (BLOCKED)

`compose.yaml` has an optional `agentgateway` service under the Compose profile
of the same name. It is a placeholder with a placeholder image tag and no
gateway configuration, and does not start by default. It is BLOCKED until a
cost API exists: the enforcement mechanism is undecided (see
`docs/migration.md`, Integration inputs). Do not rely on it for enforcement.

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
`DefaultAzureCredential`. The workload identity webhook must inject its
federated-token and tenant settings into the pod; no API key or credential value
belongs in this repository.

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

Cluster acceptance still requires real Keycloak, Foundry, Java, image registry,
workload identity, and cluster values. The historical Task 6 source-integration
blocker has been resolved in this checkout.
