# az-agent-platform-go

Planning seed for the Go agent platform; production implementation has not started. The name does not require Azure hosting.

Start with the [six-deliverable plan](docs/superpowers/plans/2026-09-14-agent-platform/index.md) and [approved design](docs/superpowers/specs/2026-09-14-hub-and-spoke-agent-platform-design.md). The plan contains the task checklist, acceptance mapping, handoff and portable framework/spike evidence.

For deterministic acceptance checks and real-boundary E2E evidence, see [E2E setup](docs/e2e-setup.md).

## Azure OpenAI through LiteLLM

Configure the LiteLLM proxy with an Azure OpenAI deployment hosted in Foundry. The `model_name` alias is what this app sends to the proxy's Responses API:

```yaml
model_list:
  - model_name: azure-alias
    litellm_params:
      model: azure/<deployment-name>
      api_base: os.environ/AZURE_API_BASE
      api_key: os.environ/AZURE_API_KEY
      api_version: os.environ/AZURE_API_VERSION
```

Start the proxy with `litellm --config config.yaml`, then configure the app:

```dotenv
AZ_AGENT_MODEL_PROVIDER=litellm
AZ_AGENT_LITELLM_BASE_URL=http://localhost:4000
AZ_AGENT_LITELLM_API_KEY=proxy-api-key
AZ_AGENT_LITELLM_MODEL=azure-alias
```

`AZURE_API_BASE` is the Azure OpenAI resource endpoint; `AZ_AGENT_LITELLM_API_KEY` authenticates the app to LiteLLM. The app sends `store: false` and its complete transient message/tool-result history on each Responses request. Without `AZ_AGENT_MODEL_PROVIDER`, the existing direct Foundry provider remains selected.

See [LiteLLM's Azure Responses guide](https://docs.litellm.ai/docs/providers/azure/azure_responses) and [OpenAI Go SDK](https://github.com/openai/openai-go) for proxy and client details.

[Provenance](docs/PROVENANCE.md) records the original copied records and the subsequent user-requested simplification. Source-repository paths in the design are historical evidence references.
