# Exercises install.ps1 against a synthetic release served over file:// (no network).
# Fixture binaries are built from Go so the version check runs a real executable.
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$installer = Join-Path $root 'install.ps1'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString('N'))
$rel = Join-Path $tmp 'rel'; $dest = Join-Path $tmp 'bin'; $fixtures = Join-Path $tmp 'fixtures'
New-Item -ItemType Directory -Path $rel, $fixtures | Out-Null
$exe = Join-Path $dest 'codegraph.exe'
$savedEnv = @{}
foreach ($name in 'CODEGRAPH_VERSION', 'CODEGRAPH_INSTALL_DIR', 'CODEGRAPH_RELEASE_BASE_URL', 'CODEGRAPH_INSTALL_TEST_ARCH', 'PATH') {
  $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name)
}

Set-Content (Join-Path $fixtures 'main.go') @'
package main

import (
	"fmt"
	"os"
	"time"
)

var version string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codegraph " + version)
	}
	if len(os.Args) > 1 && os.Args[1] == "sleep" {
		time.Sleep(time.Minute)
	}
}
'@
function Build-Fixture([string]$v) {
  $out = Join-Path $fixtures "$v.exe"
  Push-Location $fixtures
  try {
    $env:CGO_ENABLED = '0'; $env:GO111MODULE = 'off'
    go build -ldflags "-X main.version=$v" -o $out main.go
    if ($LASTEXITCODE -ne 0) { throw "fixture build failed for $v" }
  } finally { Pop-Location; Remove-Item Env:GO111MODULE, Env:CGO_ENABLED -ErrorAction SilentlyContinue }
  $out
}

# Publish VERSION TARGET FIXTURE: copies the fixture as the release asset plus its sidecar.
function Publish([string]$v, [string]$target, [string]$fixture) {
  $dir = Join-Path $rel $v
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $asset = Join-Path $dir "codegraph-$v-$target.exe"
  Copy-Item $fixture $asset
  "$((Get-FileHash $asset -Algorithm SHA256).Hash.ToLower())  codegraph-$v-$target.exe" | Set-Content "$asset.sha256" -NoNewline
  $asset
}

function Install([hashtable]$installerArgs) { (& $installer @installerArgs *>&1 | Out-String) }
function Expect-Failure([string]$message, [hashtable]$installerArgs) {
  try { & $installer @installerArgs *>&1 | Out-Null } catch {
    if ($_.Exception.Message -notlike "*$message*") { throw "expected '$message', got: $($_.Exception.Message)" }
    return
  }
  throw "installer unexpectedly succeeded: $($installerArgs.Values)"
}
function Assert-Version([string]$v) {
  $got = (& $exe --version | Out-String).Trim()
  if ($got -ne "codegraph $v") { throw "installed version is '$got', want $v" }
}
function Assert-NoLeftovers {
  $left = Get-ChildItem -Path $dest -Force | Where-Object { $_.Name -like '.codegraph-*' -or $_.Name -like 'codegraph.exe.old-*' }
  if ($left) { throw "leftover files in ${dest}: $($left.Name -join ', ')" }
}
function Assert-Kept([string]$case) {
  if ((Get-FileHash $exe).Hash -ne $script:keptHash) { throw "existing binary changed after: $case" }
  Assert-NoLeftovers
}

