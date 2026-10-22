# Guest agent protocol

`boxpool-agent` runs inside every sandbox. The host (the node daemon) talks
to it to run commands, transfer files and list listening TCP ports. This
page describes every request and message, so a client can be written
without reading the agent's code.

Protocol version: **1**.

## Transport

HTTP/1.1, served by the agent on:

| Backend | Address | Agent flag |
| --- | --- | --- |
| VM (Firecracker, vz) | vsock port 1024 | `--listen vsock:1024` (the default) |
| Docker | unix socket `/run/boxpool/agent.sock` | `--listen unix:/run/boxpool/agent.sock` |

There is no authentication: the transport is reachable only from the host.
On a unix socket, the agent holds an exclusive lock on `<path>.lock` while
it listens, and refuses to start when another agent holds it, another
process answers on the path, or the path is not a socket; it replaces a
socket nobody listens on.
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

If the WebSocket closes or fails before `exit` has been sent, the agent
kills the command's whole process group with `SIGKILL`, also when the
command itself has already exited and only its output is still being
sent. Nothing that stayed in that group outlives the session. The same
happens to every session when the agent is stopped with `SIGTERM` or
`SIGINT`.

The agent holds up to 1 MiB of stdin that the command has not read yet,
however many messages it arrived in, and keeps reading messages meanwhile,
so `resize`, `stdin_eof` and a closed connection are seen even while the
command ignores its stdin. Beyond that it stops reading messages until the
command reads (or exits), which pushes back on a client that sends faster
than the command reads. So a client that queues more than 1 MiB of stdin
to a command that never reads it, then disconnects, is noticed only when
the command reads, writes or exits, or the agent stops.

### Example

`sh -c 'echo hi; exit 3'` without a PTY:

| Direction | Kind | Content |
| --- | --- | --- |
| client → agent | text | `{"argv":["sh","-c","echo hi; exit 3"]}` |
| client → agent | text | `{"type":"stdin_eof"}` |
| agent → client | binary | `01 68 69 0a` (stdout `hi\n`) |
| agent → client | text | `{"type":"exit","code":3}` |
| agent → client | close | 1000 |

## File transfer

Both endpoints stream **uncompressed tar** (`application/x-tar`), without
buffering whole files. Paths are URL query values: percent-encode spaces,
`&`, `+`, `#` and other special characters. Relative paths are relative to
the agent's working directory.

Regular files, directories and symbolic links are supported. File and
directory mode bits (including executable, set-id and sticky bits) and
mtimes are preserved; PAX headers retain subsecond mtimes. Directory
metadata is applied after all contents have been written. Symlink targets
and mtimes are preserved without following the link; symlink permissions
are preserved where the OS supports them (Linux always uses `0777`).
Ownership is not preserved: extracted entries belong to the agent's user,
with group assignment following the destination filesystem's rules. There
is no ACL, extended-attribute or hard-link identity preservation. Hard-link,
device, FIFO and other unsupported tar entry types are rejected, not skipped.

### `GET /v1/files?path=SRC`

Returns `200 OK` with a tar rooted at `base(SRC)`, after cleaning the path.
A regular file or symlink gives one entry. A directory gives its own entry
and every descendant, without following symlinks, including a symlink
passed as `SRC`. A filesystem root or `.` is represented by `.`.

For example, `/work/project` gives `project`, `project/run.sh`,
`project/subdir`, etc.; `/work/project/run.sh` gives just `run.sh`.

```sh
curl --fail --unix-socket /run/boxpool/agent.sock \
  'http://agent/v1/files?path=/work/project' -o project.tar
# Extract into an existing host directory (only for a trusted guest archive):
tar -xf project.tar -C ./download
```

The tree is checked for unsupported entry types before streaming; rejected
entries use the JSON shape below. If reading a source fails after streaming
has begun (for example because it changes during the transfer),
the agent aborts the HTTP response: no valid completed tar is returned.
Clients must read the HTTP body to EOF and check for transport errors,
even after the tar reader reaches its end. Host-side safe extraction of
untrusted guest archives belongs to `boxpool cp` (#16).

### `PUT /v1/files?path=DEST`

Extracts the request-body tar **into the existing directory** `DEST`;
returns `204 No Content`. It does not rename the archive root to `DEST`.
For example, a tar with `project/run.sh` sent to `/work` creates
`/work/project/run.sh`. A tar containing only `run.sh` creates `/work/run.sh`.
Missing parent directories within the archive are created. Existing regular
files and symlinks at file-entry destinations are replaced; existing
directories are merged. Conflicts between directories and other entry types
are rejected. A failed PUT may leave entries written before the failure,
including temporary directory permissions; it is not transactional.

```sh
# ./project contains the directory to copy. /work must already exist.
# Avoid platform-specific metadata entries when preparing the tar.
tar -cf project.tar -C . project
curl --fail --unix-socket /run/boxpool/agent.sock -X PUT \
  -H 'Content-Type: application/x-tar' --data-binary @project.tar \
  'http://agent/v1/files?path=/work'
```

Absolute entry names and cleaned names that escape `DEST` via `..` are
rejected. `DEST` itself may be a symlink to a directory; it is resolved
before extraction. Below it, symlink parents are rejected, even when their
targets are inside `DEST`. A symlink can point anywhere (including outside
`DEST`, or to a missing target), but later entries cannot write through it.
These checks protect against archive-directed traversal; the guest is not
a security boundary against concurrent changes by its own processes.

### HTTP errors

The file and ports endpoints return `application/json` errors:

```json
{"message":"entry \"../escape\": path escapes destination"}
```

- `400 Bad Request`: absent/empty `path`, non-directory upload destination,
  invalid tar, rejected entry or extraction failure. Archive errors name the
  entry; an unreadable header names the preceding entry or `<header>`.
- `404 Not Found`: source path or destination directory does not exist.
- `500 Internal Server Error`: other failures looking up a path or reading
  listening ports (including an unsupported operating system).

## `GET /v1/ports`

Returns `200 OK`, `application/json`, with a snapshot of listening TCP
sockets in the guest's network namespace:

```json
[{"proto":"tcp","address":"0.0.0.0","port":8000},{"proto":"tcp","address":"::1","port":8080}]
```

Only state `0A` (`LISTEN`) from `/proc/net/tcp` and `/proc/net/tcp6` is
included; established connections and UDP are excluded. IPv6 addresses
use normal text notation (for example `::ffff:192.0.2.1` for an IPv4-mapped
address). An empty snapshot is `[]`. Each listening socket has an entry;
there is no promised ordering, deduplication or port watching. Missing
`/proc/net/tcp6` is allowed when IPv6 is disabled. This endpoint requires
Linux; other operating systems return an explicit unsupported error.

```sh
python3 -m http.server 8000
# In another terminal:
curl --fail --unix-socket /run/boxpool/agent.sock http://agent/v1/ports
# Includes {"proto":"tcp","address":"0.0.0.0","port":8000}.
```

## Go client

`internal/agent.Client` exposes `Upload(ctx, dest, tar io.Reader) error`,
`Download(ctx, src) (io.ReadCloser, error)` and `Ports(ctx) ([]Port, error)`
for the daemon. `Upload` consumes an already-prepared tar stream;
`Download` returns the HTTP response body, which the caller must drain to
EOF (checking read errors) and close. Cancelling the context interrupts the
request/transfer. Non-success responses include the HTTP status and the
agent's JSON error message in the returned error. No CLI is added here.
