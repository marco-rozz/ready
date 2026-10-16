# ready installer for Windows (PowerShell 5.1+; pwsh 7 works too).
#
#   irm https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.ps1 | iex
#
# Options cannot be passed through a bare pipe — use the scriptblock form:
#   & ([scriptblock]::Create((irm <url>))) -Uninstall
#   & ([scriptblock]::Create((irm <url>))) -Prefix C:\ready\bin
#
# TLS 1.2 must be set BEFORE the first request: Windows PowerShell 5.1 on
# stock .NET fails to reach GitHub otherwise.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

param(
    # Binary install directory (default: %LOCALAPPDATA%\Programs\ready\bin).
    [string]$Prefix,
    # Remove the binary and the PATH entry (user data is kept unless -Purge).
    [switch]$Uninstall,
    # With -Uninstall: also delete the install home (config, transcripts,
    # credentials, skills).
    [switch]$Purge,
    # Skip the purge confirmation.
    [switch]$Yes
)

function Main {
    param([string]$PrefixParam)

    $ErrorActionPreference = 'Stop'

    $repoSlug   = 'marco-rozz/ready'
    $releaseBase = "https://github.com/$repoSlug/releases"
    $binName    = 'ready.exe'

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
    } else {
        $version = 'latest'
        $downloadUrl = "$releaseBase/latest/download/$assetName"
    }
    $checksumUrl = if ($env:VERSION) {
        "$releaseBase/download/$version/checksums.txt"
    } else {
        "$releaseBase/latest/download/checksums.txt"
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

        # ── extract ──────────────────────────────────────────────────────
        $extractDir = Join-Path $tempDir 'extract'
        Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force
        $binSrc = Join-Path $extractDir $binName
        if (-not (Test-Path $binSrc)) {
            throw "the archive is missing $binName (a corrupted or unrelated release?)"
        }

        # ── install home ─────────────────────────────────────────────────
        $installHome  = Join-Path $env:USERPROFILE '.ready'
        $binDir       = if ($PrefixParam) { $PrefixParam } else {
            Join-Path $env:LOCALAPPDATA 'Programs\ready\bin'
        }
        if (-not (Test-Path $binDir)) {
            New-Item -ItemType Directory -Path $binDir -Force | Out-Null
        }

        # Binary first: a failure here leaves library and config untouched.
        $binDest = Join-Path $binDir $binName
        $oldPathInUse = $null
        if (Test-Path $binDest) {
            # A running ready.exe cannot be overwritten or deleted — but it CAN
            # be renamed. Numeric fallback when a previous .old removal failed.
            $oldPath = Join-Path $binDir 'ready.exe.old'
            try {
                if (Test-Path $oldPath) {
                    # best effort (the previous install may have left it in
                    # use)
                    Remove-Item $oldPath -Force -ErrorAction SilentlyContinue | Out-Null
                }
                if (Test-Path $oldPath) {
                    $n = 1
                    while (Test-Path "$oldPath.$n") { $n++ }
                    Move-Item $binDest "$oldPath.$n"
                    $oldPath = "$oldPath.$n"
                } else {
                    Move-Item $binDest $oldPath
                }
                $oldPathInUse = $oldPath
            } catch {
                throw "could not swap in the new $binName (the rename failed: $($_.Exception.Message)) — stop running ready and retry the install"
            }
        }
        Copy-Item $binSrc $binDest -Force

        # Delete the renamed .old best effort (a running process keeps the
        # file open); a leftover is cleaned by the next install.
        if ($oldPathInUse) {
            try { Remove-Item $oldPathInUse -Force -ErrorAction SilentlyContinue | Out-Null } catch { Write-Debug "ready.exe.old left in use; the next install retries the removal" }
        }

        # config copy-if-missing (the operator's — never overwritten).
        $keptConfig = $false
        if (Test-Path (Join-Path $installHome 'config.yaml')) {
            $keptConfig = $true
        } else {
            if (-not (Test-Path $installHome)) {
                New-Item -ItemType Directory -Path $installHome -Force | Out-Null
            }
            Copy-Item (Join-Path $extractDir 'config.example.yaml') (Join-Path $installHome 'config.yaml')
        }

        # library replaced wholesale by a rename-aside swap; runtime skill
        # copies are never touched (the daemon syncs them per file at boot).
        $librarySrc = Join-Path $extractDir 'skills'
        $libraryDst = Join-Path $installHome 'library\skills'
        if (-not (Test-Path (Split-Path $libraryDst))) {
            New-Item -ItemType Directory -Path (Split-Path $libraryDst) -Force | Out-Null
        }
        if (Test-Path $libraryDst) {
            $libraryOld = Join-Path $installHome 'library\skills.old'
            if (Test-Path $libraryOld) {
                Remove-Item $libraryOld -Recurse -Force -ErrorAction SilentlyContinue | Out-Null
            }
            Rename-Item $libraryDst 'skills.old'
        }
        Move-Item $librarySrc $libraryDst

        # ── user PATH (registry): the Windows installer convention ───────
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (($userPath -split ';') -notcontains $binDir) {
            [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $binDir), 'User')
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        }
        if (($userPath -split ';') -notcontains $binDir) {
            Write-Warning "the bin directory did not land in the user PATH: $binDir"
        } else {
            Write-Host "user PATH contains $binDir"
        }
        $env:Path = $env:Path.TrimEnd(';') + ';' + $binDir

        # ── final message ────────────────────────────────────────────────
        try {
            $out = & $binDest --version 2>$null
            Write-Host "installed: $out"
        } catch {
            Write-Host "installed $binName (version check unavailable: run it in a new shell)"
        }
        if ($keptConfig) {
            Write-Host ("kept the existing config at {0}\config.yaml — edit it if needed" -f $installHome)
        } else {
            Write-Host ("created {0}\config.yaml — edit it (provider endpoints, tokens) before first use" -f $installHome)
        }
        Write-Host 'restart ready serve to use the new version'
    } finally {
        Remove-Item $tempDir -Recurse -Force -ErrorAction SilentlyContinue | Out-Null
    }
}

