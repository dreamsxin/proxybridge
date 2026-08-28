package enums

import (
	"strconv"
	"strings"
)

const (
	Version = "v0.2.07"
	AppCode = "xproxy"
)

func VersionToNum() int {
	version1 := strings.TrimPrefix(Version, "v")
	arr := strings.Split(version1, ".")
	var ver int
	if len(arr) == 3 {
		if n1, err := strconv.Atoi(arr[0]); err != nil {
			return 0
		} else {
			ver = n1 * 10000
		}
		if n2, err := strconv.Atoi(arr[1]); err != nil {
			return 0
		} else {
			ver = ver + n2*100
		}
		if n3, err := strconv.Atoi(arr[2]); err != nil {
			return 0
		} else {
			ver = ver + n3
		}
	}
	return ver
}
