package logger

import (
	"io"
	"log/slog"
	"os"

	"github.com/baowk/gproxy/configs"
	"gopkg.in/natefinch/lumberjack.v2"
)

func InitLog(log *configs.Log) {
	var levelI slog.Leveler
	switch log.Level {
	case "debug":
		levelI = slog.LevelDebug
	case "info":
		levelI = slog.LevelInfo
	case "warn":
		levelI = slog.LevelWarn
	case "error":
		levelI = slog.LevelError
	default:
		levelI = slog.LevelDebug
	}

	opts := slog.HandlerOptions{
		AddSource: log.AddSource,
		Level:     levelI,
	}

	var logW io.Writer

	if log.Output == "" {
		logW = os.Stdout
	} else {
		logW = &lumberjack.Logger{
			// 日志文件名，归档日志也会保存在对应目录下
			// 若该值为空，则日志会保存到os.TempDir()目录下，日志文件名为
			// <processname>-lumberjack.log
			Filename: log.Output,

			// backup的日志是否使用本地时间戳，默认使用UTC时间
			LocalTime: true,
			// 日志大小到达MaxSize(MB)就开始backup，默认值是100.
			MaxSize: log.MaxSize,
			// 旧日志保存的最大天数，默认保存所有旧日志文件
			MaxAge: log.MaxAge,
			// 旧日志保存的最大数量，默认保存所有旧日志文件
			MaxBackups: log.MaxBackups,
			// 对backup的日志是否进行压缩，默认不压缩
			Compress: true,
		}
	}
	var logger *slog.Logger

	if log.Format == "json" {
		logger = slog.New(slog.NewJSONHandler(logW, &opts))
	} else {
		logger = slog.New(slog.NewTextHandler(logW, &opts))
	}
	slog.SetDefault(logger)
}
