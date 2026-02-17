package driver

import (
	"context"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
)

func TestProbe_ReadyWhenChannelClosed(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil) // uses closedChan()

	resp, err := d.Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	if resp.Ready == nil {
		t.Fatal("expected Ready field to be set")
	}
	if !resp.Ready.Value {
		t.Error("expected Ready=true when channel is closed")
	}
}

func TestProbe_NotReadyWhenChannelOpen(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil) // uses open channel

	resp, err := d.Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}

	if resp.Ready == nil {
		t.Fatal("expected Ready field to be set")
	}
	if resp.Ready.Value {
		t.Error("expected Ready=false when channel is open")
	}
}

func TestGetPluginInfo(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	resp, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo failed: %v", err)
	}

	if resp.Name != DriverName {
		t.Errorf("expected name %q, got %q", DriverName, resp.Name)
	}
	if resp.VendorVersion != "test" {
		t.Errorf("expected version 'test', got %q", resp.VendorVersion)
	}
}

func TestGetPluginCapabilities(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	resp, err := d.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities failed: %v", err)
	}

	if len(resp.Capabilities) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(resp.Capabilities))
	}
}
