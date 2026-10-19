# Usage: install.ps1 [-Version vX.Y.Z] [-InstallDir DIR]   install or upgrade codegraph
#        install.ps1 -Uninstall [-InstallDir DIR]           remove codegraph.exe from the install directory
# Environment: CODEGRAPH_VERSION, CODEGRAPH_INSTALL_DIR, CODEGRAPH_RELEASE_BASE_URL
param([string]$Version = $env:CODEGRAPH_VERSION, [string]$InstallDir = $env:CODEGRAPH_INSTALL_DIR, [switch]$Uninstall)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Windows PowerShell 5.1 may default to TLS 1.0/1.1, which GitHub rejects.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
if (!$InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\CodeGraph' }
$destination = Join-Path $InstallDir 'codegraph.exe'

# A running codegraph.exe cannot be deleted, only renamed, so an upgrade moves it aside first.
function Remove-StaleAside {
  Get-ChildItem -LiteralPath $InstallDir -Filter 'codegraph.exe.old-*' -Force -ErrorAction SilentlyContinue |
    ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
}

function Get-ReleaseFile([string]$uri, [string]$outFile) {
  try {
    if ($uri -like 'file://*') { Copy-Item -LiteralPath ([Uri]$uri).LocalPath -Destination $outFile }
    else { Invoke-WebRequest $uri -OutFile $outFile -UseBasicParsing }
  } catch { throw "Download failed: $uri ($($_.Exception.Message))" }
}

if (Test-Path -LiteralPath $destination -PathType Container) { throw "$destination is a directory, not a codegraph binary; remove or rename it first" }

if ($Uninstall) {
  if (Test-Path -LiteralPath $destination -PathType Leaf) {
    try { Remove-Item -LiteralPath $destination -Force } catch { throw "Could not remove ${destination}: stop running codegraph processes (MCP clients) and retry. $($_.Exception.Message)" }
    Write-Output "Removed $destination"
  } else {
    Write-Output "codegraph is not installed at $destination"
  }
  Remove-StaleAside
  Write-Output 'Repository indexes (.codegraph directories), user configuration and MCP client entries were left in place.'
  return
}

# OSArchitecture reports ARM64 even when this PowerShell runs as x64 under emulation. .NET before
# 4.7.1 lacks RuntimeInformation; PROCESSOR_ARCHITEW6432 then names the OS arch under WOW64.
$runtimeInfo = 'System.Runtime.InteropServices.RuntimeInformation' -as [type]
$osArch = if ($env:CODEGRAPH_INSTALL_TEST_ARCH) {
  $env:CODEGRAPH_INSTALL_TEST_ARCH # test-only override, used by .github/scripts/install-test.ps1
} elseif ($runtimeInfo) {
  [string]($runtimeInfo::OSArchitecture)
} else {
  $nativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
  switch ($nativeArch) { 'AMD64' { 'X64' } 'ARM64' { 'Arm64' } default { $nativeArch } }
}
$target = switch ($osArch) {
  'X64' { 'windows_amd64' }
  'Arm64' { 'windows_arm64' }
  default { throw "Unsupported architecture: $osArch (supported: X64, Arm64)" }
}

if (!$Version) {
  $release = Invoke-RestMethod 'https://api.github.com/repos/isink17/codegraph/releases/latest'
  $Version = $release.tag_name
}
if ($Version -notmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') { throw "Invalid version: $Version (expected vX.Y.Z)" }
$base = if ($env:CODEGRAPH_RELEASE_BASE_URL) { $env:CODEGRAPH_RELEASE_BASE_URL.TrimEnd('/') } else { 'https://github.com/isink17/codegraph/releases/download' }
$asset = "codegraph-$Version-$target.exe"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Remove-StaleAside
$token = [guid]::NewGuid().ToString('N')
$staged = Join-Path $InstallDir ".codegraph-$token.exe"
$sidecar = "$staged.sha256"
try {
  Get-ReleaseFile "$base/$Version/$asset" $staged
  Get-ReleaseFile "$base/$Version/$asset.sha256" $sidecar
  $expected = (((Get-Content -LiteralPath $sidecar -Raw) + '').Trim() -split '\s+')[0]
  if ($expected -notmatch '^[0-9a-fA-F]{64}$') { throw 'Invalid SHA-256 sidecar' }
  if ((Get-FileHash -LiteralPath $staged -Algorithm SHA256).Hash -ne $expected) { throw 'SHA-256 mismatch; refusing installation' }

  $reported = ''
  try { $reported = ((& $staged --version 2>$null) | Out-String).Trim() } catch { }
  if ($reported -ne "codegraph $Version") {
    throw "Version mismatch: downloaded binary reports '$reported', expected 'codegraph $Version'; refusing installation"
  }

  $aside = $null
  if (Test-Path -LiteralPath $destination) {
    $aside = "$destination.old-$token"
    Move-Item -LiteralPath $destination -Destination $aside
  }
  try {
    Move-Item -LiteralPath $staged -Destination $destination
  } catch {
    if ($aside) { Move-Item -LiteralPath $aside -Destination $destination }
    throw "Could not install ${destination}; the previous installation was left in place. $($_.Exception.Message)"
  }
  if ($aside) { Remove-Item -LiteralPath $aside -Force -ErrorAction SilentlyContinue }
} finally {
  Remove-Item -LiteralPath $staged, $sidecar -Force -ErrorAction SilentlyContinue
}

Write-Output "Installed codegraph $Version at $destination"
$entries = $env:PATH -split [IO.Path]::PathSeparator | ForEach-Object { $_.TrimEnd('\', '/') }
if ($entries -notcontains $InstallDir.TrimEnd('\', '/')) { Write-Output "Add $InstallDir to PATH to run codegraph." }
$first = Get-Command codegraph.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
if ($first -and ([IO.Path]::GetFullPath($first.Source) -ne [IO.Path]::GetFullPath($destination))) {
  Write-Warning "'codegraph' on PATH resolves to $($first.Source), not $destination. Remove the other copy or put $InstallDir first on PATH."
}
