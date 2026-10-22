package workspace

import "testing"

func TestNames(t *testing.T) {
	if got := VolumeName("alice"); got != "berth-data-alice" {
		t.Errorf("VolumeName = %q", got)
	}
	if got := ContainerName("alice"); got != "berth-ws-alice" {
		t.Errorf("ContainerName = %q", got)
	}
	if got := NetworkName("alice"); got != "berth-net-alice" {
		t.Errorf("NetworkName = %q", got)
	}
	if ProxyNetwork != "berth-proxy" {
		t.Errorf("ProxyNetwork = %q", ProxyNetwork)
	}
}
