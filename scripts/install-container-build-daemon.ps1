#Requires -Version 7.4
#Requires -RunAsAdministrator

[CmdletBinding()]
param(
    [ValidateRange(1024, 65535)]
    [int]$Port = 23760,

    [string]$ContainerName = 'multirunner-container-build-docker',

    [string]$CertificateDirectory = 'C:\multirunner\container-build\tls',

    [string]$CertificateVolume = 'multirunner-container-build-certs',

    [string]$DataVolume = 'multirunner-container-build-data',

    [string]$WorkspaceDirectory = 'C:\multirunner\container-build\workspaces',

    [string[]]$WorkFolders = @('_work'),

    [string]$ServiceName = 'multirunner',

    [string]$SourceDirectory = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path,

    [string]$RunnerImage = 'multirunner/runner-linux:container-build',

    [switch]$Replace,

    [switch]$RotateCertificates,

    [switch]$SkipImageBuild
)

$ErrorActionPreference = 'Stop'

$Image = 'docker:29.8.0-dind@sha256:5efed980cba3fc126cf54e21a5a6ff8849d05b6e0623d6e7612f48e9cd6cd17e'
$DockerHost = "tcp://127.0.0.1:$Port"
$ClientCertificates = @{
    CA   = Join-Path $CertificateDirectory 'ca.pem'
    Cert = Join-Path $CertificateDirectory 'cert.pem'
    Key  = Join-Path $CertificateDirectory 'key.pem'
}

function Invoke-Docker {
    param([Parameter(Mandatory)][string[]]$Arguments)

    $output = & docker @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "docker $($Arguments -join ' ') failed: $([string]::Join("`n", @($output)))"
    }
    return @($output)
}

function Test-DockerResource {
    param(
        [Parameter(Mandatory)][ValidateSet('container', 'volume')][string]$Type,
        [Parameter(Mandatory)][string]$Name
    )

    & docker $Type inspect $Name *> $null
    return $LASTEXITCODE -eq 0
}

function Initialize-Volume {
    param([Parameter(Mandatory)][string]$Name)

    if (-not (Test-DockerResource -Type volume -Name $Name)) {
        Invoke-Docker -Arguments @('volume', 'create', $Name) | Out-Null
    }
}

function Wait-ForDaemon {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][int]$TimeoutSeconds
    )

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        Start-Sleep -Seconds 1
        $running = (& docker container inspect $Name --format '{{.State.Running}}' 2>$null) -eq 'true'
        if (-not $running) {
            continue
        }
        & docker exec $Name test -s /certs/client/key.pem *> $null
        $certReady = $LASTEXITCODE -eq 0
        if ($certReady) {
            return
        }
    } while ((Get-Date) -lt $deadline)

    $logs = & docker logs $Name 2>&1
    throw "Container-build Docker daemon did not become ready: $([string]::Join("`n", @($logs)))"
}

function Initialize-WorkspaceDirectory {
    param(
        [Parameter(Mandatory)][string]$Directory,
        [Parameter(Mandatory)][string[]]$Folders
    )

    New-Item -ItemType Directory -Path $Directory -Force | Out-Null
    $currentUserSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    & takeown.exe /F $Directory /A /R /D Y | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Could not take ownership of workspace directory $Directory"
    }
    & icacls.exe $Directory /inheritance:r `
        /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' `
        "*${currentUserSID}:(OI)(CI)M" /C /Q | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Could not secure workspace directory $Directory"
    }

    foreach ($folder in $Folders) {
        if ([string]::IsNullOrWhiteSpace($folder) -or
            $folder -in @('.', '..') -or
            $folder.Trim() -ne $folder -or
            $folder -match '[/\\]') {
            throw "Work folder must be one relative directory name: $folder"
        }
        New-Item -ItemType Directory -Path (Join-Path $Directory $folder) -Force | Out-Null
    }
}

