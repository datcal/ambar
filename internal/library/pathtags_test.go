package library

import (
	"reflect"
	"testing"
)

func TestNormalizeSegment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Objects", "objects"},
		{"ordering prefix space", "2 Objects", "objects"},
		{"ordering prefix underscore", "4_Stone", "stone"},
		{"ordering prefix dot", "3.Weapons", "weapons"},
		{"ordering prefix hyphen", "1-tiles", "tiles"},
		{"ampersand", "Parts & Pieces", "parts-pieces"},
		{"spaces collapse", "Café  Ambiance", "café-ambiance"},
		{"2d kept", "2D", "2d"},
		{"3ds kept", "3ds", "3ds"},
		{"pure number dropped", "128", ""},
		{"lone ordering number dropped", "2", ""},
		{"format folder dropped", "PNG", ""},
		{"format folder prefix dropped", "PNG_Animations", ""},
		{"tiled dropped", "Tiled_files", ""},
		{"empty", "", ""},
		{"punctuation only", "___", ""},
		{"trailing separators trimmed", "Rocks!!!", "rocks"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeSegment(tc.in); got != tc.want {
				t.Errorf("NormalizeSegment(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPathTagSegments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"nested", "Environment/Rocks/idle.png", []string{"environment", "rocks"}},
		{"with format folder", "PNG/Plant1/idle.png", []string{"plant1"}},
		{"ordering prefixes", "2 Objects/4 Stone/wall.png", []string{"objects", "stone"}},
		{"filename only", "sprite.png", nil},
		{"dedupe", "Rocks/rocks/a.png", []string{"rocks"}},
		{"all noise", "PNG/128/x.png", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PathTagSegments(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PathTagSegments(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
