package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
)

const VERSION = "v0.0.6+20260901"

const MYIP_URL = "http://api.ipipv.com"

var (
	// 正式构建由 build.ps1/build.sh 通过 -ldflags 注入；go run 时由
	// initBuildMetadata 从 build info 和当前时间补齐，避免手工维护过期值。
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func init() {
	flag.StringVar(&config.CfgFile, "c", "config.json", "配置文件: -c config.json")
}

func main() {
	flag.Parse()
	initBuildMetadata()
	if flag.NArg() > 0 && flag.Arg(0) == "version" {
		printVersion()
		return
	}
	// 版本信息始终输出到 stdout，不受 logFile/logConsole 配置影响，
	// 便于在服务启动时直接确认运行的构建是否为最新版本。
	fmt.Printf("bridge-direct startup version=%s build_time=%s git_commit=%s\n", VERSION, BuildTime, GitCommit)

	if config.CfgFile == "" {
		panic("找不到配置文件")
	}
	v := viper.New()
	v.SetConfigFile(config.CfgFile)
	v.SetConfigType("json")
	err := v.ReadInConfig()
	if err != nil {
		panic(fmt.Sprintf("Fatal error config file: %v \n", err))
	}

	if err = v.Unmarshal(&config.Cfg); err != nil {
		panic(err)
	}
	// 缺失项用缺省值兜底，尤其是日志轮转：没有上界的日志迟早把磁盘写满
	config.Cfg.ApplyDefaults()
	printServiceEndpoints()
	//初始化日志
	initLog()

	// AES key 长度必须是 16/24/32，否则每个管理请求都会在解密时失败，
	// 服务看起来正常但功能全废——必须在启动时就暴露出来
	if n := len(config.Cfg.Key); n != 16 && n != 24 && n != 32 {
		slog.Error("invalid aes key length, want 16/24/32", "length", n)
		os.Exit(1)
	}

	slog.Info("bridge-direct starting", "version", VERSION, "mode", config.Cfg.Mode)

	go server.Start()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	sig := <-quit
	slog.Info("bridge-direct shutting down", "signal", sig.String())
}

// printServiceEndpoints is intentionally stdout-based. Operators need to see
// where to scrape metrics even when application logs are redirected to a file
// and logConsole is disabled.
func printServiceEndpoints() {
	managementAddr := config.Cfg.Addr
	if managementAddr == "" {
		managementAddr = ":8080"
	}
	fmt.Printf("bridge-direct management listen=%s\n", managementAddr)
	if config.Cfg.MetricsAddr == "" {
		fmt.Println("bridge-direct metrics=disabled")
		return
	}
	fmt.Printf("bridge-direct metrics listen=%s path=/metrics\n", config.Cfg.MetricsAddr)
}

// initBuildMetadata 只填充未被 linker 注入的值。正式构建的元数据优先级最高，
// go run/普通 go build 则使用 Go 的 VCS build info 和当前 UTC 时间。
func initBuildMetadata() {
	BuildTime, GitCommit = resolveBuildMetadata(BuildTime, GitCommit, time.Now().UTC(), nil)
}

func resolveBuildMetadata(buildTime, gitCommit string, now time.Time, info *debug.BuildInfo) (string, string) {
	if buildTime == "" || buildTime == "unknown" {
		buildTime = now.UTC().Format(time.RFC3339)
	}
	if gitCommit == "" || gitCommit == "unknown" {
		if info == nil {
			if current, ok := debug.ReadBuildInfo(); ok {
				info = current
			}
		}
		if info != nil {
			for _, setting := range info.Settings {
				if setting.Key != "vcs.revision" || setting.Value == "" {
					continue
				}
				gitCommit = shortGitCommit(setting.Value)
				break
			}
		}
	}
	if gitCommit == "" {
		gitCommit = "unknown"
	}
	return buildTime, gitCommit
}

func shortGitCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func printVersion() {
	fmt.Println("bridge-direct", VERSION)
	fmt.Println("build_time:", BuildTime)
	fmt.Println("git_commit:", GitCommit)
}

func initLog() {
	opts := slog.HandlerOptions{
		AddSource: config.Cfg.LogSource,
	}

	var logW io.Writer

	if config.Cfg.LogFile == "" {
		// 注意：这条路径进程内没有任何轮转，必须由 systemd/journald 或容器运行时
		// 负责收集和限额，别用 nohup 重定向到文件——那就是无上界增长
		logW = os.Stdout
	} else {
		logW = &lumberjack.Logger{
			// 日志文件名，归档日志也会保存在对应目录下
			Filename: config.Cfg.LogFile,
			// backup的日志是否使用本地时间戳，默认使用UTC时间
			LocalTime: true,
			// 单文件大小上限(MB)，到达就切割
			MaxSize: config.Cfg.LogMaxSizeMB,
			// 归档保留天数
			MaxAge: config.Cfg.LogMaxAgeDays,
			// 归档保留个数，与 MaxAge 谁先满足谁生效
			MaxBackups: config.Cfg.LogMaxBackups,
			// 归档是否压缩
			Compress: config.Cfg.LogCompressEnabled(),
		}
		// 同时输出到控制台，便于本地调试。
		// 生产环境交给 journald/docker 收集时应关掉，避免重复采集，
		// 而且 stdout 那份不受上面的轮转策略约束。
		if config.Cfg.LogConsole {
			logW = io.MultiWriter(os.Stdout, logW)
		}
	}

	switch config.Cfg.LogLevel {
	case "error":
		opts.Level = slog.LevelError
	case "warn":
		opts.Level = slog.LevelWarn
	case "debug":
		// debug 会给每条连接打 flow-up/flow-down 两行，SOCKS5 场景下日志量
		// 按连接数放大，只适合本地排查
		opts.Level = slog.LevelDebug
	default:
		opts.Level = slog.LevelInfo
	}
	if config.Cfg.LogFormat == "json" {
		config.Logger = slog.New(slog.NewJSONHandler(logW, &opts))
	} else {
		config.Logger = slog.New(slog.NewTextHandler(logW, &opts))
	}
	slog.SetDefault(config.Logger)
}
