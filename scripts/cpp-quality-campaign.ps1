<#
.SYNOPSIS
Runs a local, evidence-only CodeGraph campaign against a C/C++ checkout.

.PARAMETER RepositoryPath
Path to the intended repository checkout.

.PARAMETER CodeGraph
Path to the CodeGraph executable; defaults to `codegraph` on PATH.

.PARAMETER OutputDirectory
Outside-repository directory for the isolated database and machine-readable report.

.PARAMETER SymbolCheck, CallersOf, CalleesOf
Optional representative query targets. Each can be supplied more than once.

.PARAMETER MutationProbe
Copy the checkout to a temporary directory, then test add/update/delete behavior
there. The original checkout is never changed.

Example:
  .\scripts\cpp-quality-campaign.ps1 -RepositoryPath D:\RebornEP8Source `
    -CodeGraph C:\tools\codegraph.exe -OutputDirectory D:\cg-evidence\archlord
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string] $RepositoryPath,
    [string] $CodeGraph = "codegraph",
    [Parameter(Mandatory = $true)] [string] $OutputDirectory,
    [string[]] $SymbolCheck = @(),
    [string[]] $CallersOf = @(),
    [string[]] $CalleesOf = @(),
    [switch] $MutationProbe
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Get-AbsolutePath([string] $Path) {
    return [System.IO.Path]::GetFullPath((Resolve-Path -LiteralPath $Path).Path)
}

function Test-PathWithin([string] $Path, [string] $Parent) {
    $prefix = $Parent.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    return $Path.Equals($Parent, [System.StringComparison]::OrdinalIgnoreCase) -or $Path.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)
}

function Get-RelativePath([string] $Root, [string] $Path) {
    $prefix = $Root.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $Path.StartsWith($prefix, [System.StringComparison]::OrdinalIgnoreCase)) { throw "Path is outside repository root: $Path" }
    return $Path.Substring($prefix.Length).Replace([string][System.IO.Path]::DirectorySeparatorChar, '/')
}

function Get-Sha256([byte[]] $Bytes) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { return [System.BitConverter]::ToString($sha.ComputeHash($Bytes)).Replace("-", "").ToLowerInvariant() }
    finally { $sha.Dispose() }
}

function Invoke-Native([string] $Executable, [string[]] $Arguments, [string] $WorkingDirectory, [string] $CodeGraphHome = $null) {
    $stderrPath = [System.IO.Path]::GetTempFileName()
    try {
        Push-Location -LiteralPath $WorkingDirectory
        try {
            if ($null -ne $CodeGraphHome) { $env:CODEGRAPH_HOME = $CodeGraphHome }
            $stdout = @(& $Executable @Arguments 2> $stderrPath | ForEach-Object { [string] $_ })
            $exitCode = $LASTEXITCODE
            $stderr = [System.IO.File]::ReadAllText($stderrPath)
        }
        finally {
            Pop-Location
        }
        return [pscustomobject]@{
            executable = $Executable
            arguments = @($Arguments)
            exit_code = $exitCode
            stdout = ($stdout -join [Environment]::NewLine)
            stderr = $stderr
        }
    }
    finally {
        Remove-Item -LiteralPath $stderrPath -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-CodeGraph([string] $Name, [string[]] $Arguments, [string] $Repo, [string] $ConfigHome, [System.Collections.Generic.List[object]] $Commands) {
    $result = Invoke-Native $script:CodeGraphPath $Arguments $Repo $ConfigHome
    $result | Add-Member -NotePropertyName name -NotePropertyValue $Name | Out-Null
    $null = $Commands.Add($result)
    return $result
}

function Get-RepositoryFiles([string] $Root) {
    $files = [System.Collections.Generic.List[object]]::new()
    $pending = [System.Collections.Generic.Stack[string]]::new()
    $pending.Push($Root)
    while ($pending.Count -gt 0) {
        $directory = $pending.Pop()
        foreach ($entry in (Get-ChildItem -LiteralPath $directory -Force -ErrorAction Stop)) {
            if (($entry.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
            if ($entry.PSIsContainer) {
                if ($entry.Name -notin @(".git", ".codegraph")) { $pending.Push($entry.FullName) }
            } else {
                $files.Add($entry)
            }
        }
    }
    return @($files)
}

function Copy-RepositoryForProbe([string] $Source, [string] $Destination) {
    $pending = [System.Collections.Generic.Stack[object]]::new()
    $pending.Push([pscustomobject]@{ source = $Source; destination = $Destination })
    while ($pending.Count -gt 0) {
        $current = $pending.Pop()
        foreach ($entry in (Get-ChildItem -LiteralPath $current.source -Force -ErrorAction Stop)) {
            if (($entry.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
            if ($entry.PSIsContainer) {
                if ($entry.Name -in @(".git", ".codegraph")) { continue }
                $target = Join-Path $current.destination $entry.Name
                $null = New-Item -ItemType Directory -Path $target -Force
                $pending.Push([pscustomobject]@{ source = $entry.FullName; destination = $target })
            } else {
                Copy-Item -LiteralPath $entry.FullName -Destination (Join-Path $current.destination $entry.Name) | Out-Null
            }
        }
    }
}

function Remove-OwnedProbeDirectory([string] $Path, [string] $Parent) {
    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $fullParent = [System.IO.Path]::GetFullPath($Parent).TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if (-not $fullPath.StartsWith($fullParent, [System.StringComparison]::OrdinalIgnoreCase) -or
        [System.IO.Path]::GetDirectoryName($fullPath) -ne $fullParent.TrimEnd([System.IO.Path]::DirectorySeparatorChar)) {
        throw "Refusing to remove probe directory outside its owned temporary parent: $Path"
    }
    $item = Get-Item -LiteralPath $fullPath -Force -ErrorAction Stop
    if (-not $item.PSIsContainer -or ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Refusing to remove probe directory that is not an ordinary directory: $Path"
    }
    $pending = [System.Collections.Generic.Stack[System.IO.DirectoryInfo]]::new()
    $pending.Push([System.IO.DirectoryInfo]::new($fullPath))
    while ($pending.Count -gt 0) {
        $directory = $pending.Peek()
        $current = Get-Item -LiteralPath $directory.FullName -Force -ErrorAction Stop
        $withinOwnedRoot = $directory.FullName.Equals($fullPath, [System.StringComparison]::OrdinalIgnoreCase) -or
            $directory.FullName.StartsWith($fullPath + [System.IO.Path]::DirectorySeparatorChar, [System.StringComparison]::OrdinalIgnoreCase)
        if (($current.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0 -or -not $withinOwnedRoot) {
            throw "Refusing to traverse replaced or redirected cleanup directory: $($directory.FullName)"
        }
        $children = @($directory.EnumerateFileSystemInfos())
        if ($children.Count -gt 0) {
            foreach ($child in $children) {
                if (($child.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                    $child.Delete()
                } elseif (($child.Attributes -band [System.IO.FileAttributes]::Directory) -ne 0) {
                    if (-not $child.FullName.StartsWith($fullPath + [System.IO.Path]::DirectorySeparatorChar, [System.StringComparison]::OrdinalIgnoreCase)) {
                        throw "Refusing to remove path outside owned probe directory: $($child.FullName)"
                    }
                    $pending.Push([System.IO.DirectoryInfo]::new($child.FullName))
                } else {
                    $child.Delete()
                }
            }
        } else {
            $pending.Pop() | Out-Null
            $directory.Delete()
        }
    }
}

function Protect-OwnedProbeDirectory([string] $Path) {
    if ($env:OS -eq "Windows_NT") {
        $acl = Get-Acl -LiteralPath $Path
        $acl.SetAccessRuleProtection($true, $false)
        foreach ($rule in @($acl.Access)) { $null = $acl.RemoveAccessRuleSpecific($rule) }
        $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
        $rule = [System.Security.AccessControl.FileSystemAccessRule]::new($sid, "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")
        $null = $acl.AddAccessRule($rule)
        Set-Acl -LiteralPath $Path -AclObject $acl
    } else {
        $mode = [System.IO.UnixFileMode]::UserRead -bor [System.IO.UnixFileMode]::UserWrite -bor [System.IO.UnixFileMode]::UserExecute
        [System.IO.File]::SetUnixFileMode($Path, $mode)
    }
}

function Get-ContentFingerprint($Files, [string] $Root, [switch] $AllFiles) {
    $sourceExtensions = @(".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx", ".inl", ".ipp", ".tpp")
    $selected = if ($AllFiles) { $Files } else { $Files | Where-Object { $sourceExtensions -contains $_.Extension.ToLowerInvariant() } }
    $entries = foreach ($file in ($selected | Sort-Object FullName)) {
        $relative = Get-RelativePath $Root $file.FullName
        $bytes = [System.IO.File]::ReadAllBytes($file.FullName)
        $hash = Get-Sha256 $bytes
        [pscustomobject]@{ path = $relative; sha256 = $hash } | ConvertTo-Json -Compress
    }
    $manifest = [string]::Join("`n", @($entries))
    $manifestBytes = [System.Text.Encoding]::UTF8.GetBytes($manifest)
    return Get-Sha256 $manifestBytes
}

