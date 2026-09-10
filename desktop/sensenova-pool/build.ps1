param(
    [string]$Version = "0.1.0"
)

$ErrorActionPreference = "Stop"
$moduleDir = $PSScriptRoot
$outputDir = Join-Path $moduleDir "dist"
$resourceFile = Join-Path $moduleDir "rsrc.syso"
$outputFile = Join-Path $outputDir "SenseNovaPool.exe"
$previousCGO = $env:CGO_ENABLED

Push-Location $moduleDir
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) {
        throw "Go tests failed"
    }

    go run github.com/akavel/rsrc@v0.10.2 -manifest "SenseNovaPool.manifest" -o $resourceFile
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to embed the Windows application manifest"
    }

    New-Item -ItemType Directory -Path $outputDir -Force | Out-Null
    $env:CGO_ENABLED = "0"
    go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$Version" -o $outputFile .
    if ($LASTEXITCODE -ne 0) {
        throw "Go build failed"
    }

    $artifact = Get-Item -LiteralPath $outputFile
    $hash = Get-FileHash -LiteralPath $outputFile -Algorithm SHA256
    [pscustomobject]@{
        File = $artifact.FullName
        Version = $Version
        SizeMiB = [math]::Round($artifact.Length / 1MB, 2)
        SHA256 = $hash.Hash.ToLowerInvariant()
    }
}
finally {
    $env:CGO_ENABLED = $previousCGO
    Pop-Location
}
