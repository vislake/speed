package sharing

import (
	"testing"
	"time"
)

func TestShare_TableName(t *testing.T) {
	if got := (Share{}).TableName(); got != tableShares {
		t.Errorf("TableName() = %q, want %q", got, tableShares)
	}
}

func TestAccessLogEntry_TableName(t *testing.T) {
	if got := (AccessLogEntry{}).TableName(); got != tableAccessLog {
		t.Errorf("TableName() = %q, want %q", got, tableAccessLog)
	}
}

func TestShare_IsLive(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	one := 1

	tests := []struct {
		name  string
		share Share
		want  bool
	}{
		{
			name:  "live, no ceiling",
			share: Share{ExpiresAt: &future},
			want:  true,
		},
		{
			name:  "revoked",
			share: Share{ExpiresAt: &future, RevokedAt: &now},
			want:  false,
		},
		{
			name:  "expired",
			share: Share{ExpiresAt: &past},
			want:  false,
		},
		{
			name:  "expires exactly now is not live",
			share: Share{ExpiresAt: &now},
			want:  false,
		},
		{
			name:  "nil ExpiresAt is never live",
			share: Share{ExpiresAt: nil},
			want:  false,
		},
		{
			name:  "under the view ceiling",
			share: Share{ExpiresAt: &future, MaxViews: &one, ViewCount: 0},
			want:  true,
		},
		{
			name:  "view ceiling reached",
			share: Share{ExpiresAt: &future, MaxViews: &one, ViewCount: 1},
			want:  false,
		},
		{
			name:  "view ceiling exceeded",
			share: Share{ExpiresAt: &future, MaxViews: &one, ViewCount: 2},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.share.isLive(now); got != tt.want {
				t.Errorf("isLive(%v) = %v, want %v", now, got, tt.want)
			}
		})
	}
}
