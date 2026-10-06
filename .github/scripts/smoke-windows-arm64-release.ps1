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
$listener = [System.Net.HttpListener]::new()
$listener.Prefixes.Add('http://localhost:18743/')
$listener.Start()
$serve = Start-Job -ArgumentList $listener, (Join-Path $PWD 'dist') -ScriptBlock {
  param($http, $assets)
  while ($http.IsListening) {
    try {
      $context = $http.GetContext()
      $name = [System.IO.Path]::GetFileName([Uri]::UnescapeDataString($context.Request.Url.AbsolutePath))
      $file = Join-Path $assets $name
      if (Test-Path $file) {
        $bytes = [IO.File]::ReadAllBytes($file); $context.Response.StatusCode = 200
        $context.Response.OutputStream.Write($bytes, 0, $bytes.Length)
      } else { $context.Response.StatusCode = 404 }
      $context.Response.Close()
    } catch { break }
  }
}
try {
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
  Stop-Job $serve -ErrorAction SilentlyContinue
  Remove-Job $serve -Force -ErrorAction SilentlyContinue
  $listener.Stop(); $listener.Close()
}
Write-Host 'Windows ARM64 PE, version, doctor, TypeScript cgo parsing, and npm checksum install passed.'