function Get-GitIdentity([string] $Root) {
    $head = Invoke-Native "git" @("rev-parse", "HEAD") $Root
    $status = Invoke-Native "git" @("status", "--porcelain", "--untracked-files=all") $Root
    return [pscustomobject]@{
        head = if ($head.exit_code -eq 0) { $head.stdout.Trim() } else { $null }
        dirty = if ($status.exit_code -eq 0) { -not [string]::IsNullOrWhiteSpace($status.stdout) } else { $null }
        status = if ($status.exit_code -eq 0) { $status.stdout } else { $null }
        head_command = $head
        status_command = $status
    }
}

function Add-IndexParseCounts($IndexResult) {
    $counts = [ordered]@{}
    foreach ($line in ($IndexResult.stdout -split "`r?`n")) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try { $event = $line | ConvertFrom-Json } catch { continue }
        if ($event.type -ne "scan_summary" -or $null -eq $event.data.language_coverage) { continue }
        foreach ($language in $event.data.language_coverage.PSObject.Properties) {
            $counts[$language.Name] = [int]$language.Value.parse_failed
        }
    }
    return $counts
}

function Get-IndexSummary($IndexResult) {
    foreach ($line in ($IndexResult.stdout -split "`r?`n")) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try { $event = $line | ConvertFrom-Json } catch { continue }
        if ($event.type -eq "scan_summary") { return $event.data }
    }
    return $null
}

