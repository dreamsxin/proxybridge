package config

import "testing"

// 配置缺失时必须落到有上界的缺省值上：没有轮转上界的日志迟早写满磁盘
func TestApplyDefaultsFillsMissingValues(t *testing.T) {
	var c Config
	c.ApplyDefaults()

	if c.LogLevel != DefaultLogLevel {
		t.Errorf("logLevel = %q, want %q", c.LogLevel, DefaultLogLevel)
	}
	if c.LogFormat != DefaultLogFormat {
		t.Errorf("logFormat = %q, want %q", c.LogFormat, DefaultLogFormat)
	}
	if c.LogMaxSizeMB != DefaultLogMaxSizeMB {
		t.Errorf("logMaxSizeMB = %d, want %d", c.LogMaxSizeMB, DefaultLogMaxSizeMB)
	}
	if c.LogMaxAgeDays != DefaultLogMaxAgeDays {
		t.Errorf("logMaxAgeDays = %d, want %d", c.LogMaxAgeDays, DefaultLogMaxAgeDays)
	}
	if c.LogMaxBackups != DefaultLogMaxBackups {
		t.Errorf("logMaxBackups = %d, want %d", c.LogMaxBackups, DefaultLogMaxBackups)
	}
	if !c.LogCompressEnabled() {
		t.Error("logCompress should default to true")
	}
	// 双写会绕过轮转策略，缺省必须是关
	if c.LogConsole {
		t.Error("logConsole should default to false")
	}
	if c.LogSource {
		t.Error("logSource should default to false")
	}
}

// 显式配置不能被缺省值覆盖，包括显式关闭压缩
func TestApplyDefaultsKeepsExplicitValues(t *testing.T) {
	off := false
	c := Config{
		LogLevel:      "debug",
		LogFormat:     "json",
		LogMaxSizeMB:  5,
		LogMaxAgeDays: 1,
		LogMaxBackups: 2,
		LogCompress:   &off,
		LogConsole:    true,
	}
	c.ApplyDefaults()

	if c.LogLevel != "debug" || c.LogFormat != "json" {
		t.Errorf("level/format overwritten: %q %q", c.LogLevel, c.LogFormat)
	}
	if c.LogMaxSizeMB != 5 || c.LogMaxAgeDays != 1 || c.LogMaxBackups != 2 {
		t.Errorf("rotation overwritten: %d %d %d", c.LogMaxSizeMB, c.LogMaxAgeDays, c.LogMaxBackups)
	}
	if c.LogCompressEnabled() {
		t.Error("explicit logCompress=false was ignored")
	}
	if !c.LogConsole {
		t.Error("explicit logConsole=true was ignored")
	}
}

// 非法值（<=0）等价于没配，否则 lumberjack 会退回它自己的默认或不清理
func TestApplyDefaultsRejectsNonPositiveRotation(t *testing.T) {
	c := Config{LogMaxSizeMB: 0, LogMaxAgeDays: -1, LogMaxBackups: -10}
	c.ApplyDefaults()

	if c.LogMaxSizeMB != DefaultLogMaxSizeMB ||
		c.LogMaxAgeDays != DefaultLogMaxAgeDays ||
		c.LogMaxBackups != DefaultLogMaxBackups {
		t.Errorf("non-positive rotation not replaced: %d %d %d",
			c.LogMaxSizeMB, c.LogMaxAgeDays, c.LogMaxBackups)
	}
}
