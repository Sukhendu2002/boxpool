# Preview an app from a phone

Run `boxpool expose <sandbox> <port>` on the head. The sandbox can run on
any node. A phone on mobile data needs the Tailscale app, signed into the
same tailnet and connected. `--public` removes Boxpool's token requirement;
the URL remains reachable only on the tailnet.

## Prepare the head

1. Install [Tailscale](https://tailscale.com/download) on the head and the
   phone. Start Tailscale on the head and sign in (`tailscale up` on Linux).
   On macOS, start the Tailscale app and sign in there. The daemon needs
   `tailscale` on its PATH; on macOS Boxpool also checks
   `/Applications/Tailscale.app/Contents/MacOS/Tailscale`.
2. In the tailnet's [DNS settings](https://login.tailscale.com/admin/dns),
   enable MagicDNS and HTTPS Certificates. Tailscale Serve obtains and
   renews the certificate. See Tailscale's
   [HTTPS guide](https://tailscale.com/docs/how-to/set-up-https-certificates).
3. The OS account running the Boxpool daemon must be able to configure
   Tailscale Serve. A root Linux daemon already has that access. For an
   unprivileged Linux daemon, an administrator can grant operator access
   with `sudo tailscale set --operator=<daemon-user>`; substitute the actual
   service account. Test from that account, rather than just your terminal's
   login account. On macOS use the account running the Tailscale app.
4. With Boxpool's daemon running, run `boxpool expose check` on the head.
   Expected: Tailscale connected, MagicDNS and HTTPS certificates enabled,
   followed by the head's hostname. This runs under the daemon's account
   and environment without changing Serve configuration. Creating an
   expose additionally verifies that account's permission to configure it.
5. Allow the phone to reach the head's exposed HTTPS ports in your tailnet
   policy. Each expose gets a distinct port; enabling only port 443 will
   not cover the allocated ports. Keep ordinary firewall access restricted
   to the tailnet. See the [Serve CLI reference](https://tailscale.com/docs/reference/tailscale-cli/serve).

`expose` fails before starting an interactive HTTPS-enablement prompt when
Tailscale is stopped, logged out, MagicDNS is off or HTTPS is unavailable.
It reports the missing setting and where to change it. Other Boxpool
features do not require Tailscale Serve.

## Guest images

The image's agent must advertise `tcp` in `GET /v1/version`. Updating the
head binary alone does not update agents already stored inside sandbox
disks or containers. The currently pinned `v0.0.1-alpha.1` image predates
that endpoint; it must be replaced before previews work with defaults.
An old agent makes the preview answer 502 with an image-update message.

Release and rollout checklist:

1. Merge the agent and expose changes into `main`, then tag a release using
   the [release workflow](development.md#releases). The binary workflow
   explicitly invokes the guest-image workflow for token-created releases.
   Both architectures must pass their OCI checks, and amd64 must pass the
   KVM smoke, before the dependent image publish job runs. The checks
   require `tcp` and exercise actual HTTP forwarding through it.
2. Confirm the release contains VM assets and `SHA256SUMS`, and GHCR has
   both architecture images and their multi-architecture index. Download
   and verify them using the [image guide](../image/README.md#ci-and-release).
3. Update `imageVersion`, VM checksums and exact download sizes in
   `internal/artifacts/artifacts.go`, and `DefaultImage`'s verified index
   digest in `internal/backend/docker/config.go`, from those published
   artifacts. Never invent or pre-fill a digest for an unpublished image.
   Build and release the consumer binary with these pins.
4. Back up work from existing sandboxes, then recreate the ones that need
   previews using the updated base image. Stopping and starting an old
   sandbox, or restoring its old disk snapshot, retains its old agent.
   The Docker backend's create API also accepts an explicit `image` tag
   or digest before the default pin is updated.

For development before publication, build with `image/build.sh <arch>` and
use the backend integration harness described in
[development.md](development.md#linux-testing-for-each-pr), including
`BOXPOOL_TEST_ROOTFS` for Firecracker. This does not update production
image pins or existing sandbox disks.

## Check the preview

1. Start an HTTP app on port 8000 inside the sandbox. For an image with
   Node.js, run this in `boxpool sh box`:

   ```sh
   node -e 'require("http").createServer((_, r) => r.end("hello from boxpool\n")).listen(8000, "127.0.0.1")'
   ```

2. On the head, run `boxpool expose box 8000`. Open the printed token link
   on the connected phone with Wi-Fi disabled. Expected: `hello from boxpool`.
3. Open the URL without its token in a private browser window. Expected:
   401. Open the printed token link there; expected: a redirect to the clean
   URL and a Secure, HttpOnly authentication cookie.
4. Expose a dev server's port and edit its source. Check that its browser
   WebSocket connects and hot reload updates the page. Its same-origin
   Origin is translated to match the localhost Host the guest sees;
   foreign Origins remain unchanged for the app to reject.
5. Run `boxpool expose rm box 8000`. New requests fail and existing
   WebSocket sessions disconnect.

## Cookies and application compatibility

Application cookies are encrypted and namespaced per expose, then made
Secure, HttpOnly and host-only. Names, paths and lifetimes are restored for
the corresponding app; sibling exposes receive none of its session cookies.
Server-side login, logout and sessions survive daemon restarts. Existing
raw browser cookies are ignored, so log in again after this upgrade.

JavaScript `document.cookie` reads and writes are unsupported, including
client-side double-submit CSRF cookie integrations. Use server-managed
cookies or another application session mechanism. Non-cookie CORS behavior
also remains the app's responsibility: these ports are different origins
on the same site, and a sibling page can still send cross-origin requests.