try {
  $v200 = Build-Fixture 'v2.0.0'; $v201 = Build-Fixture 'v2.0.1'
  $env:CODEGRAPH_RELEASE_BASE_URL = 'file:///' + ($rel -replace '\\', '/').TrimStart('/'); $env:CODEGRAPH_INSTALL_DIR = $dest
  $env:CODEGRAPH_INSTALL_TEST_ARCH = 'X64'
  Remove-Item Env:CODEGRAPH_VERSION -ErrorAction SilentlyContinue

  Publish v2.0.0 windows_amd64 $v200 | Out-Null
  $out = Install @{ Version = 'v2.0.0' }
  if ($out -notlike '*Installed codegraph v2.0.0*') { throw "missing install message: $out" }
  if ($out -notlike '*to PATH to run codegraph*') { throw "missing PATH hint: $out" }
  Assert-Version v2.0.0; Assert-NoLeftovers
  'ok fresh install'

  Publish v2.0.1 windows_amd64 $v201 | Out-Null
  New-Item -ItemType File -Path (Join-Path $dest 'codegraph.exe.old-stale') | Out-Null
  $env:CODEGRAPH_VERSION = 'v2.0.1'
  Install @{} | Out-Null
  Remove-Item Env:CODEGRAPH_VERSION
  Assert-Version v2.0.1; Assert-NoLeftovers
  'ok upgrade over existing install, stale aside removed'

  $script:keptHash = (Get-FileHash $exe).Hash
  Publish v2.0.2 windows_amd64 $v201 | Out-Null
  Expect-Failure 'Version mismatch' @{ Version = 'v2.0.2' }; Assert-Kept 'version mismatch'
  'ok version mismatch keeps old binary'

  $asset = Publish v2.0.3 windows_amd64 $v200
  Add-Content $asset 'tampered'
  Expect-Failure 'SHA-256 mismatch' @{ Version = 'v2.0.3' }; Assert-Kept 'tampered asset'
  'ok tampered asset'

  $asset = Publish v2.0.4 windows_amd64 $v200
  Set-Content "$asset.sha256" 'not-a-digest'
  Expect-Failure 'Invalid SHA-256 sidecar' @{ Version = 'v2.0.4' }; Assert-Kept 'invalid sidecar'
  Set-Content "$asset.sha256" '' -NoNewline
  Expect-Failure 'Invalid SHA-256 sidecar' @{ Version = 'v2.0.4' }; Assert-Kept 'empty sidecar'
  Remove-Item "$asset.sha256"
  Expect-Failure 'Download failed' @{ Version = 'v2.0.4' }; Assert-Kept 'missing sidecar'
  Expect-Failure 'Download failed' @{ Version = 'v9.9.9' }; Assert-Kept 'missing asset'
  Expect-Failure 'Invalid version' @{ Version = '2.0.0' }; Assert-Kept 'invalid version'
  'ok checksum, download and version failures'

  foreach ($arch in 'X86', 'Arm', 'S390x') {
    $env:CODEGRAPH_INSTALL_TEST_ARCH = $arch
    Expect-Failure "Unsupported architecture: $arch" @{ Version = 'v2.0.1' }; Assert-Kept "unsupported $arch"
  }
  'ok unsupported architectures'

  $env:CODEGRAPH_INSTALL_TEST_ARCH = 'Arm64'
  Publish v2.0.1 windows_arm64 $v200 | Out-Null
  Expect-Failure 'Version mismatch' @{ Version = 'v2.0.1' }
  Publish v2.0.1 windows_arm64 $v201 | Out-Null
  Install @{ Version = 'v2.0.1' } | Out-Null; Assert-Version v2.0.1
  Remove-Item Env:CODEGRAPH_INSTALL_TEST_ARCH
  $hostArch = [string][System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
  $hostTarget = @{ X64 = 'windows_amd64'; Arm64 = 'windows_arm64' }[$hostArch]
  if ($hostTarget) {
    Publish v2.0.1 $hostTarget $v201 | Out-Null
    Install @{ Version = 'v2.0.1' } | Out-Null; Assert-Version v2.0.1
  } else { "SKIP: host architecture $hostArch has no Windows release target" }
  Assert-NoLeftovers
  'ok target selection'

  $sep = [IO.Path]::PathSeparator
  $shadow = Join-Path $tmp 'shadow'
  New-Item -ItemType Directory -Path $shadow | Out-Null
  Copy-Item $v200 (Join-Path $shadow 'codegraph.exe')
  $env:PATH = "$shadow$sep$dest$sep$($savedEnv.PATH)"
  $out = Install @{ Version = 'v2.0.1' }
  if ($out -notlike "*resolves to $(Join-Path $shadow 'codegraph.exe'), not $exe*") { throw "missing PATH shadow warning: $out" }
  $env:PATH = "$dest$sep$shadow$sep$($savedEnv.PATH)"
  $out = Install @{ Version = 'v2.0.1' }
  if ($out -like '*resolves to*' -or $out -like '*to PATH to run*') { throw "unexpected PATH warning: $out" }
  $env:PATH = $savedEnv.PATH
  'ok PATH shadow warning'

  if ($IsWindows -or $env:OS -eq 'Windows_NT') {
    $running = Start-Process -FilePath $exe -ArgumentList 'sleep' -PassThru -WindowStyle Hidden
    try {
      Start-Sleep -Milliseconds 500
      Install @{ Version = 'v2.0.0' } | Out-Null
      Assert-Version v2.0.0
      if (!(Get-ChildItem -Path $dest -Filter 'codegraph.exe.old-*')) { throw 'running executable was not moved aside' }
    } finally { Stop-Process -Id $running.Id -Force -ErrorAction SilentlyContinue; $running.WaitForExit() }
    Install @{ Version = 'v2.0.1' } | Out-Null
    Assert-Version v2.0.1; Assert-NoLeftovers
    'ok upgrade while codegraph.exe is running'
  } else {
    'SKIP: upgrade while codegraph.exe is running (Windows only)'
  }

  New-Item -ItemType File -Path (Join-Path $dest 'unrelated') | Out-Null
  $out = Install @{ Uninstall = $true }
  if ((Test-Path $exe) -or !(Test-Path (Join-Path $dest 'unrelated'))) { throw 'uninstall removed the wrong files' }
  if ($out -notlike "*Removed $exe*") { throw "missing uninstall message: $out" }
  $out = Install @{ Uninstall = $true }
  if ($out -notlike '*not installed*') { throw "missing not-installed message: $out" }
  'ok uninstall'

  'PowerShell installer tests passed'
} finally {
  foreach ($name in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($name, $savedEnv[$name]) }
  Remove-Item -Recurse -Force $tmp
}
