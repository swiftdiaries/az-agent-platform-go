# Task 6B Foundry model adapter

`internal/provider/foundry` supports one specific Azure AI Foundry mode: an
AAD-authenticated **project endpoint** used with a model deployment through the
Foundry Responses API. It does not infer or support a generic Foundry endpoint,
an API key, or a server-side Foundry agent endpoint.

The new nonsecret ConfigMap contract is:

| Key | Value |
| --- | --- |
| `AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT` | Azure AI Foundry project endpoint, such as `https://example.projects.ai.azure.com/projects/project-name` |
| `AZ_AGENT_FOUNDRY_DEPLOYMENT` | Foundry model deployment name |

Process composition must create an AAD `azcore.TokenCredential` and pass it to
`foundry.ConfigFromEnv` or `foundry.New`. Workload identity or a Secret-backed
credential belongs in composition; this package neither reads credentials from
environment variables nor persists them. No endpoint, deployment, or usable
credential was supplied for this task, so live Foundry evidence is **BLOCKED**.

The adapter uses the pinned MAF `foundryprovider` for Foundry routing and AAD
authentication. It sends `store:false`, disables MAF automatic function calling,
and sets the OpenAI SDK retry limit to zero. A provider response can therefore
return function calls, but only the existing product runtime dispatches them and
continues the durable history. The adapter sends the exact MAF message history,
including prior function calls and results; it creates no provider session.

`ModelRequest.OnDispatch` is called once at the final serialized HTTP request
boundary. The runtime uses it to materialize admitted input only at that
boundary. It is not called for cancellation before dispatch, credential setup,
or a request construction error.

Fixture verification:

```sh
GOCACHE=$PWD/.cache/go-build go test ./internal/provider/foundry ./internal/runtime -count=1
```

The fixture covers project-path routing, AAD authorization, deployment, tools,
returned function calls, `store:false`, zero retries, sanitized auth/internal
errors, and cancellation. It is not real Foundry acceptance evidence.