function Export-ClientCertificateSet {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Destination
    )

    $staging = Join-Path ([IO.Path]::GetTempPath()) "multirunner-container-build-$PID"
    if (Test-Path -LiteralPath $staging) {
        Remove-Item -LiteralPath $staging -Recurse -Force
    }
    New-Item -ItemType Directory -Path $staging | Out-Null
    try {
        Invoke-Docker -Arguments @('cp', "${Name}:/certs/client/.", $staging) | Out-Null
        foreach ($file in @('ca.pem', 'cert.pem', 'key.pem')) {
            $source = Join-Path $staging $file
            if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
                throw "Docker daemon did not generate client certificate $file"
            }
        }

        New-Item -ItemType Directory -Path $Destination -Force | Out-Null
        $currentUserSID = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        & takeown.exe /F $Destination /A /R /D Y | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Could not take ownership of certificate directory $Destination"
        }
        & icacls.exe $Destination /inheritance:r `
            /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' `
            "*${currentUserSID}:(OI)(CI)F" /C /Q | Out-Null
        if ($LASTEXITCODE -ne 0) {
            throw "Could not prepare certificate ACLs on $Destination"
        }
        foreach ($file in @('ca.pem', 'cert.pem', 'key.pem')) {
            $target = Join-Path $Destination $file
            if (Test-Path -LiteralPath $target) {
                & icacls.exe $target /inheritance:r `
                    /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F' "*${currentUserSID}:F" `
                    /C /Q | Out-Null
                if ($LASTEXITCODE -ne 0) {
                    throw "Could not recover certificate ACL on $target"
                }
            }
            Copy-Item -LiteralPath (Join-Path $staging $file) -Destination $target -Force
            & icacls.exe $target /inheritance:r `
                /grant:r '*S-1-5-18:F' '*S-1-5-32-544:F' "*${currentUserSID}:F" `
                /C /Q | Out-Null
            if ($LASTEXITCODE -ne 0) {
                throw "Could not restrict certificate ACL on $target"
            }
        }
    }
    finally {
        if (Test-Path -LiteralPath $staging) {
            Remove-Item -LiteralPath $staging -Recurse -Force
        }
    }
}

function Get-RunnerImageID {
    param(
        [Parameter(Mandatory)][string]$ImageName,
        [Parameter(Mandatory)][string]$HostEndpoint,
        [Parameter(Mandatory)][hashtable]$Certificates
    )

    $imageID = @(Invoke-Docker -Arguments @(
        '--tlsverify',
        '--tlscacert', $Certificates.CA,
        '--tlscert', $Certificates.Cert,
        '--tlskey', $Certificates.Key,
        '--host', $HostEndpoint,
        'image', 'inspect',
        '--format', '{{.Id}}',
        $ImageName
    ))
    if ($imageID.Count -ne 1 -or $imageID[0] -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Unexpected runner image ID: $($imageID -join ', ')"
    }
    return $imageID[0]
}

function Build-RunnerImage {
    param(
        [Parameter(Mandatory)][string]$Source,
        [Parameter(Mandatory)][string]$ImageName,
        [Parameter(Mandatory)][string]$HostEndpoint,
        [Parameter(Mandatory)][hashtable]$Certificates
    )

    $dockerfiles = @(
        'images\linux\Dockerfile',
        'images\linux\flavors\native-build.Dockerfile',
        'images\linux\flavors\node.Dockerfile'
    )
    foreach ($relativePath in $dockerfiles) {
        if (-not (Test-Path -LiteralPath (Join-Path $Source $relativePath) -PathType Leaf)) {
            throw "Missing runner image input: $relativePath"
        }
    }

    $tls = @(
        '--tlsverify',
        '--tlscacert', $Certificates.CA,
        '--tlscert', $Certificates.Cert,
        '--tlskey', $Certificates.Key,
        '--host', $HostEndpoint
    )
    $minimal = "$ImageName-minimal"
    $native = "$ImageName-native-build"

    Invoke-Docker -Arguments ($tls + @(
        'build',
        '--file', (Join-Path $Source 'images\linux\Dockerfile'),
        '--tag', $minimal,
        $Source
    )) | Out-Null
    Invoke-Docker -Arguments ($tls + @(
        'build',
        '--file', (Join-Path $Source 'images\linux\flavors\native-build.Dockerfile'),
        '--build-arg', "PARENT=$minimal",
        '--tag', $native,
        $Source
    )) | Out-Null
    Invoke-Docker -Arguments ($tls + @(
        'build',
        '--file', (Join-Path $Source 'images\linux\flavors\node.Dockerfile'),
        '--build-arg', "PARENT=$native",
        '--tag', $ImageName,
        $Source
    )) | Out-Null

    return Get-RunnerImageID `
        -ImageName $ImageName `
        -HostEndpoint $HostEndpoint `
        -Certificates $Certificates
}

$containerExists = Test-DockerResource -Type container -Name $ContainerName
if ($RotateCertificates -and -not $Replace) {
    throw '-RotateCertificates requires -Replace.'
}
if ($containerExists) {
    if ($Replace) {
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($null -ne $service -and $service.Status -ne 'Stopped') {
            throw "Windows service '$ServiceName' must be stopped before replacing the container-build daemon."
        }
        $controllers = @(
            Get-CimInstance Win32_Process -Filter "Name='multirunner.exe'" |
                Where-Object { $_.ProcessId -ne $PID }
        )
        if ($controllers.Count -ne 0) {
            throw "Found $($controllers.Count) running multirunner controller process(es); stop them before replacement."
        }
        $activeChildrenOutput = @(& docker exec $ContainerName docker ps --quiet 2>&1)
        if ($LASTEXITCODE -ne 0) {
            throw "Could not inspect active container-build children: $([string]::Join("`n", $activeChildrenOutput))"
        }
        $activeChildren = @($activeChildrenOutput | Where-Object { $_ -is [string] -and $_.Length -gt 0 })
        if ($activeChildren.Count -ne 0) {
            throw "Container-build daemon has $($activeChildren.Count) active child container(s); drain the pool before replacement."
        }
        Invoke-Docker -Arguments @('container', 'rm', '--force', $ContainerName) | Out-Null
        $containerExists = $false
    }
    elseif ((Invoke-Docker -Arguments @('container', 'inspect', '--format', '{{.State.Running}}', $ContainerName))[0] -ne 'true') {
        Invoke-Docker -Arguments @('container', 'start', $ContainerName) | Out-Null
    }
}
if ($RotateCertificates -and (Test-DockerResource -Type volume -Name $CertificateVolume)) {
    Invoke-Docker -Arguments @('volume', 'rm', $CertificateVolume) | Out-Null
}

