package allowlist

import (
	"reflect"
	"strings"
	"testing"
)

func TestMisordered(t *testing.T) {
	values := map[string]int32{
		"runner-standard":    100000,
		"runner-high":        2000000,
		"gag-control-plane":  1000000000,
		"infra-low":          1000000,
		"cluster-default":    5000000,
		"system-very-urgent": 2000000000,
	}
	tests := []struct {
		name          string
		globalDefault string
		worker        []string
		controlPlane  []string
		want          []Inversion
	}{
		{
			name:         "worker below every control-plane class",
			worker:       []string{"runner-standard", "runner-high"},
			controlPlane: []string{"gag-control-plane"},
		},
		{
			name:         "lowest control-plane class sets the floor",
			worker:       []string{"runner-standard", "runner-high"},
			controlPlane: []string{"gag-control-plane", "infra-low"},
			want: []Inversion{{Worker: "runner-high", WorkerValue: 2000000,
				ControlPlane: "infra-low", ControlPlaneValue: 1000000}},
		},
		{
			name:         "equal value is an inversion",
			worker:       []string{"infra-low"},
			controlPlane: []string{"gag-control-plane", "infra-low"},
			want: []Inversion{{Worker: "infra-low", WorkerValue: 1000000,
				ControlPlane: "infra-low", ControlPlaneValue: 1000000}},
		},
		{
			name:          "globalDefault reaches every worker",
			globalDefault: "cluster-default",
			controlPlane:  []string{"infra-low"},
			want: []Inversion{{Worker: "cluster-default", WorkerValue: 5000000,
				ControlPlane: "infra-low", ControlPlaneValue: 1000000}},
		},
		{
			name:         "absent worker class is skipped",
			worker:       []string{"not-created-yet"},
			controlPlane: []string{"infra-low"},
		},
		{
			name:         "no existing control-plane class checks nothing",
			worker:       []string{"system-very-urgent"},
			controlPlane: []string{"not-created-yet"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Misordered(ClassValues{Values: values, GlobalDefault: tt.globalDefault}, tt.worker, tt.controlPlane)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Misordered() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMisorderedError(t *testing.T) {
	if err := MisorderedError(nil); err != nil {
		t.Errorf("MisorderedError(nil) = %v, want nil", err)
	}
	err := MisorderedError([]Inversion{{Worker: "runner-high", WorkerValue: 2, ControlPlane: "infra-low", ControlPlaneValue: 1}})
	if err == nil || !strings.Contains(err.Error(), `worker class "runner-high" (2) is not below control-plane class "infra-low" (1)`) {
		t.Errorf("MisorderedError() = %v, want it to name both classes and values", err)
	}
}
