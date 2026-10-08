$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString('N'))
$assets = Join-Path $tmp 'assets'; $dest = Join-Path $tmp 'bin'
New-Item -ItemType Directory -Path $assets | Out-Null
$asset = 'codegraph-v2.0.0-windows_amd64.exe'
$payload = Join-Path $assets $asset
[IO.File]::WriteAllText($payload, 'fixture, not an executable')
(Get-FileHash $payload -Algorithm SHA256).Hash | Set-Content "$payload.sha256" -NoNewline
function Invoke-WebRequest([string]$Uri, [string]$OutFile) {
  Copy-Item (Join-Path $assets ([IO.Path]::GetFileName($Uri))) $OutFile
}
try {
  $env:PROCESSOR_ARCHITECTURE = 'AMD64'; $env:CODEGRAPH_RELEASE_BASE_URL = 'https://fixture'
  $env:CODEGRAPH_INSTALL_DIR = $dest
  & (Join-Path $root 'install.ps1') -Version v2.0.0 | Out-Null
  if ((Get-Content (Join-Path $dest 'codegraph.exe') -Raw) -ne 'fixture, not an executable') { throw 'verified fixture was not installed' }
  Add-Content $payload 'tampered'
  try { & (Join-Path $root 'install.ps1') -Version v2.0.0 | Out-Null; throw 'tampered asset unexpectedly installed' }
  catch { if ($_.Exception.Message -notlike '*SHA-256 mismatch*') { throw } }
  if ((Get-Content (Join-Path $dest 'codegraph.exe') -Raw) -ne 'fixture, not an executable') { throw 'failed update replaced existing executable' }
  'PowerShell installer tests passed'
} finally { Remove-Item -Recurse -Force $tmp }
