param([string]$Version = $env:CODEGRAPH_VERSION, [string]$InstallDir = $env:CODEGRAPH_INSTALL_DIR)
$ErrorActionPreference = 'Stop'
if (!$Version) {
  $release = Invoke-RestMethod 'https://api.github.com/repos/isink17/codegraph/releases/latest'
  $Version = $release.tag_name
}
if ($Version -notmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') { throw "Invalid version: $Version (expected vX.Y.Z)" }
if (!$InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\CodeGraph' }
$arch = switch ($env:PROCESSOR_ARCHITECTURE) { 'AMD64' { 'windows_amd64' } 'ARM64' { 'windows_arm64' } default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" } }
$base = if ($env:CODEGRAPH_RELEASE_BASE_URL) { $env:CODEGRAPH_RELEASE_BASE_URL.TrimEnd('/') } else { 'https://github.com/isink17/codegraph/releases/download' }
$asset = "codegraph-$Version-$arch.exe"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Invoke-WebRequest "$base/$Version/$asset" -OutFile (Join-Path $tmp 'codegraph.exe')
  Invoke-WebRequest "$base/$Version/$asset.sha256" -OutFile (Join-Path $tmp 'checksum')
  $expected = ((Get-Content (Join-Path $tmp 'checksum') -Raw).Trim() -split '\s+')[0]
  if ($expected -notmatch '^[0-9a-fA-F]{64}$') { throw 'Invalid SHA-256 sidecar' }
  $actual = (Get-FileHash (Join-Path $tmp 'codegraph.exe') -Algorithm SHA256).Hash
  if ($actual -ne $expected) { throw 'SHA-256 mismatch; refusing installation' }
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  $destination = Join-Path $InstallDir 'codegraph.exe'
  $staged = Join-Path $InstallDir ('.codegraph-' + [guid]::NewGuid().ToString('N') + '.exe')
  Copy-Item (Join-Path $tmp 'codegraph.exe') $staged
  try { Move-Item -Force $staged $destination } finally { if (Test-Path $staged) { Remove-Item $staged } }
  Write-Output "Installed codegraph $Version at $destination"
  if (($env:PATH -split ';') -notcontains $InstallDir) { Write-Output "Add $InstallDir to PATH to run codegraph." }
} finally { Remove-Item -Recurse -Force $tmp }
