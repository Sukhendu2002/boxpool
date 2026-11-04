'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { spawnSync, spawn } = require('node:child_process');
const { once } = require('node:events');
const { assetName, getBinary } = require('../lib/launcher');

async function fixture(t, content = '#!/bin/sh\nprintf "%s\\n" "$@"\nexit 37\n') {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'boxpool-npm-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const digest = createHash('sha256').update(content).digest('hex');
  const metadata = { version: 'v1.2.3', checksums: {
    'boxpool-linux-amd64': digest, 'boxpool-linux-arm64': digest, 'boxpool-darwin-arm64': digest,
  } };
  const urls = [];
  const options = { cacheRoot: path.join(root, 'cache'), metadata,
    fetchImpl: async url => { urls.push(url); return new Response(content); } };
  return { root, options, urls, content };
}

test('selects only supported assets', () => {
  assert.equal(assetName('linux', 'x64'), 'boxpool-linux-amd64');
  assert.equal(assetName('linux', 'arm64'), 'boxpool-linux-arm64');
  assert.equal(assetName('darwin', 'arm64'), 'boxpool-darwin-arm64');
  assert.throws(() => assetName('darwin', 'x64'), /supported hosts/);
  assert.throws(() => assetName('win32', 'x64'), /supported hosts/);
});

test('downloads the pinned release and reuses a verified cache offline', async t => {
  const { options, urls, content } = await fixture(t);
  const binary = await getBinary(options);
  assert.equal(await fs.readFile(binary, 'utf8'), content);
  assert.equal((await fs.stat(binary)).mode & 0o777, 0o755);
  assert.equal(urls.length, 1);
  assert.match(urls[0], /\/download\/v1\.2\.3\/boxpool-/);
  assert.equal(await getBinary({ ...options, fetchImpl: () => { throw Error('offline'); } }), binary);
});

test('rejects a tampered download and leaves no executable or partial file', async t => {
  const { options } = await fixture(t);
  await assert.rejects(getBinary({ ...options, fetchImpl: async () => new Response('tampered') }), /checksum mismatch/);
  const directory = path.join(options.cacheRoot, options.metadata.version, `${process.platform}-${process.arch}`);
  assert.deepEqual(await fs.readdir(directory), []);
});

test('detects corrupt cache and repairs it from a verified download', async t => {
  const { options, urls, content } = await fixture(t);
  const binary = await getBinary(options);
  await fs.writeFile(binary, 'tampered');
  await assert.rejects(getBinary({ ...options, fetchImpl: async () => new Response('also tampered') }), /checksum mismatch/);
  await getBinary(options);
  assert.equal(await fs.readFile(binary, 'utf8'), content);
  assert.equal(urls.length, 2);
});

test('cleans up failed HTTP and interrupted downloads', async t => {
  const { options } = await fixture(t);
  await assert.rejects(getBinary({ ...options, fetchImpl: async () => new Response('', { status: 404 }) }), /HTTP 404/);
  await assert.rejects(getBinary({ ...options, fetchImpl: async () => ({ ok: true, body: (async function* () {
    yield Buffer.from('partial'); throw Error('connection lost');
  })() }) }), /connection lost/);
  const directory = path.join(options.cacheRoot, options.metadata.version, `${process.platform}-${process.arch}`);
  assert.deepEqual(await fs.readdir(directory), []);
});

test('concurrent first runs publish complete verified binaries', async t => {
  const { options, content } = await fixture(t);
  const binaries = await Promise.all([getBinary(options), getBinary(options), getBinary(options)]);
  assert.equal(new Set(binaries).size, 1);
  assert.equal(await fs.readFile(binaries[0], 'utf8'), content);
  assert.deepEqual(await fs.readdir(path.dirname(binaries[0])), ['boxpool']);
});

test('rejects an unpublished source package before fetching', async () => {
  await assert.rejects(getBinary({ metadata: { version: '0.0.0-dev', checksums: {} }, fetchImpl: () => assert.fail('unexpected fetch') }), /no release checksums/);
});

async function stageLauncher(root, metadata) {
  const packageRoot = path.join(root, 'package');
  await fs.mkdir(packageRoot);
  for (const directory of ['bin', 'lib']) await fs.cp(path.join(__dirname, '..', directory), path.join(packageRoot, directory), { recursive: true });
  await fs.writeFile(path.join(packageRoot, 'release.json'), JSON.stringify(metadata));
  return path.join(packageRoot, 'bin', 'boxpool.js');
}

test('launcher preserves arguments, stdio and exit code', async t => {
  const { root, options } = await fixture(t, '#!/bin/sh\nprintf "%s\\n" "$@"\ncat\nprintf error >&2\nexit 37\n');
  await getBinary(options);
  const launcher = await stageLauncher(root, options.metadata);
  const result = spawnSync(process.execPath, [launcher, 'exec', 'name with spaces', '--', '-x'], {
    input: 'stdin fixture\n', encoding: 'utf8', env: { ...process.env, BOXPOOL_CACHE_DIR: options.cacheRoot },
  });
  assert.equal(result.status, 37, result.stderr);
  assert.equal(result.stdout, 'exec\nname with spaces\n--\n-x\nstdin fixture\n');
  assert.equal(result.stderr, 'error');
});

test('launcher forwards termination signals to the child', async t => {
  const { root, options } = await fixture(t, '#!/bin/sh\ntrap "exit 42" TERM\necho ready\nwhile :; do sleep 0.1; done\n');
  await getBinary(options);
  const launcher = await stageLauncher(root, options.metadata);
  const child = spawn(process.execPath, [launcher], { env: { ...process.env, BOXPOOL_CACHE_DIR: options.cacheRoot } });
  t.after(() => child.kill('SIGKILL'));
  await once(child.stdout, 'data');
  const exited = once(child, 'exit');
  child.kill('SIGTERM');
  const [code, signal] = await exited;
  assert.equal(code, 42);
  assert.equal(signal, null);
});
