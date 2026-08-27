<#
.SYNOPSIS
  Installs a standalone Windows-container dockerd (Moby static binaries) as a
  service, so multirunner can run Windows runners WITHOUT Docker Desktop. The
  daemon listens on its own named pipe to avoid colliding with Podman/Docker
  Desktop's default \\.\pipe\docker_engine.

.NOTES
  Run elevated (Administrator). Enabling the Containers feature may require a reboot.
  Writes a status file + transcript under %ProgramData%\multirunner so the caller
  can report the outcome.

  Isolation defaults to 'auto': process on Windows Server, Hyper-V on client
  editions. Process isolation needs an exact host/container build match, so
  forcing it on a client edition leaves containers unable to start.

  The daemon pipe is ACL'd to $Group so non-elevated docker clients can reach it.

  DataRoot is where the daemon stores images and containers. It defaults to
  %ProgramData%\multirunner\docker\data, deliberately outside InstallDir:
  Windows base images run to several GB each, so the store belongs with the
  other mutable state (and often on another volume), not under Program Files.
  Pointing DataRoot at an existing store also lets this service adopt images
  from a daemon it replaces.
#>
[CmdletBinding()]
param(
    # Deliberately independent of the docker/docker Go SDK pin in go.mod. Both
    # clients call WithAPIVersionNegotiation, so a newer daemon is fine, while
    # a data-root written by a newer daemon cannot be served by an older one.
    # Track the current stable release rather than the SDK version.
    [string]$DockerVersion = '29.6.2',
    # Windows Defender's Controlled Folder Access ships an allowlist that names
    # %ProgramFiles%\DockerStandalone\dockerd.exe. Expanding a container's
    # scratch VHD is a raw disk write, which CFA blocks for any process not on
    # that list, surfacing as "hcsshim::ExpandScratchSize failed in Win32:
    # Access is denied. (0x5)" on every container create. Installing to the
    # allowlisted path keeps Windows containers working without asking anyone
    # to weaken Defender. The binaries therefore sit directly in InstallDir,
    # since the allowlist entry matches that exact path.
    [string]$InstallDir    = "$env:ProgramFiles\DockerStandalone",
    [string]$DataRoot      = '',
    [string]$Pipe          = 'npipe:////./pipe/docker_engine_windows',
    [string]$ServiceName   = 'multirunner-dockerd',
    [string]$Group         = 'docker-users',
    [ValidateSet('process', 'hyperv', 'auto')]
    [string]$Isolation     = 'auto'
)
$ErrorActionPreference = 'Stop'

$StatusDir = Join-Path $env:ProgramData 'multirunner'
New-Item -ItemType Directory -Force -Path $StatusDir | Out-Null
$StatusFile = Join-Path $StatusDir 'winsetup-status.txt'
$LogFile = Join-Path $StatusDir 'winsetup.log'
Set-Content -Path $StatusFile -Value 'running' -Encoding ascii
try { Start-Transcript -Path $LogFile -Force | Out-Null } catch {}

function Set-Status([string]$s) { Set-Content -Path $StatusFile -Value $s -Encoding ascii }

