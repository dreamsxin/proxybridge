package cachef

import (
	"fmt"
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
