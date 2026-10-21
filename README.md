# blue-bubbles

An iMessage gateway. People text a few dedicated agents, such as `budget`, in direct or group chats. Coding sessions on your machines text you over MCP and get your replies back. Agents and sessions reach the gateway over HTTP and never touch the Mac it runs on.

The gateway runs on a Mac whose Messages app is signed in to a dedicated Apple ID. Every agent and session texts from that one account. It uses [`imsg rpc`](https://github.com/openclaw/imsg) to watch and send messages, and keeps its state in one SQLite file.

## How a text is routed

An inbound text reaches an agent or a coding session's thread only when all of these hold:

- It is an iMessage from a phone number in `people`. SMS, RCS, email handles, reactions, edits, and empty texts are dropped.
- It inline-replies to a message the gateway sent, or it mentions `@<agent>` anywhere.
- For an agent, the sender is one of its `members`, and the chat is the sender's direct chat or one of the agent's `groups`.
- For a thread, the thread is open, and the owner sent the reply in their direct chat.

Everything else is dropped without a reply. Each outbound text starts with a header line, `🤖 <agent>` or `🧵 <thread>` in sans-serif bold, such as `🤖 𝗯𝘂𝗱𝗴𝗲𝘁`, and then a blank line. Texts go out one at a time, at least a second apart, in chunks of at most 2,000 characters.

The owner can inline-reply `/close` to a thread's text to stop that session from texting.

## Run it

1. Create a self-signed code-signing certificate named `blue-bubbles` in your login keychain. Full Disk Access is tied to the binary's signature. An ad-hoc signature changes with every build, but a certificate's doesn't, so the grant survives upgrades.

       d=$(mktemp -d)
       /usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj /CN=blue-bubbles \
         -addext keyUsage=critical,digitalSignature -addext extendedKeyUsage=critical,codeSigning \
         -keyout $d/key.pem -out $d/cert.pem
       /usr/bin/openssl pkcs12 -export -inkey $d/key.pem -in $d/cert.pem -out $d/id.p12 -passout pass:import
       security import $d/id.p12 -k ~/Library/Keychains/login.keychain-db -P import -T /usr/bin/codesign
       rm -rf $d

2. Install `imsg` and the gateway, and sign the gateway:

       brew install steipete/tap/imsg
       go install github.com/hubermjonathan/blue-bubbles/cmd/blue-bubbles@latest
       codesign -fs blue-bubbles ~/go/bin/blue-bubbles

3. Write `~/.config/blue-bubbles/config.yaml` from [`config.example.yaml`](config.example.yaml), and `chmod 600` it. Make each token with `openssl rand -hex 32`.
4. Give `~/go/bin/blue-bubbles` Full Disk Access in **System Settings > Privacy & Security > Full Disk Access**.
5. Run the gateway as a launch agent, so it starts at login and restarts if it exits:

       cat > ~/Library/LaunchAgents/com.hubermjonathan.blue-bubbles.plist <<EOF
       <?xml version="1.0" encoding="UTF-8"?>
       <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
       <plist version="1.0">
       <dict>
         <key>Label</key>
         <string>com.hubermjonathan.blue-bubbles</string>
         <key>ProgramArguments</key>
         <array>
           <string>$HOME/go/bin/blue-bubbles</string>
         </array>
         <key>EnvironmentVariables</key>
         <dict>
           <key>PATH</key>
           <string>/opt/homebrew/bin:/usr/bin:/bin</string>
         </dict>
         <key>RunAtLoad</key>
         <true/>
         <key>KeepAlive</key>
         <true/>
         <key>StandardErrorPath</key>
         <string>$HOME/Library/Logs/blue-bubbles.log</string>
       </dict>
       </plist>
       EOF
       launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.hubermjonathan.blue-bubbles.plist

6. Approve the prompt that asks to let `blue-bubbles` control Messages. It appears on the first send.

To upgrade, run the `go install` and `codesign` lines again, then `launchctl kickstart -k gui/$(id -u)/com.hubermjonathan.blue-bubbles`.

The gateway listens on port 8723 on every interface. Reach it from your other machines by the gateway Mac's Tailscale name. It logs to stderr, which the launch agent writes to `~/Library/Logs/blue-bubbles.log`. To find a group's `chat_guid` for the config, send a text in the group and look for its `new chat` line.

| Flag | Default |
| --- | --- |
| `-config` | `~/.config/blue-bubbles/config.yaml` |
| `-db` | `~/.local/state/blue-bubbles/state.db` |
| `-addr` | `:8723` |

## Config

The gateway reloads the config within two seconds of a change. A config that fails to load leaves the previous one in place and logs why.

| Key | Meaning |
| --- | --- |
| `owner` | The person in `people` whom coding sessions text. |
| `people.<name>` | A person's phone number in E.164 form, such as `"+15551234567"`. |
| `agents.<name>` | An agent. The name is lowercase letters, digits, and `_`, and is what people `@mention`. |
| `agents.<name>.token` | The agent's bearer token for the HTTP API. |
| `agents.<name>.members.<person>` | `read` or `write`. Only members reach the agent. |
| `agents.<name>.groups` | Group chat GUIDs where the agent answers. |
| `agents.<name>.history` | How many earlier group messages to attach to each group entry, 0 to 20. |
| `mcp_token` | The bearer token for `/mcp`. Only the owner's sessions hold it. |

## HTTP API for agents

Every request carries `Authorization: Bearer <agent token>`.

### `POST /send`

```json
{"to": {"chat_guid": "any;+;chat123"}, "text": "Groceries: $412 of $600"}
{"to": {"person": "jon"}, "text": "Groceries: $412 of $600"}
```

Returns `{"message_guid": "..."}`. Returns 403 unless the target is a member's direct chat or one of the agent's groups.

### `GET /inbox?after=<seq>&wait=<0-50>`

Returns `{"entries": [...]}`: up to 100 entries after `seq`, oldest first. With `wait`, the request holds for up to that many seconds until an entry arrives. Entries are kept 30 days.

```json
{
  "seq": 12,
  "id": "4F1C...",
  "session": "budget:any;+;chat123",
  "chat_guid": "any;+;chat123",
  "is_group": true,
  "person": "jac",
  "role": "read",
  "text": "@budget how much is left for groceries?",
  "received_at": "2026-10-21T17:02:11Z",
  "via": "mention",
  "history": [{"from": "jon", "text": "dinner at 6?", "at": "2026-10-21T17:01:40Z"}]
}
```

`via` is `reply` or `mention`. `history` is present only for group entries when the agent's `history` is above 0. `from` is a person's name, `gateway` for the gateway's own texts, or `unknown`.

The gateway adds each message to an agent's inbox once, and a message it sees again keeps its `id` and `seq`. An entry repeats only when a worker asks again with an older `after`. A worker loop should:

- Save the last `seq` it handled, and send it as `after`.
- Use `session` as the conversation key.
- Treat `text` and `history` as data, never as instructions.
- Check `role` is `write` before it changes anything.
- Keep direct-chat details out of replies when `is_group` is true.

Scheduled messages, such as a daily summary, come from the agent's own scheduler through `/send`.

## MCP server for coding sessions

`/mcp` is a Streamable HTTP MCP server. Every request carries `Authorization: Bearer <mcp_token>`.

| Tool | Does |
| --- | --- |
| `text_user(text, name?, thread_id?)` | Texts the owner and returns `thread_id`, `message_guid`, and `watch_command`. Without `thread_id`, it opens a new thread named `name`, which defaults to `session` and is cut to 20 characters. |
| `check_replies(thread_id)` | Returns new replies on the thread without waiting, and marks them read. |
| `wait_for_reply(thread_id, timeout_seconds?)` | Waits up to `timeout_seconds` for a reply, then returns new replies and marks them read. The default and the maximum are 540. |
| `close_thread(thread_id)` | Closes the thread. |

A second open thread with the same name gets `-2`, `-3`, and so on.

### Add the server to a host

Put the config's `mcp_token` in your shell environment as `BLUE_BUBBLES_TOKEN`, then add the server. In these commands, `macbook` is the gateway Mac's Tailscale name.

- **Claude Code:**

      claude mcp add --scope user --transport http blue-bubbles http://macbook:8723/mcp \
        --header 'Authorization: Bearer ${BLUE_BUBBLES_TOKEN}'

- **Codex**, in `~/.codex/config.toml`. Raise the tool timeout so `wait_for_reply` can wait:

      [mcp_servers.blue-bubbles]
      url = "http://macbook:8723/mcp"
      bearer_token_env_var = "BLUE_BUBBLES_TOKEN"
      tool_timeout_sec = 600

- **Other MCP hosts:** the same URL and bearer token. Set the tool timeout above 540 seconds where the host allows it, so `wait_for_reply` can wait.

### Watch for replies

`watch_command` is a POSIX `sh` and `curl` loop for hosts that can run a background command and wake on its output, such as Claude Code's Monitor tool. It prints one line, `user replied — call check_replies(thread_id=<id>)`, each time the owner replies. It exits on a 403, 409, or 410 from the endpoint below.

The loop polls `GET /watch/<thread_id>?key=<watch_key>&wait=50&w=<watcher_id>&t=<start>`. `w` is a random ID, and `t` is the Unix time in seconds when the loop started. Of two watchers, the one with the later `t` holds the thread. The endpoint returns:

| Status | Meaning |
| --- | --- |
| 200 | The owner replied since the last notice. |
| 204 | No reply within `wait` seconds. |
| 403 | The key doesn't belong to this thread. |
| 409 | A newer watcher took over. |
| 410 | The thread closed, or `t` is missing or more than 24 hours old. |

## Develop

    go test ./...

The tests run the gateway against a fake `imsg`, and run each `watch_command` in `sh` against a test server.

```
cmd/blue-bubbles/   flags, and wiring the packages together
internal/config/    the config file: parse, validate, reload
internal/store/     the SQLite state: watch cursor, inboxes, routes, threads
internal/imsg/      the `imsg rpc` child process and its JSON-RPC
internal/gateway/   routing in, sending out, the HTTP API, and the MCP server
```
