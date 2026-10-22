package ibmcloud

import "testing"

func TestDP2IOPSRange(t *testing.T) {
	tests := []struct {
		sizeGB   int64
		min, max int64
	}{
		{10, 100, 1000},
		{39, 100, 1000},
		{40, 100, 2000},
		{79, 100, 2000},
		{80, 100, 4000},
		{99, 100, 4000},
		{100, 100, 6000},
		{499, 100, 6000},
		{500, 100, 10000},
		{999, 100, 10000},
		{1000, 100, 20000},
		{1999, 100, 20000},
		{2000, 200, 40000},
		{3999, 200, 40000},
		{4000, 300, 40000},
		{7999, 300, 40000},
		{8000, 500, 64000},
		{15999, 500, 64000},
		{16000, 2000, 96000},
		{32000, 2000, 96000},
	}
	for _, tt := range tests {
		min, max, ok := ShareIOPSRange("dp2", tt.sizeGB)
		if !ok || min != tt.min || max != tt.max {
			t.Errorf("ShareIOPSRange(dp2, %d) = %d, %d, %v; want %d, %d, true", tt.sizeGB, min, max, ok, tt.min, tt.max)
		}
	}
}

func TestShareIOPSRange_Unknown(t *testing.T) {
	for _, tt := range []struct {
		profile string
		sizeGB  int64
	}{
		{"dp2", 9},
		{"dp2", 32001},
		{"rfs", 100},
		{"", 100},
	} {
		if _, _, ok := ShareIOPSRange(tt.profile, tt.sizeGB); ok {
			t.Errorf("ShareIOPSRange(%q, %d) reported a range; want none", tt.profile, tt.sizeGB)
		}
	}
}

func TestValidateShareIOPS(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		sizeGB  int64
		iops    int64
		wantErr bool
	}{
		{"3000 IOPS on 10 GB is refused", "dp2", 10, 3000, true},
		{"3000 IOPS on 79 GB is refused", "dp2", 79, 3000, true},
		{"3000 IOPS on 80 GB is allowed", "dp2", 80, 3000, false},
		{"100 IOPS on 10 GB is allowed", "dp2", 10, 100, false},
		{"6001 IOPS on 499 GB is refused", "dp2", 499, 6001, true},
		{"100 IOPS on 2000 GB is refused (minimum 200)", "dp2", 2000, 100, true},
		{"2000 IOPS on 16000 GB is allowed", "dp2", 16000, 2000, false},
		{"another profile is not checked", "rfs", 10, 3000, false},
	}
	for _, tt := range tests {
		err := ValidateShareIOPS(tt.profile, tt.sizeGB, tt.iops)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}
