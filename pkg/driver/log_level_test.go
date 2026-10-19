package driver

import (
	"testing"

	"k8s.io/klog/v2"
)

// Health and capability calls arrive several times a second; at the chart's
// default level (4) they would hide real events in the log.
func TestCallLogLevel(t *testing.T) {
	tests := []struct {
		method string
		want   klog.Level
	}{
		{"/csi.v1.Identity/Probe", 6},
		{"/csi.v1.Identity/GetPluginInfo", 6},
		{"/csi.v1.Identity/GetPluginCapabilities", 6},
		{"/csi.v1.Controller/ControllerGetCapabilities", 6},
		{"/csi.v1.Node/NodeGetCapabilities", 6},
		{"/csi.v1.Node/NodeGetInfo", 6},
		{"/csi.v1.Controller/CreateVolume", 4},
		{"/csi.v1.Node/NodePublishVolume", 4},
	}
	for _, tt := range tests {
		if got := callLogLevel(tt.method); got != tt.want {
			t.Errorf("callLogLevel(%q) = %d, want %d", tt.method, got, tt.want)
		}
	}
}
