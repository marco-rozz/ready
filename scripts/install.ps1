# ready installer for Windows (PowerShell 5.1+; pwsh 7 works too).
#
#   irm https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.ps1 | iex
#
# Options cannot be passed through a bare pipe — use the scriptblock form:
#   & ([scriptblock]::Create((irm <url>))) -NoModifyPath
#   & ([scriptblock]::Create((irm <url>))) -Uninstall
#
# TLS 1.2 must be set BEFORE the first request: Windows PowerShell 5.1 on
# stock .NET fails to reach GitHub otherwise.
param(
    # Skip the persistent user-PATH edit (manual instructions are printed
    # either way).
    [switch]$NoModifyPath,
    # Remove the program and the shipped skill library (PATH and agent data
    # are never touched unless -Purge).
    [switch]$Uninstall,
    # With -Uninstall: also delete the default agent directory
    # %USERPROFILE%\.ready (config, transcripts, OAuth credentials, runtime
    # skills) after confirmation; refuses an active daemon/login.
    [switch]$Purge,
    # Skip the purge confirmation.
    [switch]$Yes
)

[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# Per-user paths (per-user-install-agent-isolation, fixed): the exe lives in
# %USERPROFILE%\.local\bin, the shipped library in
# %USERPROFILE%\.local\share\ready\library\skills, the default agent in
# %USERPROFILE%\.ready.
function Get-LauncherPath { Join-Path $env:USERPROFILE '.local\bin\ready.exe' }
function Get-ShareRootPath { Join-Path $env:USERPROFILE '.local\share\ready' }
function Get-BodyPath { Get-LauncherPath } # body == exe, published directly
function Get-LibraryPath { Join-Path (Get-ShareRootPath) 'library\skills' }
function Get-AgentDirPath { Join-Path $env:USERPROFILE '.ready' }

# Is-Elevated reports whether the current process token is elevated
# (Administrator); per-user policy refuses elevation for every mutating mode.
function Test-Elevated {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)

    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# ── registry PATH (raw read, kind and byte preservation, append-only) ────────
# The user PATH is read raw from HKCU\Environment without expanding
# REG_EXPAND_SZ variables; writes preserve the value kind and the original
# bytes, comparing expanded entries case-insensitively against the effective
# PATH and appending only.
function Get-RawUserPath {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
    if (-not $key) { return $null }
    try {
        return $key.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    } finally {
        $key.Close()
    }
}

function Get-UserPathKind {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
    if (-not $key) { return $null }
    try {
        return $key.GetValueKind('Path')
    } catch [System.IO.IOException] {
        return $null # value missing
    } finally {
        $key.Close()
    }
}

function Set-RawUserPath {
    param([string]$Value, [Microsoft.Win32.RegistryValueKind]$Kind)

    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    if (-not $key) { throw 'HKCU\Environment is not available' }
    try {
        if ($Kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString) {
            $key.SetValue('Path', $Value, [Microsoft.Win32.RegistryValueKind]::ExpandString)
        } else {
            $key.SetValue('Path', $Value, [Microsoft.Win32.RegistryValueKind]::String)
        }
    } finally {
        $key.Close()
    }
}

# Split-PathEntries splits a raw PATH value on ';' keeping empty entries.
function Split-PathEntries {
    param([string]$Raw)
    if ($null -eq $Raw) { return @() }

    return $Raw.Split(';')
}

# Test-EntryConfigured compares one raw entry (expanded, case-insensitive)
# against a target directory.
function Test-EntryConfigured {
    param([string]$Entry, [string]$Target)

    try {
        $expanded = [Environment]::ExpandEnvironmentVariables($Entry)
    } catch {
        return $false
    }

    return $expanded.TrimEnd('\') -ieq $Target.TrimEnd('\')
}

# Invoke-MaintenanceHolder runs the ready binary as the EXCLUSIVE
# maintenance-gate holder and waits (bounded) for its readiness marker.
# Returns nothing when the gate is unavailable — it throws with the busy
# explanation so an active purge aborts BEFORE any deletion.
function Invoke-MaintenanceHolder {
    param([string]$BodyPath, [string]$AgentDir, [string]$TempDir)

    $out = Join-Path $TempDir 'maintenance.out'
    if (Test-Path $out) { Remove-Item $out -Force }
    $p = Start-Process -FilePath $BodyPath -ArgumentList @('maintenance', '--agent-dir', $AgentDir) `
        -RedirectStandardOutput $out -RedirectStandardError (Join-Path $TempDir 'maintenance.err') `
        -PassThru -WindowStyle Hidden -NoNewWindow
    try {
        $deadline = (Get-Date).AddSeconds(10)
        while ((Get-Date) -lt $deadline) {
            if ($p.HasExited) {
                $errText = if (Test-Path (Join-Path $TempDir 'maintenance.err')) { Get-Content (Join-Path $TempDir 'maintenance.err') -Raw } else { '' }
                throw "cannot take the exclusive maintenance gate on $AgentDir — a daemon or mutating login appears active; stop it and retry. ($errText)"
            }
            if ((Test-Path $out) -and ((Get-Content $out -Raw -ErrorAction SilentlyContinue) -match 'MAINTENANCE-LOCKED')) {
                return $p
            }
            Start-Sleep -Milliseconds 100
        }
        throw 'the maintenance gate did not become available in time; retry'
    } catch {
        try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch {}
        throw
    }
}

function Main {
    param([switch]$NoModifyPath)

    $ErrorActionPreference = 'Stop'

    if (Test-Elevated) {
        throw 'refusing to run elevated: READY installs per-user into %USERPROFILE% only'
    }

    $repoSlug    = 'marco-rozz/ready'
    $releaseBase = "https://github.com/$repoSlug/releases"
    $binName     = 'ready.exe'

    # ── platform detection (closed table; PROCESSOR_ARCHITEW6432 first) ────
    $winArch = $env:PROCESSOR_ARCHITEW6432
    if ([string]::IsNullOrEmpty($winArch)) {
        $winArch = $env:PROCESSOR_ARCHITECTURE
    }
    switch ($winArch) {
        'AMD64' {
            $assetArch = 'amd64'
        }
        'ARM64' {
            # No native arm64 Windows asset exists today; the amd64 build
            # runs under emulation — say so explicitly.
            $assetArch = 'amd64'
            Write-Host 'NOTE: no native ARM64 windows asset — installing the AMD64 build (it runs under emulation).' -ForegroundColor Yellow
        }
        default {
            throw "unsupported Windows architecture '$winArch' — supported: AMD64 (x64)"
        }
    }

    # ── release URL prefix (latest vs VERSION pin; no API call) ───────────
    $assetName = "ready_windows_$assetArch.zip"
    if ($env:VERSION) {
        if (-not $env:VERSION.StartsWith('v')) {
            throw "VERSION must be a tag name (v…), got: $env:VERSION"
        }
        $version = $env:VERSION
        $downloadUrl = "$releaseBase/download/$version/$assetName"
        $checksumUrl = "$releaseBase/download/$version/checksums.txt"
    } else {
        $downloadUrl = "$releaseBase/latest/download/$assetName"
        $checksumUrl = "$releaseBase/latest/download/checksums.txt"
    }

    # ── download + SHA256 verify ──────────────────────────────────────────
    if (-not (Get-Command Invoke-RestMethod -ErrorAction SilentlyContinue)) {
        throw 'Invoke-RestMethod is not available in this PowerShell.'
    }

    $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("ready-install-" + [System.Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
    try {
        $archivePath = Join-Path $tempDir $assetName
        Invoke-WebRequest -Uri $downloadUrl -OutFile $archivePath -UseBasicParsing | Out-Null

        Invoke-WebRequest -Uri $checksumUrl -UseBasicParsing -OutFile (Join-Path $tempDir 'checksums.txt') | Out-Null
        $want = (Get-Content (Join-Path $tempDir 'checksums.txt')) | ForEach-Object {
            $parts = ($_ -split '\s+', 2)
            if ($parts[1].Trim() -eq $assetName) { $parts[0] }
        } | Select-Object -First 1
        if ([string]::IsNullOrEmpty($want)) {
            throw "checksums.txt does not list $assetName"
        }
        $got = (Get-FileHash -Algorithm SHA256 -Path $archivePath).Hash.ToLower()
        if ($got -ne $want.ToLower()) {
            throw "SHA256 mismatch for $assetName (want $want, got $got) — the release may have changed between the two downloads; retry"
        }

        # ── extract and validate bundle members ──────────────────────────
        $extractDir = Join-Path $tempDir 'extract'
        Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force
        $binSrc = Join-Path $extractDir $binName
        if (-not (Test-Path $binSrc)) {
            throw "the archive is missing $binName (a corrupted or unrelated release?)"
        }
        if (-not (Test-Path (Join-Path $extractDir 'skills'))) {
            throw 'the archive is missing the skills library (a corrupted or unrelated release?)'
        }
        if (-not (Test-Path (Join-Path $extractDir 'config.example.yaml'))) {
            throw 'the archive is missing config.example.yaml (a corrupted or unrelated release?)'
        }

        # ── placement (program → library → default config → PATH) ────────
        $agentDir  = Get-AgentDirPath
        $launcher  = Get-LauncherPath
        $library   = Get-LibraryPath
        $binDir    = Split-Path $launcher -Parent

        if (-not (Test-Path $binDir)) {
            New-Item -ItemType Directory -Path $binDir -Force | Out-Null
        }

        # Program first: a running ready.exe cannot be overwritten or deleted —
        # but it CAN be renamed. Numeric fallback when a previous .old removal
        # failed; a failed placement restores the old path.
        $oldPathInUse = $null
        if (Test-Path $launcher) {
            $oldPath = Join-Path $binDir 'ready.exe.old'
            try {
                if (Test-Path $oldPath) {
                    # best effort (the previous install may have left it in use)
                    Remove-Item $oldPath -Force -ErrorAction SilentlyContinue | Out-Null
                }
                if (Test-Path $oldPath) {
                    $n = 1
                    while (Test-Path "$oldPath.$n") { $n++ }
                    Move-Item $launcher "$oldPath.$n"
                    $oldPath = "$oldPath.$n"
                } else {
                    Move-Item $launcher $oldPath
                }
                $oldPathInUse = $oldPath
            } catch {
                throw "could not swap in the new $binName (the rename failed: $($_.Exception.Message)) — stop running ready and retry the install"
            }
        }
        try {
            Copy-Item $binSrc $launcher -Force
        } catch {
            # Restore the old executable name when placement fails.
            if ($oldPathInUse -and -not (Test-Path $launcher)) {
                Move-Item $oldPathInUse $launcher -ErrorAction SilentlyContinue
            }
            throw "failed to place the new executable: $($_.Exception.Message)"
        }

        # Delete the renamed .old best effort (a running process keeps the
        # file open); a leftover is cleaned by the next install.
        if ($oldPathInUse) {
            try { Remove-Item $oldPathInUse -Force -ErrorAction SilentlyContinue | Out-Null } catch { Write-Debug 'ready.exe.old left in use; the next install retries the removal' }
        }

        # library: installer-owned, replaced by a rename-aside swap; runtime
        # skill copies are never touched. A failed publication restores the
        # previous library.
        $libraryParent = Split-Path $library -Parent
        if (-not (Test-Path $libraryParent)) {
            New-Item -ItemType Directory -Path $libraryParent -Force | Out-Null
        }
        $libraryOld = Join-Path $libraryParent 'skills.old'
        $swappedAside = $false
        if (Test-Path $library) {
            if (Test-Path $libraryOld) {
                Remove-Item $libraryOld -Recurse -Force -ErrorAction SilentlyContinue | Out-Null
            }
            Rename-Item $library 'skills.old'
            $swappedAside = $true
        }
        try {
            Move-Item (Join-Path $extractDir 'skills') $library
        } catch {
            if ($swappedAside -and -not (Test-Path $library)) {
                Rename-Item $libraryOld 'skills' -ErrorAction SilentlyContinue
            }
            throw "failed to publish the skill library: $($_.Exception.Message)"
        }
        if (Test-Path $libraryOld) {
            Remove-Item $libraryOld -Recurse -Force -ErrorAction SilentlyContinue | Out-Null
        }

        # The example is program content. Only ready init creates an agent
        # configuration; installation must not pre-empt its first-run wizard.
        $examplePath = Join-Path (Get-ShareRootPath) 'config.example.yaml'
        Copy-Item (Join-Path $extractDir 'config.example.yaml') $examplePath -Force

        # ── user PATH (registry): raw read, kind/byte preservation ───────
        $pathOutcome = Invoke-PathHandling -BinDir $binDir -NoModifyPath:$NoModifyPath

        # ── final message ─────────────────────────────────────────────────
        try {
            $out = & $launcher --version 2>$null
            Write-Host "installed: $out"
        } catch {
            Write-Host "installed $binName (version check unavailable: run it in a new shell)"
        }
        Write-Host "program: $launcher"
        Write-Host "shipped skill library: $library (installer-owned; each agent keeps its own runtime copy)"
        Write-Host "reference configuration: $examplePath"
        Write-Host "PATH: $pathOutcome"
        if (Test-Path (Join-Path $agentDir 'config.yaml')) {
            Write-Host ("kept the existing config at {0}\config.yaml" -f $agentDir)
            Write-Host 'For offline configuration changes, run: ready.exe init'
            Write-Host 'restart ready.exe serve to use the new version'
        } else {
            Write-Host 'No agent configuration was created by the installer.'
            Write-Host 'Next: open a new terminal (apply the PATH instructions above if needed), then run:'
            Write-Host '  ready.exe init'
            Write-Host ("The wizard will create {0}\config.yaml and print your server and chat commands." -f $agentDir)
        }
    } finally {
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue | Out-Null
    }
}

function Invoke-PathHandling {
    param([string]$BinDir, [switch]$NoModifyPath)

    if ($NoModifyPath) {
        Write-Host 'manual PATH: add the bin directory to your user PATH (System Properties → Environment Variables) or:'
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', (`$env:Path + ';$BinDir'), 'User')"

        return 'skipped (-NoModifyPath)'
    }

    # Effective PATH reachability.
    $effective = ($env:Path -split ';') | ForEach-Object { $_.TrimEnd('\') }
    if ($effective -icontains $BinDir.TrimEnd('\')) {
        Write-Host 'manual PATH reference: add the bin directory to your user PATH (System Properties → Environment Variables) if it is missing there.'

        return 'already reachable in the current PATH'
    }

    $raw = Get-RawUserPath
    $kind = Get-UserPathKind
    if ($null -ne $raw) {
        # Configured already? Expanded, case-insensitive entry comparison.
        $configured = (Split-PathEntries -Raw $raw | Where-Object { Test-EntryConfigured -Entry $_ -Target $BinDir })
        if ($configured) {
            Write-Host 'manual PATH reference: add the bin directory to your user PATH if it is missing there.'

            return 'configured (user registry PATH)'
        }

        # Append-only, preserving the value kind and the original bytes.
        $newRaw = $raw.TrimEnd(';') + ';' + $BinDir
        $useKind = if ($kind) { $kind } else { [Microsoft.Win32.RegistryValueKind]::String }
        Set-RawUserPath -Value $newRaw -Kind $useKind
        Write-Host 'manual PATH reference: already appended; open a new shell to apply.'

        return "appended to the user registry PATH (kind $useKind preserved)"
    }

    # Missing PATH value: create a String-typed one with the single entry.
    Set-RawUserPath -Value $BinDir -Kind ([Microsoft.Win32.RegistryValueKind]::String)
    Write-Host 'manual PATH reference: created the missing user PATH value; open a new shell to apply.'

    return 'created the user registry PATH'
}

function Uninstall {
    param([switch]$Purge, [switch]$Yes)

    $ErrorActionPreference = 'Stop'

    if (Test-Elevated) {
        throw 'refusing to run elevated: READY installs per-user into %USERPROFILE% only'
    }

    $agentDir = Get-AgentDirPath
    $launcher = Get-LauncherPath
    $shareRoot = Get-ShareRootPath
    $library = Get-LibraryPath

    if ($Purge) {
        if (-not $Yes) {
            $answer = Read-Host "about to delete $agentDir (config, session transcripts, OAuth credentials, runtime skills) permanently. Type 'yes' to confirm"
            if ($answer -ne 'yes') {
                throw 'purge aborted (nothing deleted)'
            }
        }

        # Exclusive maintenance gate BEFORE any deletion: a busy daemon/login
        # aborts everything untouched.
        $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("ready-purge-" + [System.Guid]::NewGuid())
        New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
        try {
            if (-not (Test-Path $agentDir)) {
                throw "no default agent directory at $agentDir — nothing to purge"
            }
            if ((Get-Item $agentDir -Force).LinkType) {
                throw "refusing to purge $agentDir: it is a link (the real directory it points to is never traversed for deletion)"
            }
            $holder = Invoke-MaintenanceHolder -BodyPath $launcher -AgentDir $agentDir -TempDir $tempDir
            try {
                # Forget the Store key's vault entry (Windows Credential
                # Manager) before the directory and the program disappear.
                # Best-effort: a key kept in the agent directory goes with it.
                try {
                    & $launcher secrets forget-key --agent-dir $agentDir --yes *> $null
                    if ($LASTEXITCODE -ne 0) { throw 'forget-key failed' }
                } catch {
                    Write-Warning 'the store key could not be forgotten automatically; if it kept its key in the OS vault, remove the "ready" credential named after the agent id by hand'
                }
                # Config first: no new daemon can boot without it.
                if (Test-Path (Join-Path $agentDir 'config.yaml')) {
                    Remove-Item (Join-Path $agentDir 'config.yaml') -Force
                }
                Remove-Item $agentDir -Recurse -Force
                Write-Host "deleted the default agent directory $agentDir"
            } finally {
                try { Stop-Process -Id $holder.Id -Force -ErrorAction SilentlyContinue } catch {}
            }
        } finally {
            Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue | Out-Null
        }
    }

    if (Test-Path $launcher) {
        $binDir = Split-Path $launcher -Parent
        $oldPath = Join-Path $binDir 'ready.exe.old.uninstall'
        try {
            Rename-Item $launcher 'ready.exe.old.uninstall'
            Remove-Item $oldPath -Force -ErrorAction SilentlyContinue | Out-Null
            if (Test-Path $oldPath) {
                Write-Warning 'ready.exe.old.uninstall is in use; delete it manually after stopping ready'
            }
        } catch {
            throw "the binary is locked (rename failed: $($_.Exception.Message)) — stop running ready and retry"
        }
        Write-Host "removed the program $launcher"
    } else {
        Write-Host "no program found at $launcher"
    }

    if ($Purge) {
        if (Test-Path $shareRoot) {
            Remove-Item $shareRoot -Recurse -Force
            Write-Host "removed the shipped skill library $library"
        }
        Write-Host 'explicit agent directories and PATH entries remain untouched'
        Write-Host 'uninstalled.'
    } else {
        if (Test-Path $shareRoot) {
            Remove-Item $shareRoot -Recurse -Force
            Write-Host "removed the shipped skill library $library"
        }
        Write-Host "agent data kept: $agentDir holds your config, session transcripts, OAuth credentials and runtime skills"
        Write-Host 'explicit --agent-dir directories are never discovered or removed'
        Write-Host 'PATH entries were never touched'
        Write-Host 'uninstalled.'
    }
}

# Dispatch: the piped invocation has no switches; the scriptblock form is the
# documented way to pass -NoModifyPath/-Uninstall/-Purge/-Yes.
if ($Purge -and -not $Uninstall) {
    throw '-PURGE is only meaningful together with -UNINSTALL'
}
if ($Uninstall) {
    Uninstall -Purge:$Purge -Yes:$Yes
} else {
    Main -NoModifyPath:$NoModifyPath
}
