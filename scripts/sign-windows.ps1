[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$ArtifactDirectory
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if ([string]::IsNullOrWhiteSpace($env:WINDOWS_CERTIFICATE_BASE64)) { throw 'WINDOWS_CERTIFICATE_BASE64 is required.' }
if ([string]::IsNullOrWhiteSpace($env:WINDOWS_CERTIFICATE_PASSWORD)) { throw 'WINDOWS_CERTIFICATE_PASSWORD is required.' }

$ResolvedArtifacts = [IO.Path]::GetFullPath($ArtifactDirectory)
if (-not (Test-Path -LiteralPath $ResolvedArtifacts -PathType Container)) { throw "Artifact directory does not exist: $ResolvedArtifacts" }
$SignTool = Get-ChildItem -Path "${env:ProgramFiles(x86)}\Windows Kits\10\bin" -Filter signtool.exe -Recurse -File |
    Sort-Object FullName -Descending |
    Select-Object -First 1
if (-not $SignTool) { throw 'signtool.exe was not found in the Windows SDK.' }

$TemporaryDirectory = Join-Path ([IO.Path]::GetTempPath()) ("qobuz-curator-signing-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TemporaryDirectory | Out-Null
$Certificate = Join-Path $TemporaryDirectory 'certificate.pfx'
try {
    [IO.File]::WriteAllBytes($Certificate, [Convert]::FromBase64String($env:WINDOWS_CERTIFICATE_BASE64))
    $Files = Get-ChildItem -LiteralPath $ResolvedArtifacts -Filter '*.exe' -File
    if (-not $Files) { throw 'No Windows executables were found to sign.' }
    foreach ($File in $Files) {
        & $SignTool.FullName sign /fd SHA256 /td SHA256 /tr 'http://timestamp.digicert.com' /f $Certificate /p $env:WINDOWS_CERTIFICATE_PASSWORD $File.FullName
        if ($LASTEXITCODE -ne 0) { throw "Signing failed for $($File.Name)." }
        & $SignTool.FullName verify /pa /v $File.FullName
        if ($LASTEXITCODE -ne 0) { throw "Signature verification failed for $($File.Name)." }
    }
} finally {
    if (Test-Path -LiteralPath $TemporaryDirectory) {
        Remove-Item -LiteralPath $TemporaryDirectory -Recurse -Force
    }
}
