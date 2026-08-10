package nodescan

import "testing"

func TestParseCgroup(t *testing.T) {
	cases := []struct {
		name          string
		content       string
		wantPodUID    string
		wantContainer string
	}{
		{
			name:          "cgroupfs v2 single line",
			content:       "0::/kubepods/burstable/pod1234abcd-12ab-34cd-56ef-1234567890ab/deadbeef00000000000000000000000000000000000000000000000000000000",
			wantPodUID:    "1234abcd-12ab-34cd-56ef-1234567890ab",
			wantContainer: "deadbeef00000000000000000000000000000000000000000000000000000000",
		},
		{
			name:          "systemd v2 with underscores and cri-containerd scope",
			content:       "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234abcd_12ab_34cd_56ef_1234567890ab.slice/cri-containerd-abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890.scope",
			wantPodUID:    "1234abcd-12ab-34cd-56ef-1234567890ab",
			wantContainer: "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		},
		{
			name:          "systemd guaranteed qos with crio prefix",
			content:       "0::/kubepods.slice/kubepods-pod0a1b2c3d_4e5f_6789_abcd_ef0123456789.slice/crio-0011223344556677889900112233445566778899001122334455667788990011.scope",
			wantPodUID:    "0a1b2c3d-4e5f-6789-abcd-ef0123456789",
			wantContainer: "0011223344556677889900112233445566778899001122334455667788990011",
		},
		{
			name: "cgroup v1 multi-line, path on each hierarchy",
			content: "12:cpu,cpuacct:/kubepods/besteffort/podAABBCCDD-1122-3344-5566-778899AABBCC/1111111111111111111111111111111111111111111111111111111111111111\n" +
				"11:memory:/kubepods/besteffort/podAABBCCDD-1122-3344-5566-778899AABBCC/1111111111111111111111111111111111111111111111111111111111111111",
			wantPodUID:    "AABBCCDD-1122-3344-5566-778899AABBCC",
			wantContainer: "1111111111111111111111111111111111111111111111111111111111111111",
		},
		{
			name:          "host process outside kubepods",
			content:       "0::/system.slice/kubelet.service",
			wantPodUID:    "",
			wantContainer: "",
		},
		{
			name:          "empty",
			content:       "",
			wantPodUID:    "",
			wantContainer: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := parseCgroup(tc.content)
			if b.PodUID != tc.wantPodUID {
				t.Errorf("pod uid = %q, want %q", b.PodUID, tc.wantPodUID)
			}
			if b.ContainerID != tc.wantContainer {
				t.Errorf("container id = %q, want %q", b.ContainerID, tc.wantContainer)
			}
		})
	}
}
