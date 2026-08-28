$ErrorActionPreference = "Stop"

# Save caller's GOOS/GOARCH and restore on exit to avoid polluting the PowerShell session
$origGOOS = $env:GOOS
$origGOARCH = $env:GOARCH

try {
    $buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    $gitCommit = (git rev-parse --short HEAD).Trim()
    $ldflags = "-w -s -X main.BuildTime=$buildTime -X main.GitCommit=$gitCommit"

    New-Item -ItemType Directory -Force -Path "bin" | Out-Null

    $env:CGO_ENABLED = "0"

    # windows amd64
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    go build -ldflags $ldflags -o "bin/bridge-direct.exe" main.go

    # linux amd64
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    go build -ldflags $ldflags -o "bin/bridge-direct" main.go

    # linux arm64
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    go build -ldflags $ldflags -o "bin/bridge-direct-arm" main.go

    Write-Host "Build OK" -ForegroundColor Green
}
finally {
    # Restore caller's env to avoid leaking GOOS/GOARCH into the session
    if ($null -ne $origGOOS) {
        $env:GOOS = $origGOOS
    } else {
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    }
    if ($null -ne $origGOARCH) {
        $env:GOARCH = $origGOARCH
    } else {
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    }
}
