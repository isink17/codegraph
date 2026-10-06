'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { after, test } = require('node:test');
const { promisify } = require('node:util');
const execFile = promisify(require('node:child_process').execFile);
const { assetFor, install, PLATFORMS } = require('../lib/installer');

const version = require('../package.json').version;
const fixture = process.env.CODEGRAPH_TEST_BINARY || path.join(__dirname, 'native-fixture');
const binaryBytes = fs.readFileSync(fixture);
let mode = 'ok';
let requests = [];
const server = http.createServer((request, response) => {
  requests.push(request.url);
  if (mode === 'redirect' && !request.url.startsWith('/redirected/')) {
    response.writeHead(302, { location: `/redirected${request.url}` }).end();
    return;
  }
  const requestPath = request.url.replace(/^\/redirected/, '');
  if (mode === 'http-error') return response.writeHead(503).end();
  if (mode === 'missing') return response.writeHead(404).end();
  if (requestPath.endsWith('.sha256')) {
    if (mode === 'invalid-sidecar') return response.end('not-a-sha256\n');
    const bytes = mode === 'bad-checksum' ? Buffer.alloc(binaryBytes.length, 1) : binaryBytes;
    const sum = crypto.createHash('sha256').update(bytes).digest('hex');
    return response.end(`${sum}  fixture\n`);
  }
  if (mode === 'truncated') return response.end(binaryBytes.subarray(0, Math.max(1, binaryBytes.length - 1)));
  if (mode === 'redirect') return response.end(binaryBytes);
  response.end(binaryBytes);
});

const listen = new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const base = async () => {
  await listen;
  return `http://127.0.0.1:${server.address().port}`;
};
after(() => server.close());

function tempPackage() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'codegraph npm fixture '));
}

async function installFixture(dir) {
  const url = await base();
  await install({ packageDir: dir, version, env: {
    ...process.env,
    CODEGRAPH_NPM_TEST_MODE: '1',
    CODEGRAPH_NPM_TEST_RELEASE_BASE_URL: url,
  } });
}

test('maps only the six supported native targets', () => {
  assert.deepEqual(Object.keys(PLATFORMS), ['darwin', 'linux', 'win32']);
  assert.equal(assetFor('darwin', 'x64', version), `codegraph-v${version}-darwin_amd64`);
  assert.equal(assetFor('darwin', 'arm64', version), `codegraph-v${version}-darwin_arm64`);
  assert.equal(assetFor('linux', 'x64', version), `codegraph-v${version}-linux_amd64`);
  assert.equal(assetFor('linux', 'arm64', version), `codegraph-v${version}-linux_arm64`);
  assert.equal(assetFor('win32', 'x64', version), `codegraph-v${version}-windows_amd64.exe`);
  assert.equal(assetFor('win32', 'arm64', version), `codegraph-v${version}-windows_arm64.exe`);
  assert.throws(() => assetFor('freebsd', 'x64', version), /does not support freebsd\/x64/);
  assert.throws(() => assetFor('linux', 'ia32', version), /does not support linux\/ia32/);
});

