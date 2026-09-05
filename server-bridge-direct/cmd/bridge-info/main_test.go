package main

import "testing"

func TestParsePrometheusTextAggregatesLabels(t *testing.T) {
	values, err := parsePrometheusText(`# HELP bridge_dials_total target dials
# TYPE bridge_dials_total counter
bridge_dials_total{result="success"} 12
bridge_dials_total{result="failure"} 3
bridge_connections_rejected_total{reason="global_limit"} 2
bridge_connections_rejected_total{reason="port_limit"} 1
bridge_runtime_goroutines 9
`)
	if err != nil {
		t.Fatal(err)
	}
	if values["bridge_dials_total"] != 15 || values["bridge_dials_total.result=success"] != 12 || values["bridge_dials_total.result=failure"] != 3 {
		t.Fatalf("dial metrics = %#v", values)
	}
	if got := statsFromMetrics(values); got.DialOK != 12 || got.DialFail != 3 || got.Rejected != 3 || got.Goroutines != 9 {
		t.Fatalf("runtime stats = %+v", got)
	}
}

func TestJoinURL(t *testing.T) {
	if got := joinURL("127.0.0.1:5678/", "/bridge/status"); got != "http://127.0.0.1:5678/bridge/status" {
		t.Fatalf("joinURL = %q", got)
	}
}
