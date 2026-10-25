# Install hermes-go from a GitHub release on Windows.
#
#   irm https://raw.githubusercontent.com/umi008/hermes-agent-go/main/scripts/install.ps1 | iex
#
# Environment:
#   HERMES_GO_VERSION         release tag to install, e.g. v0.2.0 (default: latest)
#   HERMES_GO_INSTALL_DIR     target directory (default: %LOCALAPPDATA%\hermes-go\bin)
#   HERMES_GO_DOWNLOAD_URL    base URL of the release assets (default: GitHub)
#   HERMES_GO_NO_MODIFY_PATH  set to 1 to leave the user PATH alone
#
# The archive is verified against the release's checksums.txt before
# anything is installed. Errors throw instead of calling exit, so a failed
# install never closes the shell that ran `iex`.

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'umi008/hermes-agent-go'
    $installDir = $env:HERMES_GO_INSTALL_DIR
    if (-not $installDir) { $installDir = Join-Path $env:LOCALAPPDATA 'hermes-go\bin' }
    $downloadUrl = $env:HERMES_GO_DOWNLOAD_URL
    if (-not $downloadUrl) { $downloadUrl = "https://github.com/$repo/releases/download" }

    function Say([string]$msg) { Write-Host "hermes-go install: $msg" }

    $cpu = $env:PROCESSOR_ARCHITEW6432
    if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
    switch ($cpu) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "hermes-go install: unsupported architecture: $cpu" }
    }

    $version = $env:HERMES_GO_VERSION
    if (-not $version) {
        $latest = Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest"
        $version = $latest.tag_name
        if (-not $version) { throw 'hermes-go install: could not find the latest release; set HERMES_GO_VERSION' }
    }
    if (-not $version.StartsWith('v')) { $version = "v$version" }

    $archive = "hermes-go_$($version.Substring(1))_windows_$arch.zip"
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("hermes-go-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Say "downloading $archive ($version)"
        $zip = Join-Path $tmp $archive
        $sums = Join-Path $tmp 'checksums.txt'
        try {
            Invoke-WebRequest -UseBasicParsing "$downloadUrl/$version/$archive" -OutFile $zip
            Invoke-WebRequest -UseBasicParsing "$downloadUrl/$version/checksums.txt" -OutFile $sums
        } catch {
            throw "hermes-go install: download failed: $downloadUrl/$version/$archive ($($_.Exception.Message))"
        }

        $want = $null
        foreach ($line in Get-Content $sums) {
            $parts = $line -split '\s+'
            if ($parts.Count -ge 2 -and $parts[1] -eq $archive) { $want = $parts[0].ToLowerInvariant() }
        }
        if (-not $want) { throw "hermes-go install: $archive is not listed in checksums.txt" }
        $got = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLowerInvariant()
        if ($got -ne $want) {
            throw "hermes-go install: checksum mismatch for $archive (want $want, got $got); nothing was installed"
        }

        $unpacked = Join-Path $tmp 'unpacked'
        Expand-Archive -Path $zip -DestinationPath $unpacked
        $exe = Join-Path $unpacked 'hermes-go.exe'
        if (-not (Test-Path $exe)) { throw 'hermes-go install: archive has no hermes-go.exe' }

        New-Item -ItemType Directory -Force -Path $installDir | Out-Null
        $target = Join-Path $installDir 'hermes-go.exe'
        Copy-Item -Force $exe $target
        Say "installed $(& $target version) to $target"
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $onPath = ($userPath -split ';') -contains $installDir
    if ($onPath) {
        # Already there.
    } elseif ($env:HERMES_GO_NO_MODIFY_PATH -eq '1') {
        Say "$installDir is not on your PATH; add it to use hermes-go from any shell"
    } else {
        $newPath = if ($userPath) { "$userPath;$installDir" } else { $installDir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        $env:Path = "$env:Path;$installDir"
        Say "added $installDir to your user PATH; open a new terminal to pick it up"
    }
    Say 'next: set a provider key (e.g. OPENROUTER_API_KEY) and run: hermes-go doctor'
}
