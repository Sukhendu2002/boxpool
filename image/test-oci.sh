#!/usr/bin/env bash
# Real container checks: no --privileged, host sockets, network or extra caps.
set -euo pipefail
[[ $# == 2 ]] || { echo 'usage: image/test-oci.sh <image> <expected-version>' >&2; exit 1; }
image=$1 version=$2
[[ $(docker run --rm "$image" cat /etc/boxpool-image) == "$version" ]]
docker run --rm --network none "$image" sh -ec 'claude --version; codex --version; gemini --version'
cid=$(docker run -d --network none "$image")
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
for ((i=0; i<50; i++)); do
    if docker exec "$cid" test -S /run/boxpool/agent.sock; then break; fi
    sleep 0.1
done
docker exec "$cid" test -S /run/boxpool/agent.sock
docker exec -i -e EXPECTED_VERSION="$version" "$cid" node - <<'JS'
const http = require('http');
const assert = require('assert');
const timeout = setTimeout(() => { console.error('TCP agent smoke timed out'); process.exit(1); }, 10000);
const socketPath = '/run/boxpool/agent.sock';
http.get({socketPath, path: '/v1/version'}, res => {
  let body = '';
  res.on('data', b => body += b);
  res.on('end', () => {
    const version = JSON.parse(body);
    assert.equal(version.agent, process.env.EXPECTED_VERSION);
    assert(version.features.includes('tcp'), 'image agent lacks tcp feature');
    const app = http.createServer((_, response) => response.end('tcp-smoke-ok'));
    app.listen(0, '127.0.0.1', () => {
      const req = http.request({socketPath, path: `/v1/tcp?port=${app.address().port}`, headers: {Connection: 'Upgrade', Upgrade: 'boxpool-tcp'}});
      req.on('response', response => { throw new Error(`upgrade failed: ${response.statusCode}`); });
      req.on('error', error => { throw error; });
      req.on('upgrade', (response, socket, head) => {
        assert.equal(response.statusCode, 101);
        let data = head.toString();
        socket.on('data', b => data += b);
        socket.on('error', error => { throw error; });
        socket.on('end', () => {
          assert(data.includes('tcp-smoke-ok'), data);
          app.close();
          clearTimeout(timeout);
          console.log('OCI agent tcp feature and loopback HTTP forwarding: OK');
        });
        socket.write('GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n');
      });
      req.end();
    });
  });
}).on('error', error => { throw error; });
JS
docker exec "$cid" sh -ec '
    test "$(cat /proc/1/comm)" = tini
    test -r /proc/net/tcp
    test -c /dev/ptmx
    test "$(stat -c %a /tmp)" = 1777
    test "$(stat -c %a /var/tmp)" = 1777
    sh -c '\''sleep 0.2 </dev/null >/dev/null 2>&1 & echo $! > /tmp/orphan.pid'\''
    sleep 1
    test ! -e "/proc/$(cat /tmp/orphan.pid)"
'
docker stop --timeout 5 "$cid" >/dev/null
[[ $(docker inspect -f '{{.State.ExitCode}}' "$cid") == 0 ]]
docker logs "$cid" 2>&1 | grep -F 'agent stopped'
echo 'OCI version, agent CLIs, unprivileged unix socket, orphan reaping and SIGTERM forwarding: OK'
