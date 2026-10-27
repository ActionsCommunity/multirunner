[CmdletBinding()]
param(
    [string]$Version = $(if ($env:MULTIRUNNER_VERSION) { $env:MULTIRUNNER_VERSION } else { "latest" }),
    [string]$InstallDir = $env:MULTIRUNNER_INSTALL_DIR,
    [switch]$NoPathUpdate
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$repository = "ActionsCommunity/multirunner"

if (-not $IsWindows -and $PSVersionTable.PSEdition -eq "Core") {
    throw "install.ps1 supports Windows. Use scripts/install.sh on Linux or macOS."
}
if (-not $env:LOCALAPPDATA) {
    throw "LOCALAPPDATA is required"
}
if (-not $InstallDir) {
    $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\Multirunner"
}

[Net.ServicePointManager]::SecurityProtocol =
    [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

if ($Version -eq "latest") {
    $headers = @{
        Accept = "application/vnd.github+json"
        "X-GitHub-Api-Version" = "2022-11-28"
        "User-Agent" = "multirunner-installer"
    }
    $release = Invoke-RestMethod `
        -Uri "https://api.github.com/repos/$repository/releases/latest" `
        -Headers $headers
    $Version = [string]$release.tag_name
}
if ($Version -notmatch '^v[0-9][0-9A-Za-z._-]*$') {
    throw "Invalid release tag: $Version"
}

$architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
switch ($architecture) {
    "X64" { $arch = "amd64" }
    "Arm64" { $arch = "arm64" }
    default { throw "Unsupported Windows architecture: $architecture" }
}

$asset = "multirunner_${Version}_windows_${arch}.exe"
$releaseUrl = "https://github.com/$repository/releases/download/$Version"
$tempDir = Join-Path ([IO.Path]::GetTempPath()) ("multirunner-install-" + [guid]::NewGuid())
$staged = $null
New-Item -ItemType Directory -Path $tempDir | Out-Null

try {
    $assetPath = Join-Path $tempDir $asset
    $sumsPath = Join-Path $tempDir "SHA256SUMS.txt"
    Write-Output "Downloading Multirunner $Version for windows/$arch..."
    Invoke-WebRequest -UseBasicParsing -Uri "$releaseUrl/$asset" -OutFile $assetPath
    Invoke-WebRequest -UseBasicParsing -Uri "$releaseUrl/SHA256SUMS.txt" -OutFile $sumsPath

    $escapedAsset = [regex]::Escape($asset)
    $checksumLines = @(
        Get-Content -LiteralPath $sumsPath |
            Where-Object { $_ -match "^([0-9A-Fa-f]{64})\s+\*?(?:\./)?$escapedAsset$" }
    )
    if ($checksumLines.Count -ne 1) {
        throw "SHA256SUMS.txt must contain exactly one entry for $asset"
    }
    $expected = ([regex]::Match($checksumLines[0], '^([0-9A-Fa-f]{64})')).Groups[1].Value.ToLowerInvariant()
    $actual = (Get-FileHash -LiteralPath $assetPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "Checksum verification failed for $asset"
    }

    Unblock-File -LiteralPath $assetPath
    $versionOutput = $null
    try {
        $versionOutput = (& $assetPath --version 2>&1 | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) {
            throw "Downloaded binary failed its version check"
        }
        if ($versionOutput -notmatch [regex]::Escape($Version)) {
            throw "Downloaded binary reports an unexpected version: $versionOutput"
        }
    } catch [System.Management.Automation.ApplicationFailedException] {
        if ($_.Exception.Message -notmatch 'Access is denied') {
            throw
        }
        # Some endpoint protection policies block a script from executing a
        # binary it just downloaded. The checksum above remains authoritative.
        Write-Warning ("Endpoint security prevented the installer from running the " +
            "downloaded binary. The SHA-256 checksum was verified; run " +
            "'multirunner --version' in a new terminal to confirm the installed version.")
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $target = Join-Path $InstallDir "multirunner.exe"
    $staged = Join-Path $InstallDir (".multirunner.exe.install." + $PID)
    $backup = "$target.previous"
    Copy-Item -LiteralPath $assetPath -Destination $staged
    if (Test-Path -LiteralPath $target) {
        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
        [IO.File]::Replace($staged, $target, $backup, $true)
    } else {
        [IO.File]::Move($staged, $target)
    }

    if (-not $NoPathUpdate) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $entries = @($userPath -split ';' | Where-Object { $_ })
        if ($entries -notcontains $InstallDir) {
            $newUserPath = (@($entries) + $InstallDir) -join ';'
            [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
            Write-Output "Added $InstallDir to your user PATH. Open a new terminal to use it."
        }
        if (($env:Path -split ';') -notcontains $InstallDir) {
            $env:Path = "$InstallDir;$env:Path"
        }
    }

    Write-Output "Installed $target"
    if (Test-Path -LiteralPath $backup) {
        Write-Output "Previous binary: $backup"
    }
    if ($versionOutput) {
        Write-Output $versionOutput
    }
} finally {
    if ($staged) {
        Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
