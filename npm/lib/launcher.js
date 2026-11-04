'use strict';

const { createHash, randomUUID } = require('node:crypto');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const release = require('../release.json');

function assetName(platform, arch) {
  if (!['linux-x64', 'linux-arm64', 'darwin-arm64'].includes(`${platform}-${arch}`)) {
    throw new Error('supported hosts are Linux amd64/arm64 and Apple silicon macOS');
  }
  return `boxpool-${platform}-${arch === 'x64' ? 'amd64' : arch}`;
}

async function sha256(file) {
  const hash = createHash('sha256');
  const handle = await fs.open(file, 'r');
  try {
    for await (const chunk of handle.createReadStream({ autoClose: false })) hash.update(chunk);
  } finally {
    await handle.close();
  }
  return hash.digest('hex');
}

// Options provide isolated caches and fixture downloads to the tests; the CLI
// always uses the package's immutable release metadata and GitHub HTTPS URL.
async function getBinary({ platform = process.platform, arch = process.arch,
  cacheRoot = process.env.BOXPOOL_CACHE_DIR || path.join(process.env.XDG_CACHE_HOME || path.join(os.homedir(), '.cache'), 'boxpool'),
  metadata = release, fetchImpl = fetch } = {}) {
  const asset = assetName(platform, arch);
  const expected = metadata.checksums[asset];
  if (!/^v\d+\.\d+\.\d+(-[a-zA-Z0-9_.-]+)?$/.test(metadata.version) || !/^[a-f0-9]{64}$/.test(expected || '')) {
    throw new Error('this package has no release checksums; install a published boxpool package');
  }
  const directory = path.join(cacheRoot, metadata.version, `${platform}-${arch}`);
  const binary = path.join(directory, 'boxpool');
  try {
    if (await sha256(binary) === expected) return binary;
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  await fs.mkdir(directory, { recursive: true, mode: 0o700 });
  const temporary = path.join(directory, `.download-${randomUUID()}`);
  try {
    const response = await fetchImpl(`https://github.com/Sukhendu2002/boxpool/releases/download/${metadata.version}/${asset}`,
      { signal: AbortSignal.timeout(180000) });
    if (!response.ok || !response.body) throw new Error(`download failed: HTTP ${response.status}`);
    if (response.url && new URL(response.url).protocol !== 'https:') throw new Error('download redirected away from HTTPS');
    const file = await fs.open(temporary, 'wx', 0o600);
    try {
      let size = 0;
      for await (const chunk of response.body) {
        size += chunk.length;
        if (size > 128 * 1024 * 1024) throw new Error('download exceeds 128 MiB');
        await file.writeFile(chunk);
      }
    } finally {
      await file.close();
    }
    if (await sha256(temporary) !== expected) throw new Error('checksum mismatch; binary was not installed or executed');
    await fs.chmod(temporary, 0o755);
    await fs.rename(temporary, binary);
    return binary;
  } finally {
    await fs.rm(temporary, { force: true });
  }
}

module.exports = { assetName, getBinary };
