# Ephemeral runner entrypoint (Windows): start the runner with an injected JIT
# config. One job, then exit; multirunner provisions a replacement.
$ErrorActionPreference = 'Stop'

if (-not $env:JIT_CONFIG) {
    Write-Error 'JIT_CONFIG env var is required'
    exit 1
}

Set-Location 'C:\actions-runner'

# The image ships the stock Runner.Worker.dll plus a patched sidecar that stops
# the runner from overriding ACTIONS_RESULTS_URL / ACTIONS_CACHE_URL. Swap it in
# only when multirunner injected a cache redirect; otherwise stock behaviour must
# win so actions/upload-artifact and actions/cache still reach GitHub.
$patched = 'C:\actions-runner\bin\Runner.Worker.dll.mrpatched'
if ($env:ACTIONS_RESULTS_URL -and (Test-Path $patched)) {
    Copy-Item $patched 'C:\actions-runner\bin\Runner.Worker.dll' -Force
}

& .\run.cmd --jitconfig $env:JIT_CONFIG
exit $LASTEXITCODE
