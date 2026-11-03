# Desktop and browser takeover

The optional `boxpool-desktop` image adds TigerVNC's virtual X server,
Openbox, Chromium, fonts and X inspection tools. The base image keeps its
existing packages. Audio and GPU acceleration are not included.

## Install the image on each desktop-capable node

Desktop artifacts are built and published separately from the pinned base
release. Until a desktop release is pinned by the binary, install the tested
artifact explicitly; selecting desktop never silently falls back to base.
Build on a machine with Docker BuildKit (use `amd64` on x86 Linux):

```sh
IMAGE_VARIANT=desktop IMAGE_VERSION=desktop-dev image/build.sh arm64
image/test-desktop.sh boxpool-desktop:desktop-dev-arm64
```

For Firecracker or Virtualization.framework, decompress the matching
`dist/image/desktop/<arch>/current/rootfs-desktop-<arch>.ext4.zst` into a
host-owned, immutable base disk. Set `BOXPOOL_DESKTOP_ROOTFS` to its absolute
path in the **daemon's environment** and restart the daemon. The default
lookup is `rootfs-desktop.ext4` next to the unpacked base rootfs in the
artifacts directory. The desktop uses the existing base image's kernel.
Do not change or replace a disk while a sandbox is being created.

For Docker fallback, load the matching OCI image into the daemon's Docker
engine and set `BOXPOOL_DESKTOP_IMAGE=boxpool-desktop:desktop-dev-arm64`
in the daemon's environment. A tag or digest is required. Both variants
also publish versioned multi-architecture images and checksummed ext4
artifacts in the guest-image release workflow.

A source build embeds the new agent. Existing base images do not gain the
desktop API until rebuilt. A desktop sandbox cold-boots; it never restores
the base image's warm memory.

## Open and control a desktop

Use the same head-side Tailscale setup and device access as
[expose](expose.md). On Linux, prefix commands with `sudo` when talking to
the installed root daemon. Open a printed URL in your regular browser if
the root shell cannot launch one.

```sh
boxpool sh --image desktop --name browser
# Optionally place it on a specific node: add --os macos or --node <label>.
boxpool desktop browser
# Headless/SSH session:
boxpool desktop --no-open browser
```

`sh` creates the sandbox but does not start the display. `desktop` starts it
on demand, prints a secret HTTPS URL and opens the browser. Open that same
link on a laptop or a phone connected to the tailnet. The URL exchanges its
expose token for a Secure, HttpOnly cookie and removes the secret from the
address bar. Keep the printed link private: its holder can watch and take
over the sandbox. The viewer and all noVNC assets are embedded; no CDN is
required.

The viewer starts in **Watching · read only** mode. TigerVNC rejects key,
pointer and clipboard input in this mode. **Take over** enables mouse and
keyboard and changes the header to **You are in control**; **Release
control** disables them again without reconnecting. On a phone, use
**Keyboard** to open the on-screen keyboard. Only one viewer can connect at
a time. Closing it releases control; a second viewer gets a conflict until
the first disconnects. Clipboard transfer and remote display resizing are
disabled in both modes.

Agents use `DISPLAY=:0` for browser automation. In the desktop image it is
set in the environment. Agents can start the desktop directly through
`POST /v1/desktop` on their agent channel, or the host's
`POST /v1/sandboxes/<name>/desktop` endpoint. The response reports
`display`, `vnc` (`unix:/run/boxpool/desktop.vnc`) and `human_control`.
`GET` on the same endpoint reports the current control flag. Browser
agents should pause automation while `human_control` is true and resume
when it becomes false: takeover cooperatively hands control to a human;
it does not suspend arbitrary agent processes.

VNC and X have no TCP listener. The viewer's WebSocket reaches the Unix
VNC socket only through the agent's vsock or private Docker socket and
the head's authenticated proxy, identically on Linux and macOS nodes.
Chromium uses `--no-sandbox` inside the existing VM/container isolation
boundary because the guest runs as root without container capabilities.

```sh
boxpool desktop rm browser
```

This closes the URL and active viewer and revokes its token, keeping the
desktop running. Revoking the token with `boxpool token revoke <id>` also
closes an active viewer within the normal token-check interval (2 seconds).
Desktop URLs appear in `boxpool expose ls` with reserved port **0**, which
is a service identifier, not a guest TCP port. To create a fresh link,
remove the existing URL first. `--public` is never accepted for a desktop.

## Validation

Automated image CI starts the desktop in a container with a read-only root,
no capabilities or network, opens Chromium and captures a nonempty XWD
screenshot. Unit tests cover image selection, base warm-cache exclusion,
server input policy transitions, disconnect release, single-viewer
exclusion, proxy authentication and local/remote node routing.

Manual acceptance checks on real Linux and macOS nodes:

1. Install the matching desktop disk/image above; run
   `boxpool sh --image desktop --name browser`. In another terminal run
   `boxpool desktop browser`; Chromium should be visible.
2. Open the printed link on a tailnet-connected phone, then close that
   viewer and reconnect from a laptop. Both should scale the display.
3. Run browser automation with `DISPLAY=:0`. Click **Take over**, type a URL
   into Chromium and navigate. Input should become active within one second;
   **Release control** must immediately stop viewer keyboard/mouse input.
4. Close the viewer during takeover. Reopen it: it must be read-only.
   Revoke its token or run `boxpool desktop rm browser`: the active viewer
   must disconnect and the old link must no longer grant access.