Initialize-Volume -Name $CertificateVolume
Initialize-Volume -Name $DataVolume
Initialize-WorkspaceDirectory -Directory $WorkspaceDirectory -Folders $WorkFolders

if (-not $containerExists) {
    Invoke-Docker -Arguments @(
        'run', '--detach',
        '--name', $ContainerName,
        '--label', 'multirunner.container-build-daemon=true',
        '--restart', 'unless-stopped',
        '--privileged',
        '--env', 'DOCKER_TLS_CERTDIR=/certs',
        '--publish', "127.0.0.1:${Port}:2376",
        '--volume', "${CertificateVolume}:/certs",
        '--volume', "${DataVolume}:/var/lib/docker",
        '--volume', "${WorkspaceDirectory}:/home/runner",
        $Image
    ) | Out-Null
}

Wait-ForDaemon -Name $ContainerName -TimeoutSeconds 90
$workspaceMount = @(
    Invoke-Docker -Arguments @(
        'container', 'inspect',
        '--format', '{{range .Mounts}}{{if eq .Destination "/home/runner"}}{{.Source}}{{end}}{{end}}',
        $ContainerName
    )
)
if ($workspaceMount.Count -ne 1 -or [string]::IsNullOrWhiteSpace($workspaceMount[0])) {
    throw "Container $ContainerName has no /home/runner workspace mount. Re-run with -Replace."
}
Export-ClientCertificateSet -Name $ContainerName -Destination $CertificateDirectory

$version = @(Invoke-Docker -Arguments @(
    '--tlsverify',
    '--tlscacert', $ClientCertificates.CA,
    '--tlscert', $ClientCertificates.Cert,
    '--tlskey', $ClientCertificates.Key,
    '--host', $DockerHost,
    'version',
    '--format', '{{.Server.Version}}'
))
if ($version.Count -ne 1 -or $version[0] -ne '29.8.0') {
    throw "Unexpected container-build Docker daemon version: $($version -join ', ')"
}

if (-not $SkipImageBuild) {
    $runnerImageID = Build-RunnerImage `
        -Source $SourceDirectory `
        -ImageName $RunnerImage `
        -HostEndpoint $DockerHost `
        -Certificates $ClientCertificates
}
else {
    $runnerImageID = Get-RunnerImageID `
        -ImageName $RunnerImage `
        -HostEndpoint $DockerHost `
        -Certificates $ClientCertificates
}

Write-Output "container=$ContainerName"
Write-Output "image=$Image"
Write-Output "docker_host=$DockerHost"
Write-Output "certificates=$CertificateDirectory"
Write-Output "workspaces=$WorkspaceDirectory"
Write-Output "work_folders=$($WorkFolders -join ',')"
Write-Output "runner_image=$RunnerImage"
Write-Output "runner_image_id=$runnerImageID"
Write-Output 'status=ready'
