#!/usr/bin/env bash
# Exercise the real desktop through the agent, under Docker fallback restrictions.
set -euo pipefail
[[ $# == 1 ]] || { echo 'usage: image/test-desktop.sh <desktop-image>' >&2; exit 1; }
cid=$(docker run -d --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
    --tmpfs /tmp:rw,nosuid,nodev,mode=1777 --tmpfs /run:rw,nosuid,nodev,mode=0755 \
    --tmpfs /root:rw,nosuid,nodev,mode=0700 "$1")
cleanup() {
    status=$?
    if [[ $status != 0 ]]; then docker logs "$cid"; fi
    docker rm -f "$cid" >/dev/null 2>&1 || true
}
trap cleanup EXIT
for ((i=0; i<100; i++)); do
    if docker exec "$cid" test -S /run/boxpool/agent.sock; then break; fi
    sleep 0.1
done
docker exec -i "$cid" node - <<'JS'
const http = require('http');
const assert = require('assert');
const {execFileSync} = require('child_process');
const net = require('net');
const request = (method, path, body) => new Promise((resolve, reject) => {
  const req = http.request({socketPath: '/run/boxpool/agent.sock', method, path}, res => {
    let body = ''; res.on('data', b => body += b);
    res.on('end', () => { assert.equal(res.statusCode, 200, body); resolve(JSON.parse(body)); });
  }); req.on('error', reject); req.end(body);
});
(async () => {
  assert((await request('GET', '/v1/version')).features.includes('desktop'));
  const d = await request('POST', '/v1/desktop');
  assert.equal(d.display, ':0'); assert.equal(d.vnc, 'unix:/run/boxpool/desktop.vnc');
  assert.equal(d.human_control, false);
  await request('POST', '/v1/desktop'); // starting twice retains the session
  for (const port of [5900, 6000]) await new Promise((resolve, reject) => {
    const s = net.connect(port, '127.0.0.1');
    s.on('connect', () => { s.destroy(); reject(new Error(`private desktop listens on TCP ${port}`)); });
    s.on('error', e => { assert.equal(e.code, 'ECONNREFUSED'); resolve(); });
  });
  // A real WebSocket upgrade through the agent, without a TCP websockify
  // listener. VNC sends its banner and waits while we test takeover policy.
  const ws = await new Promise((resolve, reject) => {
    const req = http.request({socketPath: '/run/boxpool/agent.sock', path: '/v1/desktop/ws', headers: {
      Connection: 'Upgrade', Upgrade: 'websocket', 'Sec-WebSocket-Version': '13',
      'Sec-WebSocket-Key': 'Ym94cG9vbC1kZXNrdG9wIQ=='
    }});
    req.on('upgrade', (res, socket) => { assert.equal(res.statusCode, 101); socket.on('error', () => {}); resolve(socket); });
    req.on('response', res => reject(new Error(`WebSocket status ${res.statusCode}`)));
    req.on('error', reject); req.end();
  });
  const start = performance.now();
  assert((await request('POST', '/v1/desktop/control', '{"human_control":true}')).human_control);
  const elapsed = performance.now() - start;
  assert(elapsed < 1000, `takeover took ${elapsed}ms`);
  assert.equal(execFileSync('tigervncconfig', ['-display', ':0', '-get', 'AcceptKeyEvents']).toString().trim(), '1');
  assert.equal(execFileSync('tigervncconfig', ['-display', ':0', '-get', 'AcceptPointerEvents']).toString().trim(), '1');
  assert(!(await request('POST', '/v1/desktop/control', '{"human_control":false}')).human_control);
  await request('POST', '/v1/desktop/control', '{"human_control":true}');
  ws.destroy();
  for (let i = 0; ; i++) {
    if (!(await request('GET', '/v1/desktop')).human_control) break;
    assert(i < 100, 'disconnect did not release control');
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  console.log(`Real agent WebSocket, takeover (${elapsed.toFixed(1)}ms), release and disconnect reset: OK`);
})().catch(e => { console.error(e); process.exit(1); });
JS
docker exec "$cid" sh -ec '
    test -S /run/boxpool/desktop.vnc
    DISPLAY=:0 xdpyinfo >/dev/null
    screenshot=$(mktemp /tmp/boxpool-desktop.XXXXXX)
    trap '\''rm -f "$screenshot"'\'' EXIT
    # Chromium/Openbox can replace windows while xwd queries their attributes.
    # Retry that startup race, but require an actual nonempty capture to pass.
    i=0
    until DISPLAY=:0 xdotool search --onlyvisible --class chromium >/dev/null 2>&1 \
        && DISPLAY=:0 xwd -root -silent -out "$screenshot" \
        && test -s "$screenshot"; do
        i=$((i+1))
        if test "$i" -ge 100; then
            echo "desktop did not produce a Chromium screenshot" >&2
            exit 1
        fi
        sleep 0.1
    done
    test "$(tigervncconfig -display :0 -get AcceptKeyEvents)" = 0
    test "$(tigervncconfig -display :0 -get AcceptPointerEvents)" = 0
    tigervncconfig -display :0 -set AcceptKeyEvents=1 AcceptPointerEvents=1
    test "$(tigervncconfig -display :0 -get AcceptKeyEvents)" = 1
    tigervncconfig -display :0 -set AcceptKeyEvents=0 AcceptPointerEvents=0
'
echo 'Desktop agent startup, Chromium, screenshot and VNC input policy: OK'
