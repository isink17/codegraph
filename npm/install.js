'use strict';

const { install } = require('./lib/installer');

install({ packageDir: __dirname, version: require('./package.json').version }).catch(error => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});
