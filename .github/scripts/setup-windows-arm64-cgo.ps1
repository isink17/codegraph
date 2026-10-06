Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
# The hosted runner's default gcc is x86_64 MinGW and cannot assemble Go's AArch64 runtime/cgo files.
$version = '20260922'
$archive = "llvm-mingw-$version-ucrt-aarch64.zip"
$sha256 = 'a317514a7a63badd692032c0c2b8e165f630bbaebbe7a3254348051f43a64949'
$root = Join-Path $env:RUNNER_TEMP 'llvm-mingw'
$zip = Join-Path $env:RUNNER_TEMP $archive
$url = "https://github.com/mstorsjo/llvm-mingw/releases/download/$version/$archive"
Invoke-WebRequest -Uri $url -OutFile $zip
$actual = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
if ($actual -ne $sha256) { throw "llvm-mingw SHA-256 mismatch: $actual" }
Expand-Archive -Path $zip -DestinationPath $root -Force
$clang = Get-ChildItem $root -Recurse -Filter aarch64-w64-mingw32-clang.exe | Select-Object -First 1 -ExpandProperty FullName
$clangxx = Get-ChildItem $root -Recurse -Filter aarch64-w64-mingw32-clang++.exe | Select-Object -First 1 -ExpandProperty FullName
if (!$clang -or !$clangxx) { throw 'ARM64 MinGW clang drivers missing from pinned archive.' }
$target = (& $clang --target=aarch64-w64-windows-gnu -print-target-triple).Trim()
& $clang --version
Write-Host "clang target=$target"
if ($target -ne 'aarch64-w64-windows-gnu') { throw "Unexpected clang target: $target" }
$bin = Split-Path $clang
"$bin" | Out-File -FilePath $env:GITHUB_PATH -Encoding utf8 -Append
"CC=$clang" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
"CXX=$clangxx" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
"CGO_ENABLED=1" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
go env -w CC=$clang CXX=$clangxx CGO_ENABLED=1
$env:PATH = "$bin;$env:PATH"
$env:CC = $clang
$env:CXX = $clangxx
$env:CGO_ENABLED = '1'
go env CC CXX
where.exe (Split-Path $clang -Leaf)
& $clang --version
$probe = Join-Path $env:RUNNER_TEMP 'cgo-probe.go'
@'
package main
/*
int answer(void) { return 42; }
*/
import "C"
func main() { if C.answer() != 42 { panic("bad cgo result") } }
'@ | Set-Content $probe
$exe = Join-Path $env:RUNNER_TEMP 'cgo-probe.exe'
$env:GOOS = 'windows'; $env:GOARCH = 'arm64'
go build -o $exe $probe
$readobj = Get-ChildItem $root -Recurse -Filter llvm-readobj.exe | Select-Object -First 1 -ExpandProperty FullName
if (!$readobj) { throw 'llvm-readobj missing from pinned llvm-mingw archive.' }
$headers = & $readobj --file-headers $exe
$headers
if (($headers -join "`n") -notmatch 'IMAGE_FILE_MACHINE_ARM64') { throw 'cgo probe is not ARM64 PE.' }
& $exe
if ($LASTEXITCODE -ne 0) { throw "Native ARM64 cgo probe exited $LASTEXITCODE" }
Write-Host 'CGO probe compiled, linked, identified as IMAGE_FILE_MACHINE_ARM64, and ran successfully.'
