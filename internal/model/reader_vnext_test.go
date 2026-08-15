package model

import "testing"

func TestReaderThoughtVersionKeyOrdersClockDeviceAndOperation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		left  ReaderThoughtVersionKey
		right ReaderThoughtVersionKey
		want  int
	}{
		{
			name:  "higher logical clock wins",
			left:  ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-a", OpID: "op-a"},
			right: ReaderThoughtVersionKey{LogicalClock: 1, DeviceID: "device-z", OpID: "op-z"},
			want:  1,
		},
		{
			name:  "device breaks equal clock",
			left:  ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-b", OpID: "op-a"},
			right: ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-a", OpID: "op-z"},
			want:  1,
		},
		{
			name:  "operation id breaks equal device",
			left:  ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-a", OpID: "op-b"},
			right: ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-a", OpID: "op-a"},
			want:  1,
		},
		{
			name:  "device uses UTF-8 bytes instead of UTF-16 code units",
			left:  ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-\U00010000", OpID: "op-a"},
			right: ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-\ue000", OpID: "op-z"},
			want:  1,
		},
		{
			name:  "same key compares equal regardless of operation kind",
			left:  ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-a", OpID: "op-a"},
			right: ReaderThoughtVersionKey{LogicalClock: 2, DeviceID: "device-a", OpID: "op-a"},
			want:  0,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.left.Compare(tc.right); got != tc.want {
				t.Fatalf("Compare() = %d, want %d", got, tc.want)
			}
		})
	}
}
