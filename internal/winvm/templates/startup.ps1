# Runtime guest boot task (baked into the golden image, runs at every VM boot).
# Reads the JIT config from the attached config ISO, runs one job, powers off.
# Housekeeping: if the ISO carries rearm.txt, reset the eval clock instead.
$ErrorActionPreference = 'SilentlyContinue'
Start-Transcript -Path 'C:\mr-startup.log' -Force | Out-Null

# The boot task can fire before the CD-ROM (JIT ISO) is mounted, so retry the
# drive scan for a while before giving up.
$jit = $null
$cfgRoot = $null
for ($i = 0; $i -lt 60; $i++) {
    foreach ($d in Get-PSDrive -PSProvider FileSystem) {
        if (Test-Path (Join-Path $d.Root 'rearm.txt')) {
            cscript //nologo C:\Windows\System32\slmgr.vbs /rearm | Out-Null
            Stop-Computer -Force
            return
        }
        $p = Join-Path $d.Root 'jitconfig.txt'
        if (Test-Path $p) { $jit = (Get-Content $p -Raw).Trim(); $cfgRoot = $d.Root }
    }
    if ($jit) { break }
    Start-Sleep -Seconds 2
}
if (-not $jit) {
    Write-Host 'No jitconfig.txt found on any drive; powering off.'
    Stop-Computer -Force
    return
}

# Apply the injected runner env (cache redirect etc.) so run.cmd + actions inherit
# it. Lines are KEY=VAL; the cache URLs already point at the SLIRP host (10.0.2.2).
$envFile = Join-Path $cfgRoot 'runnerenv.txt'
if (Test-Path $envFile) {
    foreach ($line in Get-Content $envFile) {
        $kv = $line -split '=', 2
        if ($kv.Count -eq 2 -and $kv[0]) { Set-Item -Path ("Env:" + $kv[0].Trim()) -Value $kv[1] }
    }
}

# Wait for network before contacting GitHub.
for ($i = 0; $i -lt 60; $i++) {
    if (Test-Connection -Quiet -Count 1 -ComputerName 'github.com') { break }
    Start-Sleep -Seconds 2
}

# Resilience: correct any clock skew before contacting GitHub. The golden bakes
# RealTimeIsUniversal=1, but if the RTC has drifted a skewed clock makes the JIT
# OAuth token "not yet valid" and the runner can't create its broker session.
& tzutil /s 'UTC' 2>$null
try {
    $date = (Invoke-WebRequest 'https://github.com' -Method Head -UseBasicParsing -TimeoutSec 20).Headers.Date
    if ($date) { Set-Date ([DateTime]::Parse($date).ToUniversalTime()) | Out-Null }
} catch {}

Set-Location 'C:\actions-runner'

# The golden bakes the stock Runner.Worker.dll plus a patched sidecar that stops
# the runner from overriding ACTIONS_RESULTS_URL / ACTIONS_CACHE_URL. Swap it in
# only when multirunner injected a cache redirect; otherwise stock behaviour must
# win so actions/upload-artifact and actions/cache still reach GitHub.
$patched = 'C:\actions-runner\bin\Runner.Worker.dll.mrpatched'
if ($env:ACTIONS_RESULTS_URL -and (Test-Path $patched)) {
    Copy-Item $patched 'C:\actions-runner\bin\Runner.Worker.dll' -Force
}

Write-Host 'Starting ephemeral runner...'
& .\run.cmd --jitconfig $jit

# Ephemeral: one job done -> power off, which terminates QEMU.
Stop-Computer -Force
