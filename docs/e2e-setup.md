# E2E setup and evidence

Use this guide to run the deterministic acceptance fixture and, when real
dependencies are available, collect the separate real-boundary evidence. A
fixture pass does not prove Keycloak, Foundry, Java MCP, or Kubernetes.

## Prerequisites

- Go `1.26.0` and Docker. The integration suite starts an isolated
  `postgres:16` container unless `TEST_DATABASE_URL` points to an isolated
  PostgreSQL 16 instance.
- For a real deployment: a Keycloak issuer and test JWT, an Azure AI Foundry
  **project endpoint** and deployment, a Java MCP endpoint, and either Compose
  Azure environment credentials or Kubernetes workload identity.
- Keep secrets outside the repository. Start from
  `deploy/config/agent-platform.env.example`; it names every Compose input.

## Local fixture and race checks

From the repository root, with Docker running:

```sh
GOCACHE=$PWD/.cache/go-build go test -race ./... -count=1 -timeout=120s
go vet ./...
```

For the three-service HTTP acceptance path specifically:

```sh
GOCACHE=$PWD/.cache/go-build go test -race ./integration -run 'TestAcceptanceThreeServiceHTTP' -count=1 -timeout=120s
```

The fixture creates disposable databases and uses model/MCP peers in-process.
Set `TEST_DATABASE_URL` only to an isolated PostgreSQL 16 database; do not aim
the suite at a shared or production database.

## Compose real-service check

Fill the required values in a secret environment file outside this repository,
then load it and render the Compose configuration:

```sh
set -a
. /secure/path/agent-platform.env
set +a
export COMPOSE_PROJECT_NAME=agent-platform-task-6
docker compose -f deploy/compose.yaml config --quiet
docker compose -f deploy/compose.yaml up --build
```

In another terminal, confirm PostgreSQL and all application replicas are
running:

```sh
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs --tail=100 agent-platform
```

Compose deliberately does not publish the application port. Through the
approved ingress for that environment, check both endpoints:

```sh
curl -fsS https://<approved-ingress>/livez
curl -fsS https://<approved-ingress>/readyz
```

`/livez` only proves that the process serves HTTP. `/readyz` also proves that
the process is not draining and PostgreSQL can validate the loaded definition
registry; neither endpoint calls Foundry or Java MCP.

## Kubernetes real-service check

Follow [the deployment guide](../deploy/README.md) to create the task-scoped
database Secret and replace the manifest's required nonsecret placeholders.
Validate before applying, then wait for the rollout:

```sh
kubectl apply --dry-run=client -f deploy/kubernetes.yaml
kubectl diff --server-side --field-manager=task-6 --dry-run=server -f deploy/kubernetes.yaml
kubectl apply -f deploy/kubernetes.yaml
kubectl -n agent-platform-task-6 rollout status deployment/agent-platform
kubectl -n agent-platform-task-6 get pods,svc,statefulset,pvc -l app.kubernetes.io/managed-by=task-6
```

For a direct health check, run this in a separate terminal while the command
below remains open:

```sh
kubectl -n agent-platform-task-6 port-forward service/agent-platform 8080:8080
curl -fsS http://127.0.0.1:8080/livez
curl -fsS http://127.0.0.1:8080/readyz
```

## Foundry model route

The Foundry adapter uses MAF's `foundryprovider` with an AAD-authenticated
Azure AI Foundry project endpoint. It sends each model turn to the **Responses
API**, not the legacy Completions API; the adapter's wire-contract test requires
the `/openai/v1/responses` path. `AZ_AGENT_FOUNDRY_DEPLOYMENT` is the Foundry
deployment name, so it may name a deployment of GPT-5.6 when that deployment is
available in the target Foundry project. Model availability remains a Foundry
deployment decision, not a limitation imposed by this application's API path.

## Record the boundary honestly

For each real run, record the exact Git revision, Go version, PostgreSQL 16
version, rendered-manifest checks, and redacted command results. Record three
ready pods; a real Keycloak-signed request and expired-token result; one AAD
Foundry project-endpoint call with its deployment name; and one Java MCP call
with the opaque tool-call ID and W3C trace context visible on both sides. Do
not record prompts, credentials, raw business identifiers, or secret values.

Mark a case **PASS** only when its real boundary was exercised and recorded.
Missing credentials, an unavailable cluster, or a fixture-only run is
**BLOCKED**, not a real E2E pass. See [the acceptance ledger](acceptance.md)
for the case-to-test mapping and current status.
