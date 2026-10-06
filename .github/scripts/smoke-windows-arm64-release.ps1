$ErrorActionPreference = 'Stop'
$binary = Join-Path $PWD 'dist/codegraph_v2.0.0_windows_arm64/codegraph.exe'
if (!(Test-Path $binary)) { throw "Missing release binary: $binary" }
$readobj = Get-ChildItem (Join-Path $env:RUNNER_TEMP 'llvm-mingw') -Recurse -Filter llvm-readobj.exe | Select-Object -First 1 -ExpandProperty FullName
if (!$readobj) { throw 'llvm-readobj missing from pinned toolchain.' }
$headers = & $readobj --file-headers $binary
$headers
if (($headers -join "`n") -notmatch 'IMAGE_FILE_MACHINE_ARM64') { throw 'CodeGraph release executable is not ARM64 PE.' }
$version = (& $binary --version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $version -ne 'codegraph v2.0.0') { throw "Unexpected CodeGraph version: $version" }
& $binary doctor
if ($LASTEXITCODE -ne 0) { throw 'CodeGraph doctor failed.' }
$fixture = Join-Path $env:RUNNER_TEMP 'codegraph-ts-fixture'
New-Item -ItemType Directory -Force $fixture | Out-Null
'export function parsedSymbol(): number { return 42; }' | Set-Content (Join-Path $fixture 'sample.ts')
& $binary index $fixture
if ($LASTEXITCODE -ne 0) { throw 'CodeGraph TypeScript fixture index failed.' }
$stats = & $binary stats $fixture --json | Out-String
if ($LASTEXITCODE -ne 0 -or $stats -notmatch 'typescript') { throw "Tree-sitter capability smoke failed: $stats" }

$env:CODEGRAPH_NPM_TEST_MODE = '1'
$ready = Join-Path $env:RUNNER_TEMP 'release-fixture-ready'
$serverScript = Join-Path $env:RUNNER_TEMP 'release-fixture-server.js'
@'
const fs = require('node:fs'), http = require('node:http'), path = require('node:path');
const assets = process.argv[2], ready = path.join(process.env.RUNNER_TEMP, 'release-fixture-ready');
http.createServer((req, res) => {
  const name = path.basename(decodeURIComponent(new URL(req.url, 'http://localhost').pathname));
  const file = path.join(assets, name);
  if (!fs.existsSync(file)) { res.writeHead(404).end(); return; }
  res.writeHead(200).end(fs.readFileSync(file));
}).listen(18743, '127.0.0.1', () => fs.writeFileSync(ready, 'ready'));
'@ | Set-Content -Encoding utf8 $serverScript
$server = Start-Process -FilePath node -ArgumentList @($serverScript, (Join-Path $PWD 'dist')) -PassThru -NoNewWindow
try {
  for ($i = 0; $i -lt 100 -and !(Test-Path $ready); $i++) {
    if ($server.HasExited) { throw "Asset fixture server exited with code $($server.ExitCode)." }
    Start-Sleep -Milliseconds 100
  }
  if (!(Test-Path $ready)) { throw 'Asset fixture server did not become ready.' }
  $env:CODEGRAPH_NPM_TEST_RELEASE_BASE_URL = 'http://localhost:18743'
  Push-Location npm
  try {
    & npm version 2.0.0 --no-git-tag-version --ignore-scripts
    if ($LASTEXITCODE -ne 0) { throw 'Could not stage npm package version 2.0.0.' }
    $tarball = (& npm pack --silent | Out-String).Trim()
  } finally { Pop-Location }
  $prefix = Join-Path $env:RUNNER_TEMP 'npm global prefix with spaces'
  & npm install --global --prefix $prefix (Join-Path $PWD "npm/$tarball")
  if ($LASTEXITCODE -ne 0) { throw 'npm global tarball installation failed.' }
  $installed = Join-Path $prefix 'codegraph.cmd'
  $installedVersion = (& $installed --version | Out-String).Trim()
  if ($LASTEXITCODE -ne 0 -or $installedVersion -ne 'codegraph v2.0.0') { throw "Installed npm command failed: $installedVersion" }
  & $installed doctor
  if ($LASTEXITCODE -ne 0) { throw 'Installed npm doctor failed.' }
} finally {
  Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
}
Write-Host 'Windows ARM64 PE, version, doctor, TypeScript cgo parsing, and npm checksum install passed.'
