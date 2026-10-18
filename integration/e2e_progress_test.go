package integration_test

// e2eReachedStage is the progress ratchet for TestE2EConversationAgent:
// stages <= e2eReachedStage must pass; later stages skip as pending.
// Each hop writes its stage body test-first, implements, and bumps this in the same commit.
const e2eReachedStage = 8
