# Fetch, verify and unpack the pinned SQLite amalgamation (third_party/sqlite/).
# Windows counterpart of fetch-sqlite.sh; same pins, same checks.
#
# Usage: scripts/release/fetch-sqlite.ps1 [-Dest DIR] [-Archive FILE]
#   -Dest     unpack directory, outside the repository
#             (default: <temp>\hqq-sqlite-<version>)
#   -Archive  verify and unpack this local copy instead of downloading it
#
# Prints the amalgamation directory on stdout. The archive must match the
# pinned SHA256 before anything is unpacked. SHA3-256 is checked too when the
# runtime provides System.Security.Cryptography.SHA3_256 (.NET 8+ on
# supported Windows versions); otherwise the log says it was not checked.
[CmdletBinding()]
param(
    [string]$Dest = '',
    [string]$Archive = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2

function Fail([string]$Message) {
    [Console]::Error.WriteLine("fetch-sqlite: $Message")
    exit 1
}

function Note([string]$Message) {
    [Console]::Error.WriteLine("fetch-sqlite: $Message")
}

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$Root = [System.IO.Path]::GetFullPath((Join-Path $ScriptDir '..\..'))
$PinDir = Join-Path (Join-Path $Root 'third_party') 'sqlite'

# VERSION: key=value lines; '#' starts a comment.
$pins = @{}
foreach ($line in Get-Content -LiteralPath (Join-Path $PinDir 'VERSION')) {
    if ($line -match '^\s*(#|$)') { continue }
    $key, $value = $line -split '=', 2
    if ($null -eq $value) { Fail "bad line in VERSION: $line" }
    $pins[$key.Trim()] = $value.Trim()
}
foreach ($key in 'version', 'archive', 'url') {
    if (-not $pins.ContainsKey($key) -or [string]::IsNullOrEmpty($pins[$key])) {
        Fail "missing $key in $PinDir\VERSION"
    }
}
$Version = $pins['version']
$ArchiveName = $pins['archive']
$Url = $pins['url']
if ($Url -notlike 'https://*') { Fail "url must be https: $Url" }
if ($ArchiveName -match '[\\/]' -or $ArchiveName.StartsWith('.')) {
    Fail "archive must be a plain file name: $ArchiveName"
}

function Get-Pinned([string]$File, [string]$Name) {
    $hash = $null
    foreach ($line in Get-Content -LiteralPath $File) {
        $parts = $line.Trim() -split '\s+', 2
        if ($parts.Count -eq 2 -and $parts[1] -eq $Name) {
            $hash = $parts[0].ToLowerInvariant()
            break
        }
    }
    if ($null -eq $hash -or $hash -notmatch '^[0-9a-f]{64}$') {
        Fail "no valid entry for $Name in $File"
    }
    return $hash
}
$PinSha256 = Get-Pinned (Join-Path $PinDir 'SHA256') $ArchiveName
$PinSha3 = Get-Pinned (Join-Path $PinDir 'SHA3-256') $ArchiveName

$Sha3Supported = $false
try {
    $Sha3Supported = [System.Security.Cryptography.SHA3_256]::IsSupported
} catch {
    $Sha3Supported = $false
}

function Test-Archive([string]$Path) {
    $sha256 = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($sha256 -ne $PinSha256) { return $false }
    if ($Sha3Supported) {
        $bytes = [System.IO.File]::ReadAllBytes($Path)
        $digest = [System.Security.Cryptography.SHA3_256]::HashData($bytes)
        $sha3 = ($digest | ForEach-Object { $_.ToString('x2') }) -join ''
        return ($sha3 -eq $PinSha3)
    }
    return $true
}

if ([string]::IsNullOrEmpty($Dest)) {
    $Dest = Join-Path ([System.IO.Path]::GetTempPath()) "hqq-sqlite-$Version"
}
$Dest = [System.IO.Path]::GetFullPath((Join-Path (Get-Location) $Dest))
$RootFull = $Root.TrimEnd('\', '/')
$Sep = [System.IO.Path]::DirectorySeparatorChar
if ($Dest.Equals($RootFull, [StringComparison]::OrdinalIgnoreCase) -or
    $Dest.StartsWith($RootFull + $Sep, [StringComparison]::OrdinalIgnoreCase)) {
    Fail "refusing to unpack inside the repository: $Dest"
}
New-Item -ItemType Directory -Force -Path $Dest | Out-Null

if ($Archive) {
    if (-not (Test-Path -LiteralPath $Archive -PathType Leaf)) { Fail "no such archive: $Archive" }
    $zip = (Resolve-Path -LiteralPath $Archive).Path
    if (-not (Test-Archive $zip)) { Fail "checksum mismatch: $zip (pinned SHA256 $PinSha256)" }
} else {
    $zip = Join-Path $Dest $ArchiveName
    if ((Test-Path -LiteralPath $zip -PathType Leaf) -and (Test-Archive $zip)) {
        Note "reusing verified $zip"
    } else {
        Note "downloading $Url"
        if ($PSVersionTable.PSVersion.Major -lt 6) {
            [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        }
        $part = "$zip.part"
        $downloaded = $false
        for ($attempt = 1; $attempt -le 3 -and -not $downloaded; $attempt++) {
            try {
                Invoke-WebRequest -Uri $Url -OutFile $part -UseBasicParsing
                $downloaded = $true
            } catch {
                Note "download attempt $attempt failed: $($_.Exception.Message)"
                Start-Sleep -Seconds 2
            }
        }
        if (-not $downloaded) { Fail "download failed: $Url" }
        if (-not (Test-Archive $part)) {
            Remove-Item -LiteralPath $part -Force
            Fail "checksum mismatch for the download from $Url; nothing was unpacked"
        }
        Move-Item -LiteralPath $part -Destination $zip -Force
    }
}

if ($Sha3Supported) {
    Note 'checksum ok: SHA256 and SHA3-256 match the pins'
} else {
    Note 'checksum ok: SHA256 matches the pin; SHA3-256 not checked (no .NET SHA3_256)'
}

# Refuse entry names that would escape the unpack directory.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$entries = [System.IO.Compression.ZipFile]::OpenRead($zip)
try {
    foreach ($entry in $entries.Entries) {
        if ($entry.FullName -match '^[\\/]' -or $entry.FullName -match '(^|[\\/])\.\.([\\/]|$)') {
            Fail "archive has absolute or parent-relative member names: $($entry.FullName)"
        }
    }
} finally {
    $entries.Dispose()
}

$base = [System.IO.Path]::GetFileNameWithoutExtension($ArchiveName)
$unpacked = Join-Path $Dest $base
$stage = Join-Path $Dest ".stage-$PID"
if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
New-Item -ItemType Directory -Path $stage | Out-Null
try {
    Expand-Archive -LiteralPath $zip -DestinationPath $stage -Force
    $src = Join-Path $stage $base
    if (-not (Test-Path -LiteralPath $src -PathType Container)) { Fail "archive does not contain $base/" }
    if (Test-Path -LiteralPath $unpacked) { Remove-Item -LiteralPath $unpacked -Recurse -Force }
    Move-Item -LiteralPath $src -Destination $unpacked
} finally {
    Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
}

foreach ($file in 'sqlite3.c', 'sqlite3.h') {
    if (-not (Test-Path -LiteralPath (Join-Path $unpacked $file) -PathType Leaf)) {
        Fail "missing $file in $unpacked"
    }
}

Note "SQLite $Version unpacked in $unpacked"
Write-Output $unpacked
