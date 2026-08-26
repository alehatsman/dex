package ctx

import "testing"

func TestPressure(t *testing.T) {
	// thresholds: normal<60%, compress 60-80%, evict 80-90%, critical≥90%
	// at DefaultWindowSize=128_000: 60%=76800, 80%=102400, 90%=115200
	cases := []struct {
		used int
		want PressureLevel
	}{
		{0, PressureNormal},
		{76_799, PressureNormal},
		{76_800, PressureCompress},
		{102_399, PressureCompress},
		{102_400, PressureEvict},
		{115_199, PressureEvict},
		{115_200, PressureCritical},
		{128_000, PressureCritical},
		{200_000, PressureCritical},
	}
	for _, c := range cases {
		l := Ledger{WindowSize: DefaultWindowSize, UsedTokens: c.used}
		if got := l.Pressure(); got != c.want {
			t.Errorf("used=%d: got %s, want %s", c.used, got, c.want)
		}
	}
}

func TestUtilization(t *testing.T) {
	l := Ledger{WindowSize: 100, UsedTokens: 75}
	if got := l.Utilization(); got != 0.75 {
		t.Fatalf("got %v, want 0.75", got)
	}
	// capped at 1.0
	l.UsedTokens = 200
	if got := l.Utilization(); got != 1.0 {
		t.Fatalf("got %v, want 1.0", got)
	}
}