$repo = Get-AbsolutePath $RepositoryPath
if (-not (Test-Path -LiteralPath $repo -PathType Container)) { throw "RepositoryPath is not a directory: $RepositoryPath" }
$out = [System.IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $out) { $out = Get-AbsolutePath $out }
if (Test-PathWithin $out $repo) { throw "OutputDirectory must be outside the checkout to keep source and evidence separate." }
if ((Test-Path -LiteralPath $out) -and (Get-ChildItem -LiteralPath $out -Force | Select-Object -First 1)) {
    throw "OutputDirectory must be new or empty so this run starts with a fresh isolated database and preserves prior evidence."
}
if (-not (Test-Path -LiteralPath $CodeGraph -PathType Leaf)) {
    $command = Get-Command $CodeGraph -ErrorAction SilentlyContinue
    if ($null -eq $command) { throw "CodeGraph executable not found: $CodeGraph" }
    $script:CodeGraphPath = $command.Source
} else {
    $script:CodeGraphPath = Get-AbsolutePath $CodeGraph
}

$null = New-Item -ItemType Directory -Path $out -Force
$dbDirectory = Join-Path $out "database"
$codeGraphHome = Join-Path $out "codegraph-home"
$null = New-Item -ItemType Directory -Path $dbDirectory -Force
$null = New-Item -ItemType Directory -Path (Join-Path $codeGraphHome "config") -Force
$config = @{ db_dir = $dbDirectory } | ConvertTo-Json -Compress
$utf8WithoutBOM = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText((Join-Path $codeGraphHome "config/config.json"), $config, $utf8WithoutBOM)

