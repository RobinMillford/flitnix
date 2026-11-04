package main

import (
	"math"
	"testing"
)

func TestMedian_Conventional(t *testing.T) {
	tests := []struct {
		name      string
		input     []float64
		expected  float64
		expectErr bool
	}{
		{
			name:      "Empty input",
			input:     []float64{},
			expected:  0.0,
			expectErr: true,
		},
		{
			name:      "Nil input",
			input:     nil,
			expected:  0.0,
			expectErr: true,
		},
		{
			name:      "One value",
			input:     []float64{42.5},
			expected:  42.5,
			expectErr: false,
		},
		{
			name:      "Odd sample count (already sorted)",
			input:     []float64{10.0, 20.0, 30.0, 40.0, 50.0},
			expected:  30.0,
			expectErr: false,
		},
		{
			name:      "Odd sample count (unsorted)",
			input:     []float64{50.0, 10.0, 40.0, 20.0, 30.0},
			expected:  30.0,
			expectErr: false,
		},
		{
			name:      "Even sample count (already sorted)",
			input:     []float64{10.0, 20.0, 30.0, 40.0},
			expected:  25.0, // (20 + 30) / 2
			expectErr: false,
		},
		{
			name:      "Even sample count (unsorted)",
			input:     []float64{40.0, 10.0, 30.0, 20.0},
			expected:  25.0,
			expectErr: false,
		},
		{
			name:      "Duplicate observations (odd)",
			input:     []float64{5.0, 5.0, 5.0, 5.0, 5.0},
			expected:  5.0,
			expectErr: false,
		},
		{
			name:      "Duplicate observations (even)",
			input:     []float64{5.0, 5.0, 10.0, 10.0},
			expected:  7.5, // (5 + 10) / 2
			expectErr: false,
		},
		{
			name:      "Ten observations (even - 5GB Config 1 authoritative)",
			input:     []float64{642.87, 840.06, 936.28, 989.18, 930.46, 937.97, 967.14, 984.52, 867.80, 957.05},
			expected:  937.125, // (936.28 + 937.97) / 2
			expectErr: false,
		},
		{
			name:      "Ten observations (even - 5GB Config 4 authoritative)",
			input:     []float64{899.85, 933.49, 929.20, 837.37, 944.22, 965.84, 937.05, 967.10, 955.22, 892.66},
			expected:  935.27, // (933.49 + 937.05) / 2
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := median(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error for empty sample %q, got nil with result %v", tc.name, got)
				}
				if err != ErrEmptySample {
					t.Fatalf("expected ErrEmptySample, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.name, err)
			}
			if math.Abs(got-tc.expected) > 1e-4 {
				t.Errorf("median(%v) = %v, expected %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestUpperMiddleHistorical(t *testing.T) {
	// Upper-middle selects cpy[len/2], which for n=10 is index 5
	c1_5gb := []float64{642.87, 840.06, 936.28, 989.18, 930.46, 937.97, 967.14, 984.52, 867.80, 957.05}
	got, err := upperMiddleHistorical(c1_5gb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := 937.97 // sorted index 5
	if math.Abs(got-expected) > 1e-4 {
		t.Errorf("upperMiddleHistorical() = %v, expected %v", got, expected)
	}

	// For empty, must return ErrEmptySample, never silently report zero
	if _, err := upperMiddleHistorical(nil); err == nil {
		t.Errorf("expected error for nil, got nil")
	}
	if _, err := upperMiddleHistorical([]float64{}); err == nil {
		t.Errorf("expected error for empty slice, got nil")
	}
}