function Uninstall {
    param([switch]$Purge, [switch]$Yes)

    $ErrorActionPreference = 'Stop'

    $installHome = Join-Path $env:USERPROFILE '.ready'
    $binDir      = Join-Path $env:LOCALAPPDATA 'Programs\ready\bin'
    $binDest     = Join-Path $binDir 'ready.exe'

    if (Test-Path $binDest) {
        $oldPath = Join-Path $binDir 'ready.exe.old.uninstall'
        try {
            Rename-Item $binDest 'ready.exe.old.uninstall'
            Remove-Item $oldPath -Force -ErrorAction SilentlyContinue | Out-Null
            if (Test-Path $oldPath) {
                Write-Warning "ready.exe.old.uninstall is in use; delete it manually after stopping ready"
            }
        } catch {
            throw "the binary is locked (rename failed: $($_.Exception.Message)) — stop running ready and retry"
        }
        Write-Host "removed the binary from $binDir"
    } else {
        Write-Host "no binary found at $binDest"
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $newPath = ($userPath -split ';' | Where-Object { $_ -ne $binDir }) -join ';'
    if ($newPath -ne $userPath) {
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Host 'removed the bin directory from the user PATH'
    }

    if ($Purge) {
        if (-not $Yes) {
            $answer = Read-Host "about to delete $installHome (config, session transcripts, OAuth credentials, skills) permanently. Type 'yes' to confirm"
            if ($answer -ne 'yes') {
                throw 'purge aborted (nothing deleted)'
            }
        }
        Remove-Item $installHome -Recurse -Force
        Write-Host "deleted $installHome"
    } else {
        Write-Host "data kept: $installHome holds your config, session transcripts, OAuth credentials and skills"
        Write-Host 'remove it by hand if that is what you want (or suppress with -Uninstall -Purge)'
    }
    Write-Host 'uninstalled.'
}

# Dispatch: the piped invocation has no switches; the scriptblock form is the
# documented way to pass -Uninstall/-Purge/-Yes/-Prefix.
if ($Purge -or $Yes) {
    throw '-PURGE and -YES are only meaningful together with -UNINSTALL'
}
if ($Uninstall) {
    Uninstall -Purge:$Purge -Yes:$Yes
} else {
    Main -Prefix $Prefix
}