$files = Get-RepositoryFiles $repo
$extensions = @(".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx", ".inl", ".ipp", ".tpp")
$sourceFiles = @($files | Where-Object { $extensions -contains $_.Extension.ToLowerInvariant() })
$headerExtensions = @(".h", ".hh", ".hpp", ".hxx", ".inl", ".ipp", ".tpp")
$solutions = @($files | Where-Object { $_.Name -match '\.sln$' } | ForEach-Object { Get-RelativePath $repo $_.FullName } | Sort-Object)
$buildMetadata = @($files | Where-Object { $_.Name -match '^(CMakeLists\.txt|Makefile|meson\.build|[^/]+\.(vcxproj|vcproj|props|targets))$' } | ForEach-Object { Get-RelativePath $repo $_.FullName } | Sort-Object)
$compileCommands = @($files | Where-Object { $_.Name -eq "compile_commands.json" } | ForEach-Object { Get-RelativePath $repo $_.FullName } | Sort-Object)

$commands = [System.Collections.Generic.List[object]]::new()
$version = Invoke-CodeGraph "version" @("version") $repo $codeGraphHome $commands
$buildInfo = $null
$goCommand = Get-Command go -ErrorAction SilentlyContinue
if ($null -ne $goCommand) {
    $buildInfo = Invoke-Native $goCommand.Source @("version", "-m", $script:CodeGraphPath) $repo
    $buildInfo | Add-Member -NotePropertyName name -NotePropertyValue "codegraph_build_info" | Out-Null
    $null = $commands.Add($buildInfo)
}
$index = Invoke-CodeGraph "fresh_index" @("index", $repo, "--jsonl", "--no-history") $repo $codeGraphHome $commands
$stats = Invoke-CodeGraph "stats" @("stats", $repo) $repo $codeGraphHome $commands
$audit = Invoke-CodeGraph "graph_audit" @("audit", $repo, "--examples", "0") $repo $codeGraphHome $commands

$queries = [System.Collections.Generic.List[object]]::new()
foreach ($symbol in $SymbolCheck) {
    $queries.Add((Invoke-CodeGraph "find_symbol:$symbol" @("find_symbol", $repo, $symbol, "--limit", "20") $repo $codeGraphHome $commands))
}
foreach ($symbol in $CallersOf) {
    $queries.Add((Invoke-CodeGraph "find_callers:$symbol" @("find_callers", $repo, $symbol, "--limit", "20") $repo $codeGraphHome $commands))
}
foreach ($symbol in $CalleesOf) {
    $queries.Add((Invoke-CodeGraph "find_callees:$symbol" @("find_callees", $repo, $symbol, "--limit", "20") $repo $codeGraphHome $commands))
}

