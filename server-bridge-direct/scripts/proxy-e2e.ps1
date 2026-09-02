[CmdletBinding()]
param(
    [int]$Rounds = 1,
    [Parameter(Mandatory = $true)]
    [string]$ProxyFile,
    [int]$Concurrency = 10,
    [int]$RequestsPerProxy = 2,
    [int]$RequestTimeoutSeconds = 25,
    [string]$BridgeBin = "",
    [string]$Report = "",
    [switch]$VerboseOutput,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    $goArgs = @(
        "run", "./cmd/proxy-e2e",
        "-rounds", $Rounds,
        "-proxy-file", $ProxyFile,
        "-concurrency", $Concurrency,
        "-requests-per-proxy", $RequestsPerProxy,
        "-request-timeout", ("{0}s" -f $RequestTimeoutSeconds)
    )
    if ($BridgeBin -ne "") { $goArgs += @("-bridge-bin", $BridgeBin) }
    if ($Report -ne "") { $goArgs += @("-report", $Report) }
    if ($VerboseOutput) { $goArgs += "-verbose" }
    if ($DryRun) { $goArgs += "-dry-run" }

    & go @goArgs
    exit $LASTEXITCODE
}
finally {
    Pop-Location
}
