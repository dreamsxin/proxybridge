BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_COMMIT=unknown
if command -v git >/dev/null 2>&1; then
    GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || true)
    if [ -z "${GIT_COMMIT}" ]; then
        GIT_COMMIT=unknown
    fi
fi
LDFLAGS="-w -s -X main.BuildTime=${BUILD_TIME} -X main.GitCommit=${GIT_COMMIT}"

#arm
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "${LDFLAGS}" -o bin/bridge-direct-arm main.go
#linux amd
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o bin/bridge-direct main.go
