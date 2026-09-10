package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPermissionListerFunc_ImplementsPermissionLister(t *testing.T) {
	var l PermissionLister = PermissionListerFunc(
		func(ctx context.Context, tenantID, userID string) ([]string, error) {
			return []string{"notes:read"}, nil
		},
	)
	got, err := l.ListPermissions(context.Background(), "tenant-1", "user-1")
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(got) != 1 || got[0] != "notes:read" {
		t.Errorf("ListPermissions = %v, want [notes:read]", got)
	}
}

func TestPermissionListerFunc_PropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	l := PermissionListerFunc(func(ctx context.Context, tenantID, userID string) ([]string, error) {
		return nil, wantErr
	})
	if _, err := l.ListPermissions(context.Background(), "t", "u"); !errors.Is(err, wantErr) {
		t.Errorf("ListPermissions error = %v, want %v", err, wantErr)
	}
}

func TestMembershipCheckerFunc_ImplementsMembershipChecker(t *testing.T) {
	var c MembershipChecker = MembershipCheckerFunc(
		func(ctx context.Context, tenantID, userID string) (bool, error) {
			return userID == "still-here", nil
		},
	)
	active, err := c.IsActiveMember(context.Background(), "tenant-1", "still-here")
	if err != nil {
		t.Fatalf("IsActiveMember: %v", err)
	}
	if !active {
		t.Error("IsActiveMember(still-here) = false, want true")
	}

	left, err := c.IsActiveMember(context.Background(), "tenant-1", "gone")
	if err != nil {
		t.Fatalf("IsActiveMember: %v", err)
	}
	if left {
		t.Error("IsActiveMember(gone) = true, want false")
	}
}

func TestMembershipCheckerFunc_PropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	c := MembershipCheckerFunc(func(ctx context.Context, tenantID, userID string) (bool, error) {
		return false, wantErr
	})
	if _, err := c.IsActiveMember(context.Background(), "t", "u"); !errors.Is(err, wantErr) {
		t.Errorf("IsActiveMember error = %v, want %v", err, wantErr)
	}
}

func TestSubjectResolverFunc_ImplementsSubjectResolver(t *testing.T) {
	var r SubjectResolver = SubjectResolverFunc(
		func(req *http.Request) (string, bool) {
			userID := req.Header.Get("X-Test-User")
			return userID, userID != ""
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := r.Subject(req); ok {
		t.Fatal("Subject reported ok=true with no header set")
	}

	req.Header.Set("X-Test-User", "u-42")
	userID, ok := r.Subject(req)
	if !ok || userID != "u-42" {
		t.Fatalf("Subject = (%q, %v), want (%q, true)", userID, ok, "u-42")
	}
}
