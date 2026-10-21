# blue-bubbles

- `instructions` and the `*Description` constants in `internal/gateway/mcp.go` are prompts that decide when models call the tools. Reword them only when the task is to change that behavior.
- `README.md` is the reference for routing, the outbound format, config, flags, the HTTP API, the MCP tools, and the watch statuses. A change to any of them updates its `README.md` section and the tests in `internal/gateway/gateway_test.go` in the same commit.
- Nothing in `internal/` reads a flag. `cmd/blue-bubbles/` wires the packages together, and the README's Develop section maps them.
- Phone numbers, tokens, and chat GUIDs live in the gateway Mac's config file, never in this repo.
