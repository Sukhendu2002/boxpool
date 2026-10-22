# Guest agent protocol

`boxpool-agent` runs inside every sandbox. The host (the node daemon) talks
to it to run commands. This page describes every request and message, so a
client can be written without reading the agent's code.

Protocol version: **1**.

## Transport

HTTP/1.1, served by the agent on:

| Backend | Address | Agent flag |
| --- | --- | --- |
| VM (Firecracker, vz) | vsock port 1024 | `--listen vsock:1024` (the default) |
| Docker | unix socket `/run/boxpool/agent.sock` | `--listen unix:/run/boxpool/agent.sock` |

There is no authentication: the transport is reachable only from the host.
The `Host` header is ignored.

## `GET /v1/version`

Returns the protocol version and the agent's build version.

```http
GET /v1/version HTTP/1.1
Host: agent
```

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"protocol":1,"agent":"0.1.0"}
```

`protocol` changes only when a change breaks existing clients. A client
should check it before anything else and refuse a version it does not know.

## `GET /v1/exec`

Runs one command. The request is a WebSocket handshake
([RFC 6455](https://www.rfc-editor.org/rfc/rfc6455)); everything after it
is WebSocket messages. It is a `GET`, not a `POST`, because RFC 6455
requires WebSocket handshakes to be `GET` requests.

```http
GET /v1/exec HTTP/1.1
Host: agent
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
```

The agent answers `101 Switching Protocols`.

### Messages

There are two kinds of WebSocket message:

- **Binary** messages carry stream data. The first byte is the stream id,
  the rest is the data:

  | Id | Stream | Direction |
  | --- | --- | --- |
  | `0` | stdin | client → agent |
  | `1` | stdout | agent → client |
  | `2` | stderr | agent → client |

  For example, the bytes `01 68 69 0a` are `hi\n` on stdout.
- **Text** messages are JSON control messages. Each has a `type`.

Neither side sends a message larger than 32768 bytes (stream id included),
so the default read limit of common WebSocket libraries is enough. Long
data is split across several messages.

Both sides ignore control messages with an unknown `type` and binary
messages with an unknown stream id, so new ones can be added without a new
protocol version.

#### Exec request (client → agent, first message)

The first message must be a text message with the command to run:

```json
{"argv":["sh","-c","echo hi; exit 3"],"env":["FOO=bar"],"cwd":"/root","tty":false,"rows":24,"cols":80}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `argv` | array of strings | Required. The command and its arguments. `argv[0]` is looked up in the agent's `PATH` unless it contains a `/`. No shell is involved. |
| `env` | array of `KEY=value` | Optional. Added to the agent's environment; a key that is already set is overridden. |
| `cwd` | string | Optional. Working directory; the agent's own when empty. |
| `tty` | bool | Optional. Run the command on a new PTY. |
| `rows`, `cols` | integers | Optional, PTY only. Initial window size; 24 × 80 when either is missing or 0. |

In PTY mode, set `TERM` in `env` (for example `TERM=xterm-256color`):
the agent does not choose one.

#### `resize` (client → agent)

Sets the PTY's window size; the command gets `SIGWINCH`. Ignored without a
PTY, or when `rows` or `cols` is 0.

```json
{"type":"resize","rows":40,"cols":120}
```

#### `stdin_eof` (client → agent)

Closes the command's stdin, so it reads end-of-file. Ignored in PTY mode,
where the client sends Ctrl-D (`04`) as stdin data instead, like a
terminal does.

```json
{"type":"stdin_eof"}
```

#### `exit` (agent → client, final)

The command has exited and all its output has been sent. This is the last
message; the agent then closes the WebSocket with status 1000 (normal).

```json
{"type":"exit","code":3}
```

`code` is the exit status, or 128 plus the signal number when a signal
killed the command (for example 130 for `SIGINT`, 137 for `SIGKILL`), as
in a shell.

#### `error` (agent → client, final)

The command could not be run: a bad exec request, a command that is not
found, a working directory that does not exist. No `exit` follows; the
agent closes the WebSocket with status 1000.

```json
{"type":"error","message":"exec: \"nope\": executable file not found in $PATH"}
```

### Streams

Without a PTY, stdin, stdout and stderr are three pipes, and stdout and
stderr arrive on streams 1 and 2.

With a PTY, the command's stdin, stdout and stderr are all the PTY, so all
of its output arrives on stream 1 and nothing is sent on stream 2. The PTY
behaves like a terminal: input is echoed, Ctrl-C (`03`) sends `SIGINT` to
the foreground process group, and output line endings are `\r\n`.

### Lifecycle

1. The client opens the WebSocket and sends the exec request.
2. The agent starts the command, without a PTY in a new process group, with
   a PTY in a new session.
3. Data and control messages flow both ways until the command exits.
4. The agent sends the rest of the output, then `exit` (or `error` if the
   command never started), then closes the WebSocket.

Output written before the command exits is always delivered. Once the
command has exited, the agent stops reading output after 250 ms without
any: a background child that keeps stdout open does not hold the session
open. Background children are not killed when the command exits normally.

If the WebSocket closes or fails before `exit`, the agent kills the
command's whole process group with `SIGKILL`. Nothing that stayed in that
group outlives the session. The same happens to every session when the
agent is stopped with `SIGTERM` or `SIGINT`.

### Example

`sh -c 'echo hi; exit 3'` without a PTY:

| Direction | Kind | Content |
| --- | --- | --- |
| client → agent | text | `{"argv":["sh","-c","echo hi; exit 3"]}` |
| client → agent | text | `{"type":"stdin_eof"}` |
| agent → client | binary | `01 68 69 0a` (stdout `hi\n`) |
| agent → client | text | `{"type":"exit","code":3}` |
| agent → client | close | 1000 |
