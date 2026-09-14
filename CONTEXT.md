# Agent Platform

Language for the general-purpose agent platform that replaces the existing LangGraph-based agent backend.

## Language

**Agent platform**:
The domain-agnostic service that runs the hub and journey subagents. Vacation planning and shift swapping are reference journeys, not concepts built into the platform.

**Chat backend**:
The deep module that accepts UI input through AG-UI and exposes a narrow in-process seam to the agent platform.

**Conversation thread**:
A persistent conversation that a user can leave, resume later, or reconnect to while its current turn is still running.

**Journey**:
A declarative specialist definition consisting of a system prompt reference, allowed external MCP tools, and skills. Tool input and output schemas belong to the MCP server.

**Journey context**:
The persistent conversation-specific history and state retained for a journey across turns. It can exist while no journey is actively running.

**Active specialist**:
The journey subagent that currently owns specialist execution for a conversation.

**Explicit target**:
An optional stable journey identifier supplied with a user message so the hub routes it to that journey. UI mention syntax is outside the agent platform language.

**Handoff message**:
An explicit task or message plus platform-selected conversation context sent to a journey, which continues from its own retained journey context.

**Definition version**:
An immutable compiled journey bundle referenced by retained journey context. It identifies the prompt, tool allowlist, and skill bundle, but does not freeze external model or MCP server behavior.

**Steering**:
A correction or additional instruction sent while a journey subagent is active. It is delivered with the conversation context at the next safe point.

**Human interaction**:
A durable request for one user's input that pauses a journey at a safe boundary. It is either a clarification question or an explicit action approval.

**Clarification question**:
A structured question with choices and optional custom text. Its answer supplies information; it does not authorize an action.

**Action approval**:
A one-time allow or deny decision bound to one exact proposed tool call and its arguments. Changed arguments require a new approval.

**Awaiting input**:
A durable journey state with no active execution owner. An authorized answer can later resume the pinned journey context on any replica without replaying completed tools.
