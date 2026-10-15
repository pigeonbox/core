package logger

import (
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var Logger *zap.Logger

// once 保护惰性兜底初始化：core 是可嵌入库（server/fnos 库式调用、单元测试
// 都可能不经过 bootstrap 的 Init），宿主未初始化时日志降级到默认 logger 而非 panic。
var once sync.Once

func get() *zap.Logger {
	once.Do(func() {
		if Logger == nil {
			l, err := zap.NewProduction()
			if err != nil {
				l = zap.NewNop()
			}
			Logger = l
		}
	})
	return Logger
}

type Config struct {
	Level      string
	Filename   string
	MaxSize    int
	MaxBackups int
	MaxAge     int
	Compress   bool
}

func Init(cfg *Config) error {
	level := getLevel(cfg.Level)

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	var core zapcore.Core
	encoder := zapcore.NewJSONEncoder(encoderConfig)

	consoleEncoder := zapcore.NewConsoleEncoder(encoderConfig)
	consoleCore := zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), level)

	// 环形缓冲旁路（管理端「系统日志」页数据源，见 ring.go）：与控制台/文件
	// 链同级别 Tee，业务代码零改动。惰性兜底路径（get 的 NewProduction）不
	// 经 Init，不挂环——面板只呈现 bootstrap 之后的运行日志。
	ring := newRingCore(level)
	if cfg.Filename != "" {
		sink, err := openFileSink(cfg)
		if err != nil {
			return err
		}
		fileCore := zapcore.NewCore(encoder, sink, level)
		core = zapcore.NewTee(consoleCore, fileCore, ring)
	} else {
		core = zapcore.NewTee(consoleCore, ring)
	}

	Logger = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))
	return nil
}

// openFileSink 按配置构造文件写出端。
//
// 行为分叉（2026-10-16 修复：MaxSize/MaxBackups/MaxAge/Compress 四字段此前
// 定义后从未被消费，日志文件无限增长）：
//   - MaxSize > 0（显式配置了轮转）→ lumberjack 承接四字段，按大小轮转；
//   - MaxSize <= 0 → 保持历史 O_APPEND 裸文件行为不变（零配置用户零变化，
//     不引入意外轮转语义；lumberjack 对 MaxSize=0 会回落 100MB 默认值，
//     故此处不能用 lumberjack 兜零配置）。
func openFileSink(cfg *Config) (zapcore.WriteSyncer, error) {
	if cfg.MaxSize > 0 {
		return zapcore.AddSync(&lumberjack.Logger{
			Filename:   cfg.Filename,
			MaxSize:    cfg.MaxSize,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAge,
			Compress:   cfg.Compress,
		}), nil
	}
	file, err := os.OpenFile(cfg.Filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return zapcore.AddSync(file), nil
}

func getLevel(level string) zapcore.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	case "fatal":
		return zapcore.FatalLevel
	default:
		return zapcore.InfoLevel
	}
}

func Sync() {
	if Logger != nil {
		_ = Logger.Sync()
	}
}

func Debug(msg string, fields ...zap.Field) {
	get().Debug(msg, fields...)
}

func Info(msg string, fields ...zap.Field) {
	get().Info(msg, fields...)
}

func Warn(msg string, fields ...zap.Field) {
	get().Warn(msg, fields...)
}

func Error(msg string, fields ...zap.Field) {
	get().Error(msg, fields...)
}

func Fatal(msg string, fields ...zap.Field) {
	get().Fatal(msg, fields...)
}

func With(fields ...zap.Field) *zap.Logger {
	// 走 get() 懒解析：宿主未 Init 时（库式嵌入/单测）兜底 logger 而非 nil panic。
	return get().With(fields...)
}
