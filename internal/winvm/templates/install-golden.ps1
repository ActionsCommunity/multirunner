# Bake-time provisioning (runs once during the golden install, from the
# autounattend ISO). Installs the runner, applies the cache-redirect patch,
# registers the boot task, then powers off -> the disk becomes the golden image.
# Placeholder __RUNNER_VERSION__ is substituted by `multirunner bake`.
#
# Progress is written to COM1 (captured by the bake's `-serial file:` log) so the
# host can verify the bake actually finished without mounting the guest disk. The
# bake REQUIRES the final "MR:GOLDEN_OK" marker; anything else is a failed bake.
$ErrorActionPreference = 'Stop'
$ver = '__RUNNER_VERSION__'

function Mark($m) { try { cmd /c "echo MR:$m>COM1" | Out-Null } catch {} }
Mark 'install-golden start'

try {
    # Locate this script's source ISO (to copy startup.ps1 from the same media).
    $src = $null
    foreach ($d in Get-PSDrive -PSProvider FileSystem) {
        if (Test-Path (Join-Path $d.Root 'startup.ps1')) { $src = $d.Root; break }
    }

    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    New-Item -ItemType Directory -Force C:\actions-runner | Out-Null

    # fetchOrStage prefers a copy staged on the autounattend CD (the host fetched
    # it fast); only if it's absent does it download (the VM's user-mode network is
    # slow/flaky, so the CD is the primary path).
    function Assert-PayloadHash($name, $dest, $algorithm, $expected) {
        $actual = (Get-FileHash $dest -Algorithm $algorithm).Hash
        if ($actual -ne $expected) {
            Remove-Item $dest -Force -ErrorAction SilentlyContinue
            throw "$name $algorithm mismatch: got $actual"
        }
    }

    function FetchOrStage($name, $url, $dest, $algorithm, $expected) {
        if ($src -and (Test-Path (Join-Path $src $name))) {
            try {
                Copy-Item (Join-Path $src $name) $dest -Force
                # The CD source is read-only; Copy-Item carries that attribute over, and
                # Expand-Archive/Remove-Item then fail with "insufficient access rights".
                Set-ItemProperty $dest -Name IsReadOnly -Value $false -Force
                Assert-PayloadHash $name $dest $algorithm $expected
                Mark "$name staged from CD"
                return
            }
            catch { Mark "$name staged copy rejected: $($_.Exception.Message)" }
        }
        for ($i = 0; $i -lt 30; $i++) {
            try {
                Invoke-WebRequest $url -OutFile $dest -UseBasicParsing -TimeoutSec 300
                Assert-PayloadHash $name $dest $algorithm $expected
                Mark "$name downloaded"
                return
            }
            catch { Mark "$name retry $i $($_.Exception.Message)"; Start-Sleep -Seconds 10 }
        }
        throw "$name unavailable (no CD copy and download failed)"
    }

    $url = "https://github.com/actions/runner/releases/download/v$ver/actions-runner-win-x64-$ver.zip"
    FetchOrStage 'runner.zip' $url C:\runner.zip SHA256 '__RUNNER_SHA256__'
    Expand-Archive C:\runner.zip C:\actions-runner -Force
    Remove-Item C:\runner.zip
    Mark 'runner extracted'

    # Install MinGit (portable Git for Windows). Required so actions/checkout uses
    # real git (incremental fetch + the dotgit-cache bundle seed) instead of the
    # REST API full-archive download, and so `run:` steps and the job hook can run
    # git. Prepended to the machine PATH so every process (runner, hook, job) sees it.
    $gitUrl = '__MINGIT_URL__'
    FetchOrStage 'mingit.zip' $gitUrl C:\mingit.zip SHA256 '__MINGIT_SHA256__'
    Expand-Archive C:\mingit.zip C:\mingit -Force
    Remove-Item C:\mingit.zip
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    [Environment]::SetEnvironmentVariable('Path', 'C:\mingit\cmd;' + $machinePath, 'Machine')
    Mark 'mingit installed'

    # Cache-redirect patch, kept as a SIDECAR rather than applied in place.
    # Renaming the ACTIONS_RESULTS_URL / ACTIONS_CACHE_URL UTF-16 literals
    # (last char L->X) stops the runner overriding them for `uses:` actions, which
    # is only correct when multirunner injects replacements (cache.enabled).
    # Patching the live DLL leaves ACTIONS_RESULTS_URL unset whenever the cache is
    # off and breaks actions/upload-artifact, so startup.ps1 swaps this copy in
    # only when a redirect is actually present.
    $dll = 'C:\actions-runner\bin\Runner.Worker.dll'
    $patched = $dll + '.mrpatched'
    $bytes = [IO.File]::ReadAllBytes($dll)
    function Set-LastCharX([byte[]]$b, [string]$s) {
        $p = [Text.Encoding]::Unicode.GetBytes($s)
        for ($i = 0; $i -le $b.Length - $p.Length; $i++) {
            $m = $true
            for ($j = 0; $j -lt $p.Length; $j++) { if ($b[$i + $j] -ne $p[$j]) { $m = $false; break } }
            if ($m) { $b[$i + $p.Length - 2] = [byte][char]'X'; return }
        }
    }
    Set-LastCharX $bytes 'ACTIONS_RESULTS_URL'
    Set-LastCharX $bytes 'ACTIONS_CACHE_URL'
    [IO.File]::WriteAllBytes($patched, $bytes)
    Mark 'cache patch staged'

    [Environment]::SetEnvironmentVariable('RUNNER_DISABLE_AUTOUPDATE', '1', 'Machine')

    # Make Windows read the emulated hardware clock as UTC (QEMU presents UTC).
    # Otherwise the guest adds its local-TZ offset on top, skewing the clock by
    # hours, which makes the runner's JIT OAuth token "not valid until <future>"
    # so the broker session fails and the runner exits before listening for jobs.
    Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\TimeZoneInformation' -Name RealTimeIsUniversal -Value 1 -Type DWord -Force
    & tzutil /s 'UTC'
    Mark 'clock set to UTC'

    # Don't let the admin password expire (would break any future autologon).
    & net accounts /maxpwage:unlimited | Out-Null

    # Install the boot task that runs the ephemeral runner. LogonType must be
    # ServiceAccount (S4U): the default for -User SYSTEM is InteractiveToken,
    # which only fires when a user is logged on interactively — at boot there is
    # no interactive session, so the task would never run.
    if ($src) {
        Copy-Item (Join-Path $src 'startup.ps1') C:\actions-runner\startup.ps1 -Force
        # dotgit-cache job-started hook (no-op unless MR_GIT_BUNDLE_URL is injected).
        if (Test-Path (Join-Path $src 'githook.ps1')) { Copy-Item (Join-Path $src 'githook.ps1') C:\mr-githook.ps1 -Force }
    }
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' `
        -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\actions-runner\startup.ps1'
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    Register-ScheduledTask -TaskName 'multirunner' -Action $action -Trigger $trigger `
        -Principal $principal -Force | Out-Null
    Mark 'task registered'

    # Optional toolchains baked into the golden (multirunner bake --tools ...).
    # __TOOLS__ is a comma list substituted by the host (empty => none). Big
    # single-file payloads are staged on the CD by the host; FetchOrStage uses
    # them, falling back to a slow in-guest download.
    function Add-MachinePath($p) {
        $mp = [Environment]::GetEnvironmentVariable('Path', 'Machine')
        if ($mp -notlike "*$p*") { [Environment]::SetEnvironmentVariable('Path', "$p;$mp", 'Machine') }
    }
    $tools = '__TOOLS__'.Split(',') | ForEach-Object { $_.Trim() } | Where-Object { $_ }
    foreach ($t in $tools) {
        switch ($t) {
            'node' {
__NODE_INSTALLS__
                Mark 'node installed'
            }
            'go' {
                FetchOrStage 'go.zip' '__GO_URL__' C:\go.zip SHA256 '__GO_SHA256__'
                Expand-Archive C:\go.zip C:\ -Force   # extracts C:\go
                Remove-Item C:\go.zip
                Add-MachinePath 'C:\go\bin'
                Mark 'go installed'
            }
            'dotnet' {
                New-Item -ItemType Directory -Force C:\dotnet | Out-Null
__DOTNET_INSTALLS__
                [Environment]::SetEnvironmentVariable('DOTNET_ROOT', 'C:\dotnet', 'Machine')
                [Environment]::SetEnvironmentVariable('DOTNET_CLI_TELEMETRY_OPTOUT', '1', 'Machine')
                Add-MachinePath 'C:\dotnet'
                Mark 'dotnet installed'
            }
            'buildtools' {
__BUILDTOOLS_INSTALLS__
                Mark 'buildtools installed'
            }
            default { Mark "unknown tool $t" }
        }
    }

    Mark 'GOLDEN_OK'

    # SetupComplete.cmd runs while Setup is still finishing (the image only
    # finalizes to IMAGE_STATE_COMPLETE after this returns and the autologon
    # session loads). Powering off here would freeze the golden mid-OOBE, so
    # every runtime boot would re-run OOBE and die before the runner task fires.
    # Instead spawn a DETACHED poller: wait for Setup to finish
    # (SystemSetupInProgress -> 0), give it a moment, then power off. The bake
    # then captures a COMPLETE, normally-booting golden.
    $poller = 'try { while ((Get-ItemProperty HKLM:\SYSTEM\Setup -Name SystemSetupInProgress -EA Stop).SystemSetupInProgress -ne 0) { Start-Sleep 5 } } catch {}; Start-Sleep 25; cmd /c "echo MR:finalized>COM1"; Stop-Computer -Force'
    Start-Process powershell -WindowStyle Hidden -ArgumentList '-NoProfile', '-Command', $poller
} catch {
    Mark "FATAL L$($_.InvocationInfo.ScriptLineNumber) $($_.Exception.Message)"
    Stop-Computer -Force
}
