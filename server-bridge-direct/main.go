package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"


	"github.com/baowk/bridge-direct/config"
	"github.com/baowk/bridge-direct/server"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
)

const VERSION = "v0.0.5+20260609"
const MYIP_URL = "http://api.ipipv.com"

var (
	BuildTime = "unknown"
	GitCommit = "unknown"
)

func init() {
	flag.StringVar(&config.CfgFile, "c", "config.json", "配置文件: -c config.json")
	flag.Parse()
}

func main() {
	if flag.NArg() > 0 && flag.Arg(0) == "version" {
		printVersion()
		return
	}

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

func printVersion() {
	fmt.Println("bridge-direct", VERSION)
	fmt.Println("build_time:", BuildTime)
	fmt.Println("git_commit:", GitCommit)
}

func initLog() {
	opts := slog.HandlerOptions{
		AddSource: config.Cfg.LogSource,
		Level:     slog.LevelDebug,
	}

	var logW io.Writer

	if config.Cfg.LogFile == "" {
		logW = os.Stdout
	} else {
		logW = &lumberjack.Logger{
			// 日志文件名，归档日志也会保存在对应目录下
			// 若该值为空，则日志会保存到os.TempDir()目录下，日志文件名为
			// <processname>-lumberjack.log
			Filename: config.Cfg.LogFile,

			// backup的日志是否使用本地时间戳，默认使用UTC时间
			LocalTime: true,
			// 日志大小到达MaxSize(MB)就开始backup，默认值是100.
			MaxSize: 100,
			// 旧日志保存的最大天数，默认保存所有旧日志文件
			MaxAge: 7,
			// 旧日志保存的最大数量，默认保存所有旧日志文件
			MaxBackups: 10,
			// 对backup的日志是否进行压缩，默认不压缩
			Compress: true,
		}
	}
	if config.Cfg.LogLevel == "error" {
		opts.Level = slog.LevelError
	} else if config.Cfg.LogLevel == "info" {
		opts.Level = slog.LevelInfo
	} else if config.Cfg.LogLevel == "warn" {
		opts.Level = slog.LevelWarn
	}
	if config.Cfg.LogFormat == "json" {
		config.Logger = slog.New(slog.NewJSONHandler(logW, &opts))
	} else {
		config.Logger = slog.New(slog.NewTextHandler(logW, &opts))
	}
	slog.SetDefault(config.Logger)
}
