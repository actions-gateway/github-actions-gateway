package cgroup

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocate(t *testing.T) {
	const (
		kindRoot = "kubelet.slice/kubelet-kubepods.slice"
		kind     = "/docker/c4418cb3a46760163d03c4e46b92727c5dd6c8a99d2a85d45fe7130503ad4142/" + kindRoot
		kindPod  = kind + "/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-pod1b4e28ba_2fa1_11d2_883f_0016d3cca427.slice"
		gkePod   = "/kubepods.slice/kubepods-pod6fa459ea_ee8a_3ca4_894e_db77e160355e.slice"
		fsPod    = "/kubepods/besteffort/pod1b4e28ba-2fa1-11d2-883f-0016d3cca427"
	)
	scope := "/cri-containerd-" + runnerID + ".scope"
	for name, tc := range map[string]struct {
		root, path string
		want       Place
		ok         bool
	}{
		"kind's container":              {kindRoot, kindPod + scope, Place{InPod, burstableUID, runnerID, true}, true},
		"below kind's container":        {kindRoot, kindPod + scope + "/nested", Place{InPod, burstableUID, runnerID, false}, true},
		"beside the containers":         {kindRoot, kindPod + "/nested", Place{Level: InPod, Pod: burstableUID}, true},
		"kind's pod":                    {kindRoot, kindPod, Place{Level: PodLevel, Pod: burstableUID}, true},
		"kind's QoS slice":              {kindRoot, kind + "/kubelet-kubepods-burstable.slice", Place{Level: QoSLevel}, true},
		"kind's pods' root":             {kindRoot, kind, Place{Level: RootLevel}, true},
		"above kind's pods":             {kindRoot, "/docker/c4418cb3a46760163d03c4e46b92727c5dd6c8a99d2a85d45fe7130503ad4142/kubelet.slice", Place{Level: Above}, true},
		"a Guaranteed pod on GKE":       {"kubepods.slice", gkePod + scope, Place{InPod, guaranteedUID, runnerID, true}, true},
		"cgroupfs":                      {"kubepods", fsPod + "/" + runnerID, Place{InPod, burstableUID, runnerID, true}, true},
		"cgroupfs with a systemd name":  {"kubepods", fsPod + scope, Place{Level: InPod, Pod: burstableUID}, true},
		"a pods' root named in the pod": {kindRoot, kindPod + scope + "/" + kindRoot, Place{InPod, burstableUID, runnerID, false}, true},
		"a static pod's UID":            {"kubepods", "/kubepods/pod9c1185a5c5e9fc54612808977ee8f548", Place{}, false},
		"a directory no pod's":          {kindRoot, kind + "/kubelet-kubepods-burstable.slice/other.slice", Place{}, false},
		"no leading slash":              {kindRoot, kindPod[1:], Place{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := Locate(tc.root, tc.path)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
