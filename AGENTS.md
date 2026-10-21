# blue-bubbles

`README.md` is the reference: routing rules, config, the HTTP API, and the MCP tools. This file holds only what it doesn't.

- Tool descriptions and server instructions in `mcp.go` are the text that makes models call the tools. Change them only on purpose.
- A change to the routing rules updates "How a text is routed" in `README.md` and the tests in `gateway_test.go`.
- `go test ./...` runs everything against a fake `imsg`. Nothing in the tests sends a real text.
- Phone numbers, tokens, and chat GUIDs live in the gateway Mac's config file, never in this repo.
