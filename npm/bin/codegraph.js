#!/usr/bin/env node
'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');

const native = path.join(__dirname, 'native', process.platform === 'win32' ? 'codegraph.exe' : 'codegraph');
if (!fs.existsSync(native)) {
  process.stderr.write(`CodeGraph ${require('../package.json').version} native binary is missing. Reinstall with npm scripts enabled (do not use --ignore-scripts).\n`);
  process.exit(1);
}

const child = spawn(native, process.argv.slice(2), { stdio: 'inherit', windowsHide: true });
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(signal, () => child.kill(signal));
}
child.on('error', error => {
  process.stderr.write(`Unable to start CodeGraph native binary: ${error.message}\n`);
  process.exitCode = 1;
});
child.on('exit', (code, signal) => {
  process.exitCode = signal ? 128 + ({ SIGINT: 2, SIGTERM: 15, SIGHUP: 1 }[signal] || 1) : (code ?? 1);
});
