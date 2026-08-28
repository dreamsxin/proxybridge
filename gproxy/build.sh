#GOOS darwin、freebsd、linux、windows
#GOARCH 386、amd64、arm arm64



#windows
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build  -ldflags '-w -s' -o bin/xproxy-x64.exe cmd/main.go

#mac amd
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build  -ldflags '-w -s' -o bin/xproxy-darwin-x64 cmd/main.go
#arm
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build  -ldflags '-w -s' -o bin/xproxy-darwin-arm cmd/main.go

#arm
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build  -ldflags '-w -s' -o bin/xproxy-linux-arm cmd/main.go
#linux amd
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build  -ldflags '-w -s' -o bin/xproxy-linux-x64 cmd/main.go


if command -v upx &> /dev/null; then
    echo "Compressing binary with UPX..."
    upx -1 ./bin/xproxy-linux-x64
else
    echo "UPX not found. Please install UPX to compress the binary."
    echo "Installation options:"
    echo "  Ubuntu/Debian: sudo apt-get install upx"
    echo "  CentOS/RHEL:   sudo yum install upx"
    echo "  macOS:         brew install upx"
    echo "  Windows:       Download from https://upx.github.io/"
    echo ""
fi