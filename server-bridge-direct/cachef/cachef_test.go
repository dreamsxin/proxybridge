package cachef

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCachef(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "abc.txt")
	cf, err := New(filename)
	if err != nil {
		t.Error(err)
		return
	}
	cf.Add(123, "abc11")
	cf.Add(1234, "abc")
	cf.Add(123, "abcd112")
	cf.Close()

	cf1, err1 := New(filename)
	if err1 != nil {
		t.Error(err1)
		return
	}
	cf1.Add(1235, "abc")
	cf1.Del(123)
	cf1.Add(12311, "abc")
	cf1.Add(1231, "abc")
	cf1.Add(123, "abcadd")
	cf1.Del(123)
	fmt.Println(cf1.Get(123))
	cf1.Close()

}

// Replace 拒绝空集合：远端返回空列表时不能把本地数据擦掉
func TestReplaceRejectsEmptySet(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bridge.db")
	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}

	if err := cf.Replace(nil); err == nil {
		t.Fatal("expected Replace(nil) to be rejected")
	}
	if err := cf.Replace([]Bridge{}); err == nil {
		t.Fatal("expected Replace(empty) to be rejected")
	}
	if got := len(cf.All()); got != 1 {
		t.Fatalf("have %d bridges after rejected replace, want 1", got)
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if want := "8001,1.2.3.4:80\n"; string(content) != want {
		t.Fatalf("data file = %q, want %q", content, want)
	}
}

// Replace 成功时整体替换，本地多出来的端口要消失
func TestReplaceSwapsWholeSet(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bridge.db")
	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}

	if err := cf.Replace([]Bridge{{Port: 8002, ProxyAddr: "5.6.7.8:90"}}); err != nil {
		t.Fatal(err)
	}
	if cf.Get(8001) != nil {
		t.Fatal("port 8001 should be gone after Replace")
	}
	if b := cf.Get(8002); b == nil || b.ProxyAddr != "5.6.7.8:90" {
		t.Fatalf("port 8002 = %+v, want proxyAddr 5.6.7.8:90", b)
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if want := "8002,5.6.7.8:90\n"; string(content) != want {
		t.Fatalf("data file = %q, want %q", content, want)
	}
}

// Replace 写盘失败必须回滚内存，否则会留下「内存已空、磁盘还是旧数据」，
// 紧接着的 InitBridgeHandler 会一个桥都不起
func TestReplaceRollsBackWhenDumpFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows 上目录权限位不生效，无法制造写失败")
	}
	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")
	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := cf.Replace([]Bridge{{Port: 8002, ProxyAddr: "5.6.7.8:90"}}); err == nil {
		t.Fatal("expected Replace to fail while the directory is read-only")
	}
	if b := cf.Get(8001); b == nil || b.ProxyAddr != "1.2.3.4:80" {
		t.Fatalf("port 8001 = %+v, want the pre-replace value back", b)
	}
	if cf.Get(8002) != nil {
		t.Fatal("failed Replace must not leave the new set in memory")
	}
}

// 正常写入后目录里只应有数据文件本身：临时文件必须被 rename 掉或清理掉
func TestDumpLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")

	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	for i := 0; i < 5; i++ {
		if err := cf.Add(uint16(8001+i), "1.2.3.4:80"); err != nil {
			t.Fatal(err)
		}
	}
	if err := cf.Del(8003); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %s", entry.Name())
		}
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("dir contains %v, want only bridge.db", names)
	}
}

// dump 失败时不能破坏已有数据：旧实现先 Truncate 再写，失败就留下空文件或半截内容。
// 这里让临时文件创建失败（目录不可写），断言数据文件仍然是上一次的完整内容。
func TestFailedDumpKeepsPreviousFileIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows 上目录权限位不生效，无法制造写失败")
	}
	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")

	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := cf.Add(8002, "5.6.7.8:90"); err == nil {
		t.Fatal("expected dump to fail while the directory is read-only")
	}

	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("data file changed after a failed dump: %q -> %q", before, after)
	}

	// 恢复目录权限后重新加载：磁盘上仍是失败前的状态，不会出现空文件
	os.Chmod(dir, 0o700)
	reloaded, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reloaded.Close)
	if got := len(reloaded.All()); got != 1 {
		t.Fatalf("reloaded %d bridges, want 1", got)
	}
	if b := reloaded.Get(8001); b == nil || b.ProxyAddr != "1.2.3.4:80" {
		t.Fatalf("port 8001 = %+v, want proxyAddr 1.2.3.4:80", b)
	}
}