test('packs, globally installs and invokes the tarball wrapper', async t => {
  const work = tempPackage();
  t.after(() => fs.rmSync(work, { recursive: true, force: true }));
  const pack = await execFile('npm', ['pack', '--json', '--pack-destination', work], { cwd: path.resolve(__dirname, '..'), encoding: 'utf8' });
  const metadata = JSON.parse(pack.stdout)[0];
  const files = metadata.files.map(file => file.path);
  assert(files.includes('bin/codegraph.js'));
  assert(files.includes('lib/installer.js'));
  assert(!files.some(file => /(^|\/)(\.git|node_modules|\.npm|evidence|dist)(\/|$)/i.test(file)));
  assert(metadata.size < 100_000, `unexpected tarball size: ${metadata.size}`);
  const archive = path.join(work, metadata.filename);
  const prefix = path.join(work, 'global prefix with spaces');
  await execFile('npm', ['install', '--global', '--prefix', prefix, archive], {
    cwd: work,
    encoding: 'utf8',
    env: { ...process.env, CODEGRAPH_NPM_TEST_MODE: '1', CODEGRAPH_NPM_TEST_RELEASE_BASE_URL: await base() },
  });
  assert(requests.some(url => url.includes(`/v${version}/codegraph-v${version}-`)));

  const moduleDir = path.join(prefix, ...(process.platform === 'win32' ? ['node_modules'] : ['lib', 'node_modules']), '@isink17', 'codegraph');
  const native = path.join(moduleDir, 'bin', 'native', process.platform === 'win32' ? 'codegraph.exe' : 'codegraph');
  assert.deepEqual(fs.readFileSync(native), binaryBytes);
  const command = process.platform === 'win32' ? path.join(prefix, 'codegraph.cmd') : path.join(prefix, 'bin', 'codegraph');
  const invoke = (args, options = {}) => process.platform === 'win32'
    ? spawnSync(command, args, { shell: true, encoding: 'utf8', ...options })
    : spawnSync(command, args, { encoding: 'utf8', ...options });
  const versionResult = invoke(['--version']);
  assert.equal(versionResult.status, 0, versionResult.stderr);
  assert.equal(versionResult.stdout.trim(), `codegraph ${version}`);
  const argsResult = invoke(['args', 'space value', 'semi;colon']);
  assert.equal(argsResult.status, 0, argsResult.stderr);
  assert.match(argsResult.stdout, /space value/);
  assert.match(argsResult.stdout, /semi;colon/);
  const stdinResult = invoke(['stdin'], { input: 'stdio remains transparent\n' });
  assert.equal(stdinResult.stdout, 'stdio remains transparent\n');
  const failure = invoke(['fail']);
  assert.equal(failure.status, 23);

  const ignoredPrefix = path.join(work, 'ignore scripts prefix');
  await execFile('npm', ['install', '--global', '--ignore-scripts', '--prefix', ignoredPrefix, archive], { cwd: work, encoding: 'utf8' });
  const ignoredCommand = process.platform === 'win32' ? path.join(ignoredPrefix, 'codegraph.cmd') : path.join(ignoredPrefix, 'bin', 'codegraph');
  const ignored = process.platform === 'win32'
    ? spawnSync(ignoredCommand, ['--version'], { shell: true, encoding: 'utf8' })
    : spawnSync(ignoredCommand, ['--version'], { encoding: 'utf8' });
  assert.notEqual(ignored.status, 0);
  assert.match(ignored.stderr, /Reinstall with npm scripts enabled/);

});

test('fails closed and cleans partial native binaries on every download failure', async t => {
  const dir = tempPackage();
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const root = path.join(dir, 'bin', 'native');
  fs.mkdirSync(root, { recursive: true });
  const native = path.join(root, process.platform === 'win32' ? 'codegraph.exe' : 'codegraph');

  for (const [failureMode, message] of [
    ['missing', /HTTP 404/],
    ['http-error', /HTTP 503/],
    ['bad-checksum', /SHA-256 mismatch/],
    ['invalid-sidecar', /invalid SHA-256 format/],
    ['truncated', /SHA-256 mismatch/],
  ]) {
    mode = failureMode;
    fs.writeFileSync(native, 'partial executable');
    await assert.rejects(installFixture(dir), error => {
      assert.match(error.message, message);
      assert.match(error.message, new RegExp(`CodeGraph ${version}`));
      return true;
    });
    assert.equal(fs.existsSync(native), false);
    assert.deepEqual(fs.readdirSync(root), []);
  }

  mode = 'redirect';
  await installFixture(dir);
  assert.deepEqual(fs.readFileSync(native), binaryBytes);
  mode = 'ok';
  await installFixture(dir);
  assert.deepEqual(fs.readFileSync(native), binaryBytes);
});

test('restricts test URL override to explicit localhost mode', async () => {
  const dir = tempPackage();
  await assert.rejects(install({ packageDir: dir, version, env: { CODEGRAPH_NPM_TEST_RELEASE_BASE_URL: 'http://127.0.0.1:9' } }), /requires CODEGRAPH_NPM_TEST_MODE/);
  await assert.rejects(install({ packageDir: dir, version, env: { CODEGRAPH_NPM_TEST_MODE: '1', CODEGRAPH_NPM_TEST_RELEASE_BASE_URL: 'http://example.com' } }), /must use HTTP on localhost/);
  await assert.rejects(install({ packageDir: dir, version: 'v2; touch nope', env: {} }), /Invalid CodeGraph package version/);
  fs.rmSync(dir, { recursive: true, force: true });
});
