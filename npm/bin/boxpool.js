#!/usr/bin/env node
'use strict';

const { spawn } = require('node:child_process');
const os = require('node:os');
const { getBinary } = require('../lib/launcher');

async function main() {
  const binary = await getBinary();
  const child = spawn(binary, process.argv.slice(2), { stdio: 'inherit' });
  const forward = signal => child.kill(signal);
  const signals = ['SIGINT', 'SIGTERM', 'SIGHUP'];
  const handlers = signals.map(signal => () => forward(signal));
  signals.forEach((signal, i) => process.on(signal, handlers[i]));
  child.once('error', error => { console.error(`boxpool: ${error.message}`); process.exitCode = 1; });
  child.once('exit', (code, signal) => {
    signals.forEach((s, i) => process.removeListener(s, handlers[i]));
    process.exitCode = code === null ? 128 + (os.constants.signals[signal] || 1) : code;
  });
}

main().catch(error => { console.error(`boxpool: ${error.message}`); process.exitCode = 1; });