// 崩在 rename 之前会留下临时文件：它不叫 bridge.db，所以不能影响加载结果
func TestStaleTempFileIsIgnoredOnLoad(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")
	if err := os.WriteFile(filename, []byte("8001,1.2.3.4:80\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// 上次崩溃遗留的半截临时文件
	if err := os.WriteFile(filename+".tmp-123456", []byte("9999,garbage"), 0600); err != nil {
		t.Fatal(err)
	}

	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	if got := len(cf.All()); got != 1 {
		t.Fatalf("loaded %d bridges, want 1", got)
	}
	if cf.Get(9999) != nil {
		t.Fatal("stale temp file leaked into the loaded data")
	}
	if err := cf.Add(8002, "5.6.7.8:90"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if want := "8001,1.2.3.4:80\n8002,5.6.7.8:90\n"; string(content) != want {
		t.Fatalf("data file = %q, want %q", content, want)
	}
}
func TestUnmarshalKeepsLastLineWithoutNewline(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bridge.db")
	if err := os.WriteFile(filename, []byte("8001,1.2.3.4:80\n8002,5.6.7.8:90"), 0600); err != nil {
		t.Fatal(err)
	}

	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	if got := len(cf.All()); got != 2 {
		t.Fatalf("loaded %d bridges, want 2", got)
	}
	b := cf.Get(8002)
	if b == nil || b.ProxyAddr != "5.6.7.8:90" {
		t.Fatalf("port 8002 = %+v, want proxyAddr 5.6.7.8:90", b)
	}
}

// 端口段与单端口重叠时不能产生重复条目，否则 Get/Del 只命中第一条
func TestUnmarshalDeduplicatesPorts(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "bridge.db")
	if err := os.WriteFile(filename, []byte("8001:8003,1.2.3.4:80\n8002,5.6.7.8:90\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)

	if got := len(cf.All()); got != 3 {
		t.Fatalf("loaded %d bridges, want 3", got)
	}
	if b := cf.Get(8002); b == nil || b.ProxyAddr != "5.6.7.8:90" {
		t.Fatalf("port 8002 = %+v, want proxyAddr 5.6.7.8:90 (later line wins)", b)
	}
	if err := cf.Del(8002); err != nil {
		t.Fatal(err)
	}
	if b := cf.Get(8002); b != nil {
		t.Fatalf("port 8002 still present after Del: %+v", b)
	}
}

func TestAddRollsBackMemoryWhenDumpFails(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")
	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	// Make only the next dump fail without relying on directory permission bits,
	// which are not effective in all test environments (notably Windows).
	cf.filename = filepath.Join(dir, "missing", "bridge.db")
	if err := cf.Add(8002, "5.6.7.8:90"); err == nil {
		t.Fatal("expected Add to fail when its dump directory is missing")
	}
	if cf.Get(8001) == nil {
		t.Fatal("previous bridge disappeared from memory after failed Add")
	}
	if cf.Get(8002) != nil {
		t.Fatal("failed Add remained in memory")
	}

	// The original file was never overwritten.
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("data file changed after failed Add: %q -> %q", before, after)
	}
}

func TestDelRollsBackMemoryWhenDumpFails(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "bridge.db")
	cf, err := New(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cf.Close)
	if err := cf.Add(8001, "1.2.3.4:80"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	cf.filename = filepath.Join(dir, "missing", "bridge.db")
	if err := cf.Del(8001); err == nil {
		t.Fatal("expected Del to fail when its dump directory is missing")
	}
	if b := cf.Get(8001); b == nil || b.ProxyAddr != "1.2.3.4:80" {
		t.Fatalf("previous bridge not restored in memory after failed Del: %+v", b)
	}

	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("data file changed after failed Del: %q -> %q", before, after)
	}
}