try {
    $id = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not $id.IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)) {
        throw 'This script must be run as Administrator.'
    }

    # 1. Containers feature
    $feature = Get-WindowsOptionalFeature -Online -FeatureName Containers
    if ($feature.State -ne 'Enabled') {
        Write-Host 'Enabling Windows Containers feature...'
        $res = Enable-WindowsOptionalFeature -Online -FeatureName Containers -All -NoRestart
        if ($res.RestartNeeded) {
            Write-Warning 'A reboot is required to finish enabling Containers. Reboot, then re-run.'
            Set-Status 'reboot-required'
            return
        }
    }

    # 2. Download Moby static binaries. They land directly in $InstallDir with
    # no bin\ subfolder so dockerd.exe matches the Controlled Folder Access
    # allowlist entry exactly; see the note on $InstallDir above. Extraction
    # goes through a temp staging dir so the zip's own docker\ folder is not
    # left behind inside the install directory.
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $dockerd = Join-Path $InstallDir 'dockerd.exe'
    if (-not (Test-Path $dockerd)) {
        $url = "https://download.docker.com/win/static/stable/x86_64/docker-$DockerVersion.zip"
        $zip = Join-Path $env:TEMP "docker-$DockerVersion.zip"
        $staging = Join-Path $env:TEMP "docker-$DockerVersion-extract"
        Write-Host "Downloading $url ..."
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
        Remove-Item $staging -Recurse -Force -ErrorAction SilentlyContinue
        Expand-Archive -Path $zip -DestinationPath $staging -Force   # extracts <staging>\docker\*.exe
        Copy-Item (Join-Path $staging 'docker\*.exe') $InstallDir -Force
        Remove-Item $zip, $staging -Recurse -Force -ErrorAction SilentlyContinue
    }

    # 3. Resolve isolation. Process isolation requires an exact host/container
    # build match and is only generally usable on Windows Server; client
    # editions (Pro/Enterprise/IoT) must use Hyper-V. Mirrors autoIsolation()
    # in internal/backend so the daemon default matches what multirunner asks
    # for per container.
    if ($Isolation -eq 'auto') {
        $installType = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion' `
            -Name InstallationType -ErrorAction SilentlyContinue).InstallationType
        $Isolation = if ($installType -eq 'Server') { 'process' } else { 'hyperv' }
        Write-Host "Auto-selected isolation: $Isolation (InstallationType=$installType)"
    }

    # 4. Ensure the pipe-access group exists. Without "group" in daemon.json the
    # named pipe ACL is Administrators/LocalSystem only, so a non-elevated
    # `docker -H <pipe> ...` fails with "permission denied". The multirunner
    # service runs as LocalSystem and is unaffected, but interactive use and
    # troubleshooting break without this.
    if (-not (Get-LocalGroup -Name $Group -ErrorAction SilentlyContinue)) {
        Write-Host "Creating local group $Group ..."
        New-LocalGroup -Name $Group -Description 'Access to the multirunner Windows docker pipe' | Out-Null
    }
    $me = "$env:USERDOMAIN\$env:USERNAME"
    $inGroup = $false
    try {
        $inGroup = [bool](Get-LocalGroupMember -Group $Group -ErrorAction Stop |
            Where-Object { $_.Name -eq $me })
    }
    catch { }
    if (-not $inGroup) {
        try {
            Add-LocalGroupMember -Group $Group -Member $me -ErrorAction Stop
            Write-Host "Added $me to $Group (sign out and back in for it to take effect)"
        }
        catch { Write-Warning "Could not add $me to ${Group}: $($_.Exception.Message)" }
    }

    # 5. daemon.json. Config and the image store live under %ProgramData%, not
    # InstallDir: Program Files is for read-only program files, while the image
    # store grows to many GB and the daemon rewrites config on every install.
    $cfgDir = Join-Path $StatusDir 'docker'
    if ([string]::IsNullOrWhiteSpace($DataRoot)) { $DataRoot = Join-Path $cfgDir 'data' }
    New-Item -ItemType Directory -Force -Path $cfgDir, $DataRoot | Out-Null
    $cfgPath = Join-Path $cfgDir 'daemon.json'
    $daemon = [ordered]@{
        hosts       = @($Pipe)
        group       = $Group
        'exec-opts' = @("isolation=$Isolation")
        'data-root' = $DataRoot
    }
    $daemon | ConvertTo-Json | Set-Content -Path $cfgPath -Encoding ascii
    Write-Host "Wrote $cfgPath"

    # 6. Register + start the service
    if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
        Write-Host "Service $ServiceName already exists; reconfiguring."
        Stop-Service $ServiceName -ErrorAction SilentlyContinue
        & sc.exe delete $ServiceName | Out-Null
        Start-Sleep -Seconds 2
    }
    Write-Host "Registering service $ServiceName ..."
    & $dockerd --register-service --service-name $ServiceName --config-file $cfgPath
    Start-Service $ServiceName

    Write-Host ''
    Write-Host "Done. Windows dockerd is running on: $Pipe"
    Write-Host "  isolation: $Isolation"
    Write-Host "  pipe access group: $Group"
    Write-Host "  data-root: $DataRoot"
    Set-Status 'ok'
}
catch {
    Set-Status ("error: " + $_.Exception.Message)
    Write-Error $_
}
finally {
    try { Stop-Transcript | Out-Null } catch {}
}
