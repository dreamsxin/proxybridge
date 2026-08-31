package cachef

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)


func TestCachef(t *testing.T) {
	filename := "abc.txt"
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

// 末行没有换行符时，该条记录不能被丢弃
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