$mutation = $null
if ($MutationProbe) {
    $tempParent = (Get-AbsolutePath ([System.IO.Path]::GetTempPath())).TrimEnd([System.IO.Path]::DirectorySeparatorChar)
    $probeRoot = Join-Path $tempParent ("codegraph-cpp-mutation-" + [guid]::NewGuid().ToString("N"))
    $probeHome = Join-Path $out "mutation-codegraph-home"
    $probeDB = Join-Path $out "mutation-database"
    $probeOwned = $false
    try {
        $null = New-Item -ItemType Directory -Path $probeRoot -ErrorAction Stop
        $probeOwned = $true
        Protect-OwnedProbeDirectory $probeRoot
        $probeRoot = Get-AbsolutePath $probeRoot
        $null = New-Item -ItemType Directory -Path $probeDB -Force
        $null = New-Item -ItemType Directory -Path (Join-Path $probeHome "config") -Force
        $probeConfig = @{ db_dir = $probeDB } | ConvertTo-Json -Compress
        [System.IO.File]::WriteAllText((Join-Path $probeHome "config/config.json"), $probeConfig, $utf8WithoutBOM)
        Copy-RepositoryForProbe $repo $probeRoot
        $originalSourceFingerprint = Get-ContentFingerprint $files $repo -AllFiles
        $probeIndex = Invoke-CodeGraph "mutation_initial_index" @("index", $probeRoot, "--jsonl", "--no-history") $probeRoot $probeHome $commands
        if ($probeIndex.exit_code -ne 0) { throw "Temporary checkout indexing failed." }
        $eligible = @($sourceFiles | Where-Object { $_.Extension.ToLowerInvariant() -in @(".c", ".cc", ".cpp", ".cxx") } | Sort-Object { Get-RelativePath $repo $_.FullName })
        if ($eligible.Count -eq 0) { throw "Mutation probe cannot modify existing file: repository has no eligible C/C++ source file." }
        $selectedRelative = Get-RelativePath $repo $eligible[0].FullName
        $probeExistingFile = Join-Path $probeRoot ($selectedRelative.Replace('/', [System.IO.Path]::DirectorySeparatorChar))
        $originalBytes = [System.IO.File]::ReadAllBytes($probeExistingFile)
        $markerBytes = [System.Text.Encoding]::ASCII.GetBytes("`n// codegraph campaign mutation probe`n")
        $modifiedBytes = [byte[]]::new($originalBytes.Length + $markerBytes.Length)
        [System.Buffer]::BlockCopy($originalBytes, 0, $modifiedBytes, 0, $originalBytes.Length)
        [System.Buffer]::BlockCopy($markerBytes, 0, $modifiedBytes, $originalBytes.Length, $markerBytes.Length)
        [System.IO.File]::WriteAllBytes($probeExistingFile, $modifiedBytes)
        $probeModify = Invoke-CodeGraph "mutation_existing_file_update" @("update_graph", $probeRoot, "--jsonl", "--no-history") $probeRoot $probeHome $commands
        $modifySummary = Get-IndexSummary $probeModify
        if ($probeModify.exit_code -ne 0 -or $null -eq $modifySummary -or $modifySummary.files_changed -lt 1) { throw "Existing-file modification was not reindexed." }
        $probeFile = Join-Path $probeRoot "codegraph-campaign-probe.cpp"
        [System.IO.File]::WriteAllText($probeFile, "int codegraph_campaign_probe() { return 7; }`n", $utf8WithoutBOM)
        $probeAdd = Invoke-CodeGraph "mutation_add_update" @("update_graph", $probeRoot, "--jsonl", "--no-history") $probeRoot $probeHome $commands
        $addSummary = Get-IndexSummary $probeAdd
        Remove-Item -LiteralPath $probeFile -Force
        $probeDelete = Invoke-CodeGraph "mutation_delete_update" @("update_graph", $probeRoot, "--jsonl", "--no-history") $probeRoot $probeHome $commands
        $deleteSummary = Get-IndexSummary $probeDelete
        if ($probeAdd.exit_code -ne 0 -or $probeDelete.exit_code -ne 0 -or $null -eq $addSummary -or $addSummary.files_changed -lt 1 -or $null -eq $deleteSummary -or $deleteSummary.files_deleted -lt 1) {
            throw "Add/delete mutation probe failed."
        }
        if ((Get-ContentFingerprint $files $repo -AllFiles) -ne $originalSourceFingerprint) { throw "Original checkout files changed during mutation probe." }
        $mutation = [pscustomobject]@{
            existing_file = $selectedRelative
            existing_file_update_exit_code = $probeModify.exit_code
            existing_file_update_summary = $modifySummary
            original_source_preserved = $true
            add_update_exit_code = $probeAdd.exit_code
            delete_update_exit_code = $probeDelete.exit_code
            add_update_summary = $addSummary
            delete_update_summary = $deleteSummary
            probe_file = "codegraph-campaign-probe.cpp"
            temporary_copy_policy = "recursive content copy excluding .git, .codegraph, and reparse points; all mutations occur in runner-owned temporary checkout"
        }
    } catch {
        $failedCommands = $commands | ConvertTo-Json -Depth 100
        [System.IO.File]::WriteAllText((Join-Path $out "failed-mutation-commands.json"), $failedCommands + [Environment]::NewLine, $utf8WithoutBOM)
        throw "Mutation probe failed; command evidence saved to $out/failed-mutation-commands.json. $($_.Exception.Message)"
    } finally {
        $mutationCommands = $commands | ConvertTo-Json -Depth 100
        [System.IO.File]::WriteAllText((Join-Path $out "mutation-commands.json"), $mutationCommands + [Environment]::NewLine, $utf8WithoutBOM)
        if ($probeOwned -and (Test-Path -LiteralPath $probeRoot)) {
            try {
                Remove-OwnedProbeDirectory $probeRoot $tempParent
            } catch {
                $cleanupFailure = [pscustomobject]@{ temporary_repository = $probeRoot; cleanup_error = $_.Exception.Message; commands = @($commands) } | ConvertTo-Json -Depth 100
                [System.IO.File]::WriteAllText((Join-Path $out "mutation-cleanup-failure.json"), $cleanupFailure + [Environment]::NewLine, $utf8WithoutBOM)
                throw "Mutation probe cleanup failed; evidence saved to $out/mutation-cleanup-failure.json. $($_.Exception.Message)"
            }
        }
    }
}

