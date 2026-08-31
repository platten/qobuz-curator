[CmdletBinding()]
param(
    [ValidatePattern('^[A-Za-z0-9._+\-]+$')]
    [string]$Version = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$ProjectDir = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$OutputDir = [IO.Path]::GetFullPath((Join-Path $ProjectDir 'dist'))
$ExpectedOutputDir = [IO.Path]::GetFullPath("$ProjectDir$([IO.Path]::DirectorySeparatorChar)dist")
$CycloneDXVersion = 'v1.12.0'

function Remove-QobuzTemporaryDirectory {
    param([Parameter(Mandatory)][string]$Path)
    $ResolvedPath = [IO.Path]::GetFullPath($Path)
    $TemporaryRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    $Leaf = Split-Path -Leaf $ResolvedPath
    if (-not $ResolvedPath.StartsWith($TemporaryRoot, [StringComparison]::OrdinalIgnoreCase) -or -not $Leaf.StartsWith('qobuz-curator-', [StringComparison]::Ordinal)) {
        throw "Refusing unsafe temporary directory removal: $ResolvedPath"
    }
    if (Test-Path -LiteralPath $ResolvedPath) {
        Remove-Item -LiteralPath $ResolvedPath -Recurse -Force
    }
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    try { $Version = (& git -C $ProjectDir describe --tags --always --dirty 2>$null).Trim() } catch { $Version = 'dev' }
}
if ([string]::IsNullOrWhiteSpace($Version)) { $Version = 'dev' }
if ($Version -notmatch '^[A-Za-z0-9._+\-]+$') { throw 'Version contains unsupported characters.' }

$StagingDir = Join-Path ([IO.Path]::GetTempPath()) ("qobuz-curator-build-" + [guid]::NewGuid().ToString('N'))
$ToolDir = Join-Path ([IO.Path]::GetTempPath()) ("qobuz-curator-tools-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $StagingDir, $ToolDir | Out-Null
$OriginalLocation = Get-Location
Set-Location -LiteralPath $ProjectDir

try {
    $Targets = @(
        @{ OS = 'linux'; Arch = 'amd64'; Suffix = '' },
        @{ OS = 'linux'; Arch = 'arm64'; Suffix = '' },
        @{ OS = 'windows'; Arch = 'amd64'; Suffix = '.exe' },
        @{ OS = 'windows'; Arch = 'arm64'; Suffix = '.exe' },
        @{ OS = 'darwin'; Arch = 'amd64'; Suffix = '' },
        @{ OS = 'darwin'; Arch = 'arm64'; Suffix = '' }
    )

    Write-Host "Building Qobuz Curator $Version" -ForegroundColor Cyan
    foreach ($Target in $Targets) {
        $Artifact = "qobuz-curator-$($Target.OS)-$($Target.Arch)$($Target.Suffix)"
        Write-Host "  -> $Artifact" -ForegroundColor DarkCyan
        $PreviousCGO = $env:CGO_ENABLED
        $PreviousGOOS = $env:GOOS
        $PreviousGOARCH = $env:GOARCH
        try {
            $env:CGO_ENABLED = '0'
            $env:GOOS = $Target.OS
            $env:GOARCH = $Target.Arch
            & go build -trimpath -buildvcs=false "-ldflags=-s -w -X main.version=$Version" -o (Join-Path $StagingDir $Artifact) .
            if ($LASTEXITCODE -ne 0) { throw "Go build failed for $($Target.OS)/$($Target.Arch)." }
        } finally {
            $env:CGO_ENABLED = $PreviousCGO
            $env:GOOS = $PreviousGOOS
            $env:GOARCH = $PreviousGOARCH
        }
    }

    Set-Content -LiteralPath (Join-Path $StagingDir 'VERSION.txt') -Value $Version -Encoding utf8NoBOM
    Copy-Item -LiteralPath (Join-Path $ProjectDir 'LICENSE') -Destination $StagingDir
    Copy-Item -LiteralPath (Join-Path $ProjectDir 'README.md') -Destination $StagingDir

    Write-Host 'Generating CycloneDX SBOM' -ForegroundColor Cyan
    $PreviousGOBIN = $env:GOBIN
    try {
        $env:GOBIN = $ToolDir
        & go install "github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$CycloneDXVersion"
        if ($LASTEXITCODE -ne 0) { throw 'Could not install the pinned CycloneDX generator.' }
    } finally {
        $env:GOBIN = $PreviousGOBIN
    }
    $CycloneDX = Join-Path $ToolDir 'cyclonedx-gomod.exe'
    & $CycloneDX bin -std -json -version $Version -output (Join-Path $StagingDir 'qobuz-curator.cdx.json') (Join-Path $StagingDir 'qobuz-curator-windows-amd64.exe')
    if ($LASTEXITCODE -ne 0) { throw 'CycloneDX SBOM generation failed.' }

    $ChecksumLines = Get-ChildItem -LiteralPath $StagingDir -File |
        Where-Object Name -ne 'SHA256SUMS' |
        Sort-Object Name |
        ForEach-Object { "$( (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant() )  $($_.Name)" }
    Set-Content -LiteralPath (Join-Path $StagingDir 'SHA256SUMS') -Value $ChecksumLines -Encoding ascii

    if ($OutputDir -ne $ExpectedOutputDir -or -not $OutputDir.StartsWith($ProjectDir + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing unsafe output directory: $OutputDir"
    }
    if (Test-Path -LiteralPath $OutputDir) {
        Remove-Item -LiteralPath $OutputDir -Recurse -Force
    }
    Move-Item -LiteralPath $StagingDir -Destination $OutputDir
    $StagingDir = $null
    Write-Host "Release artifacts are in $OutputDir" -ForegroundColor Green
} finally {
    Set-Location -LiteralPath $OriginalLocation
    if ($StagingDir) { Remove-QobuzTemporaryDirectory -Path $StagingDir }
    Remove-QobuzTemporaryDirectory -Path $ToolDir
}
