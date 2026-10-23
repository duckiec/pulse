**Title:** Show HN: Pulse – A zero-trust, CGO-free API client (Postman/Bruno alternative)

**Body:**
I got tired of waiting for 600MB Electron API clients to load just to send a GET request, and I was frustrated by the proprietary file formats that lock your API collections into a specific tool.

I built Pulse. It’s a single CGO-free Go binary that uses standard RFC 7230 `.http` text files. 

What makes it different from just another CLI runner:
1. **Agent Firewall:** If you use Claude or Cursor via MCP to test your APIs, Pulse acts as a zero-trust proxy. You can define a `.pulse-guard` file that blocks destructive methods (`DELETE`, `POST`) or rate-limits the LLM so it can't accidentally drop your database or leak your OS keyring credentials into the chat context.
2. **`pulse mock --from-cache`:** It caches successful requests in local SQLite. If you go offline, you can instantly spin up a local mock server that replays your real API traffic with zero configuration.
3. **`pulse record`:** A built-in reverse proxy that watches your local traffic and automatically generates standard `.http` files with secrets redacted out.
4. **Wasm Sandbox:** Pre-request scripts run in a zero-capability Wazero sandbox instead of un-sandboxed Node.js.

It has a Bubble Tea TUI for interactive exploration, but runs the exact same engine headlessly for CI (`pulse test`). 

Repo: [https://github.com/duckiec/pulse](https://github.com/duckiec/pulse)
Would love feedback from terminal power-users and anyone automating their API tests with LLMs.