$git = Get-GitIdentity $repo
$goVersion = $null
$cgoEnabled = $null
if ($null -ne $buildInfo -and $buildInfo.exit_code -eq 0) {
    if ($buildInfo.stdout -match ': (go[0-9.]+)') { $goVersion = $Matches[1] }
    if ($buildInfo.stdout -match '(?m)^\s*build\s+CGO_ENABLED=(\d+)\s*$') { $cgoEnabled = $Matches[1] -eq "1" }
}
$report = [pscustomobject]@{
    schema = "codegraph.cpp_quality_campaign/v1"
    generated_at_utc = [DateTime]::UtcNow.ToString("o")
    identity = [pscustomobject]@{
        repository_root = $repo
        root_marker = "repository-relative paths normalized to /"
        git = $git
        c_cpp_content_sha256 = Get-ContentFingerprint $files $repo
        codegraph_version = $version.stdout
        codegraph_executable_sha256 = Get-Sha256 ([System.IO.File]::ReadAllBytes($script:CodeGraphPath))
        go_version = $goVersion
        cgo_enabled = $cgoEnabled
        environment = [pscustomobject]@{ os = [Environment]::OSVersion.VersionString; architecture = $env:PROCESSOR_ARCHITECTURE; powershell = $PSVersionTable.PSVersion.ToString(); edition = $PSVersionTable.PSEdition }
    }
    inventory = [pscustomobject]@{
        c_cpp_source_files = @($sourceFiles | Where-Object { $headerExtensions -notcontains $_.Extension.ToLowerInvariant() }).Count
        c_cpp_header_files = @($sourceFiles | Where-Object { $headerExtensions -contains $_.Extension.ToLowerInvariant() }).Count
        extensions_counted = [pscustomobject]@{
            source = @(".c", ".cc", ".cpp", ".cxx")
            header = $headerExtensions
        }
        solutions = $solutions
        build_metadata = $buildMetadata
        compile_commands = $compileCommands
    }
    parse_errors_by_language = Add-IndexParseCounts $index
    mutation_probe = $mutation
    queries = @($queries)
    commands = @($commands)
    evidence_policy = "Raw command outputs and exit codes only; no inferred quality score or source upload. Timestamps are report metadata, not run identity."
}
$reportPath = Join-Path $out "campaign-report.json"
$json = $report | ConvertTo-Json -Depth 100
[System.IO.File]::WriteAllText($reportPath, $json + [Environment]::NewLine, $utf8WithoutBOM)
Write-Output $reportPath

if (@($commands | Where-Object { $_.exit_code -ne 0 }).Count -gt 0) { exit 1 }
