package main

import (
	"reflect"
	"testing"
)

func TestMonitoringOptions_Config(t *testing.T) {
	o := monitoringOptions{
		namespace: "mon", controllerService: "rel-metrics", nodeService: "rel-node-metrics",
		selector: "app.kubernetes.io/name=csi,app.kubernetes.io/instance=rel", controllerPort: 8080, nodePort: 9810,
	}
	cfg, err := o.config("kube-system")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DriverNamespace != "kube-system" || cfg.MonitoringNamespace != "mon" {
		t.Errorf("namespaces: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.Selector, map[string]string{"app.kubernetes.io/name": "csi", "app.kubernetes.io/instance": "rel"}) {
		t.Errorf("selector: %+v", cfg.Selector)
	}
	if len(cfg.Targets) != 2 || cfg.Targets[0].Component != "controller" || cfg.Targets[0].Port != 8080 ||
		cfg.Targets[1].Service != "rel-node-metrics" || cfg.Targets[1].Component != "node" || cfg.Targets[1].Port != 9810 {
		t.Errorf("targets: %+v", cfg.Targets)
	}
}

func TestMonitoringOptions_NodePortZeroDropsNodeTarget(t *testing.T) {
	o := monitoringOptions{namespace: "mon", controllerService: "c", nodeService: "n", selector: "a=b", controllerPort: 8080}
	cfg, err := o.config("kube-system")
	if err != nil || len(cfg.Targets) != 1 || cfg.Targets[0].Component != "controller" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestMonitoringOptions_BadSelector(t *testing.T) {
	o := monitoringOptions{namespace: "mon", selector: "nonsense"}
	if _, err := o.config("kube-system"); err == nil {
		t.Fatal("want an error for a selector without =")
	}
}
