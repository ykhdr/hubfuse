package main

import (
	"reflect"
	"testing"
)

func TestSplitDeviceArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"space separated", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"comma separated", []string{"a,b,c"}, []string{"a", "b", "c"}},
		{"mixed", []string{"a,b", "c"}, []string{"a", "b", "c"}},
		{"trims whitespace", []string{" a , b ", "c "}, []string{"a", "b", "c"}},
		{"drops empties", []string{"a,,b", ",", "c"}, []string{"a", "b", "c"}},
		{"all empty yields nil", []string{",", " "}, nil},
		{"single all token", []string{"all"}, []string{"all"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitDeviceArgs(tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitDeviceArgs(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
