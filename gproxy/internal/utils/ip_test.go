package utils

import (
	"testing"
	"time"
)

func TestIpInfo(t *testing.T) {
	//台湾
	//202.160.81.94
	//202.160.80.25

	//香港
	//149.126.94.249
	//43.248.60.105
	//154.64.154.205

	//澳门
	//149.88.196.158
	//149.87.152.168
	t1 := time.Now()
	Init("../assets/GeoLite2-City.mmdb")
	t.Log(time.Since(t1))
	t2 := time.Now()
	i, err := GetIpInfo("149.88.196.158")
	if err != nil {
		t.Error(err)
	}
	t.Log(time.Since(t2))
	t.Log(i)

	t3 := time.Now()
	t.Log(IsMainlandChina("154.64.154.205"))
	t.Log(time.Since(t3))

	t4 := time.Now()
	i, err = GetIpInfo("202.160.81.94")
	if err != nil {
		t.Error(err)
	}
	t.Log(time.Since(t4))

	t.Log(i)

	t5 := time.Now()
	i, err = GetIpInfo("149.88.196.158")
	if err != nil {
		t.Error(err)
	}
	t.Log(time.Since(t5))

	t.Log(i)
	t.Log(i.IsMainlandChina(), i.Country.ISOCode, i.GetCountryEn(), i.GetCityEn())
}
