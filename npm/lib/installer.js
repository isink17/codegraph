'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const path = require('node:path');
const { pipeline } = require('node:stream/promises');

const RELEASES = 'https://github.com/isink17/codegraph/releases/download';
const PLATFORMS = Object.freeze({
  darwin: Object.freeze({ x64: 'darwin_amd64', arm64: 'darwin_arm64' }),
  linux: Object.freeze({ x64: 'linux_amd64', arm64: 'linux_arm64' }),
  win32: Object.freeze({ x64: 'windows_amd64', arm64: 'windows_arm64' }),
});

function assetFor(platform, arch, version) {
  const target = PLATFORMS[platform]?.[arch];
  if (!target) throw new Error(`CodeGraph ${version} does not support ${platform}/${arch}.`);
  return `codegraph-v${version}-${target}${platform === 'win32' ? '.exe' : ''}`;
}

function releaseBase(env) {
  const override = env.CODEGRAPH_NPM_TEST_RELEASE_BASE_URL;
  if (!override) return RELEASES;
  if (env.CODEGRAPH_NPM_TEST_MODE !== '1') throw new Error('Test release URL requires CODEGRAPH_NPM_TEST_MODE=1.');
  const url = new URL(override);
  if (!['localhost', '127.0.0.1', '[::1]'].includes(url.hostname) || url.protocol !== 'http:') {
    throw new Error('Test release URL must use HTTP on localhost.');
  }
  return url.toString().replace(/\/$/, '');
}

function get(url, redirects = 0) {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    if (!['https:', 'http:'].includes(parsed.protocol)) return reject(new Error('Unsupported download protocol.'));
    const transport = parsed.protocol === 'https:' ? https : http;
    const request = transport.get(parsed, { headers: { 'User-Agent': '@isink17/codegraph installer' } }, response => {
      if ([301, 302, 303, 307, 308].includes(response.statusCode)) {
        response.resume();
        if (redirects >= 5 || !response.headers.location) return reject(new Error('Too many or invalid download redirects.'));
        const next = new URL(response.headers.location, parsed);
        if (parsed.protocol === 'https:' && next.protocol !== 'https:') return reject(new Error('Refusing insecure download redirect.'));
        return resolve(get(next.toString(), redirects + 1));
      }
      if (response.statusCode !== 200) {
        response.resume();
        return reject(new Error(`Download failed with HTTP ${response.statusCode}.`));
      }
      resolve(response);
    });
    request.setTimeout(120_000, () => request.destroy(new Error('Download timed out.')));
    request.on('error', reject);
  });
}

async function download(url, destination) {
  await pipeline(await get(url), fs.createWriteStream(destination, { flags: 'wx', mode: 0o600 }));
}

async function fetchChecksum(url) {
  const response = await get(url);
  let content = '';
  for await (const chunk of response) {
    content += chunk.toString('utf8');
    if (content.length > 4096) throw new Error('Checksum sidecar is too large.');
  }
  const match = content.trim().match(/^([a-fA-F0-9]{64})(?:[ \t]+(?:\*?[^\r\n]+))?$/);
  if (!match) throw new Error('Checksum sidecar has invalid SHA-256 format.');
  return match[1].toLowerCase();
}

async function install({ packageDir, version, platform = process.platform, arch = process.arch, env = process.env }) {
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error(`Invalid CodeGraph package version: ${version}.`);
  }
  const asset = assetFor(platform, arch, version);
  const base = releaseBase(env);
  const tag = `v${version}`;
  const root = path.join(packageDir, 'bin', 'native');
  const binary = path.join(root, platform === 'win32' ? 'codegraph.exe' : 'codegraph');
  const temporary = `${binary}.${process.pid}.${crypto.randomBytes(6).toString('hex')}.tmp`;
  const release = `${base}/${encodeURIComponent(tag)}/${encodeURIComponent(asset)}`;

  fs.mkdirSync(root, { recursive: true });
  // A pre-existing partial binary must never survive a failed reinstall as an apparently usable command.
  fs.rmSync(binary, { force: true });
  try {
    const expected = await fetchChecksum(`${release}.sha256`);
    await download(release, temporary);
    const actual = crypto.createHash('sha256').update(fs.readFileSync(temporary)).digest('hex');
    if (actual !== expected) throw new Error(`SHA-256 mismatch for ${asset}.`);
    fs.chmodSync(temporary, 0o755);
    fs.renameSync(temporary, binary);
  } catch (error) {
    fs.rmSync(temporary, { force: true });
    throw new Error(`Unable to install CodeGraph ${version} for ${platform}/${arch} from ${tag} (${asset}): ${error.message}`);
  }
}

module.exports = { PLATFORMS, assetFor, install };
